// Package wechatkf implements the Channel interface for WeChat Customer Service (KF).
// It polls the KF sync message API to receive messages from customers.
package wechatkf

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"gitee.com/kobeyoung81/nanogateway/internal/channel"
	"gitee.com/kobeyoung81/nanogateway/internal/config"
	"gitee.com/kobeyoung81/nanogateway/internal/gateway"
	"gitee.com/kobeyoung81/nanoprotocol"
)

// Adapter implements the Channel interface for WeChat KF.
type Adapter struct {
	corpID   string
	kfSecret string
	openKfID string
	busURL   string

	callbackToken  string
	encodingAESKey string
	callbackAddr   string
	aesKey         []byte // derived from encodingAESKey

	handler channel.InboundHandler
	state   syncStateClient

	// Access token management
	token      string
	tokenExpAt time.Time
	mu         sync.Mutex

	notifyCh chan struct{} // buffered(1), callback signals polling loop
	server   *http.Server
}

type syncStateClient interface {
	GetWeChatKFSyncState(openKfID string) (*nanoprotocol.WeChatKFSyncStateResponse, error)
	UpdateWeChatKFCursor(openKfID, cursor string) error
	UpdateWeChatKFCallbackToken(openKfID, callbackToken string, tokenExpiresAt time.Time) error
	Close() error
}

// New creates a new WeChat KF adapter.
func New(corpID, kfSecret, openKfID, callbackToken, encodingAESKey, callbackAddr, busURL string) (*Adapter, error) {
	if corpID == "" {
		return nil, fmt.Errorf("wechatkf: corpID is required")
	}
	if kfSecret == "" {
		return nil, fmt.Errorf("wechatkf: kfSecret is required")
	}
	if openKfID == "" {
		return nil, fmt.Errorf("wechatkf: openKfID is required")
	}
	if callbackToken == "" {
		return nil, fmt.Errorf("wechatkf: token is required")
	}
	if encodingAESKey == "" {
		return nil, fmt.Errorf("wechatkf: encodingAESKey is required")
	}

	// Derive AES key: base64 decode encodingAESKey + "="
	aesKey, err := base64.StdEncoding.DecodeString(encodingAESKey + "=")
	if err != nil {
		return nil, fmt.Errorf("wechatkf: invalid encodingAESKey: %w", err)
	}
	if len(aesKey) != 32 {
		return nil, fmt.Errorf("wechatkf: encodingAESKey must decode to 32 bytes, got %d", len(aesKey))
	}

	if callbackAddr == "" {
		callbackAddr = ":8082"
	}

	return &Adapter{
		corpID:         corpID,
		kfSecret:       kfSecret,
		openKfID:       openKfID,
		busURL:         busURL,
		callbackToken:  callbackToken,
		encodingAESKey: encodingAESKey,
		callbackAddr:   callbackAddr,
		aesKey:         aesKey,
		notifyCh:       make(chan struct{}, 1),
	}, nil
}

func (a *Adapter) Name() string { return "wechatkf" }

func (a *Adapter) Start(ctx context.Context, handler channel.InboundHandler) error {
	a.handler = handler

	// Fetch initial access token
	if err := a.refreshToken(ctx); err != nil {
		return fmt.Errorf("wechatkf: initial token fetch: %w", err)
	}

	if a.state == nil {
		if a.busURL == "" {
			return fmt.Errorf("wechatkf: busURL is required")
		}
		client, err := nanoprotocol.NewClient(a.busURL, nanoprotocol.WithToken(config.InternalToken()))
		if err != nil {
			return fmt.Errorf("wechatkf: connect to bus: %w", err)
		}
		a.state = client
	}

	if state, err := a.state.GetWeChatKFSyncState(a.openKfID); err != nil {
		return fmt.Errorf("wechatkf: load sync state: %w", err)
	} else if state != nil {
		slog.Info("wechatkf: loaded sync state", "open_kf_id", a.openKfID, "cursor", state.Cursor)
	}

	// Start callback HTTP server
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", a.handleCallback)
	a.server = &http.Server{Addr: a.callbackAddr, Handler: mux}

	slog.Info("wechatkf: channel started", "open_kf_id", a.openKfID, "callback_addr", a.callbackAddr)

	errCh := make(chan error, 1)
	go func() {
		if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	// 30s fallback polling
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errCh:
			return err
		case <-a.notifyCh:
			a.pollMessages(ctx)
		case <-ticker.C:
			a.pollMessages(ctx)
		}
	}
}

func (a *Adapter) Send(ctx context.Context, msg channel.OutboundMessage) error {
	if msg.ChatID == "" {
		return fmt.Errorf("wechatkf: external user ID (chat ID) is empty")
	}

	for _, item := range msg.Media {
		// wechat KF only supports media sent via wecom API, so we filter by platform = wecom
		if item.Platform != "wecom" {
			continue
		}
		if err := a.sendKFMediaMessage(ctx, msg.ChatID, item); err != nil {
			slog.Warn("wechatkf: send media failed", "media_id", item.MediaID, "error", err)
		}
	}

	if msg.Content != "" {
		// WeChat KF is a plain-text channel — strip markdown so recipients get
		// readable text instead of raw formatting characters.
		return a.sendKFTextMessage(ctx, msg.ChatID, gateway.MarkdownToText(msg.Content))
	}
	return nil
}

func (a *Adapter) Stop() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.server != nil {
		a.server.Shutdown(context.Background())
		a.server = nil
	}
	if a.state != nil {
		a.state.Close()
		a.state = nil
	}
	slog.Info("wechatkf: channel stopped")
	return nil
}

// Access token management

type tokenResponse struct {
	ErrCode     int    `json:"errcode"`
	ErrMsg      string `json:"errmsg"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

func (a *Adapter) refreshToken(ctx context.Context) error {
	url := fmt.Sprintf(
		"https://qyapi.weixin.qq.com/cgi-bin/gettoken?corpid=%s&corpsecret=%s",
		a.corpID, a.kfSecret,
	)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	var tokenResp tokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return fmt.Errorf("unmarshal response: %w", err)
	}

	if tokenResp.ErrCode != 0 {
		return fmt.Errorf("token error: %d %s", tokenResp.ErrCode, tokenResp.ErrMsg)
	}

	a.mu.Lock()
	a.token = tokenResp.AccessToken
	a.tokenExpAt = time.Now().Add(time.Duration(tokenResp.ExpiresIn-300) * time.Second) // refresh 5min early
	a.mu.Unlock()

	return nil
}

func (a *Adapter) getToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	token := a.token
	expAt := a.tokenExpAt
	a.mu.Unlock()

	if token != "" && time.Now().Before(expAt) {
		return token, nil
	}

	if err := a.refreshToken(ctx); err != nil {
		return "", err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	return a.token, nil
}

// pollMessages calls the KF sync message API and processes new messages.
func (a *Adapter) pollMessages(ctx context.Context) {
	state, err := a.state.GetWeChatKFSyncState(a.openKfID)
	if err != nil {
		slog.Warn("wechatkf: load sync state failed", "error", err)
		return
	}

	cursor := ""
	callbackToken := ""
	if state != nil {
		cursor = state.Cursor
		if state.CallbackToken != "" && !state.TokenExpiresAt.IsZero() && time.Now().Before(state.TokenExpiresAt) {
			callbackToken = state.CallbackToken
		}
	}

	for pages := 0; ; pages++ {
		messages, nextCursor, hasMore, err := a.syncMessages(ctx, cursor, callbackToken)
		if err != nil {
			slog.Warn("wechatkf: sync messages failed", "error", err)
			return
		}

		if err := a.processMessages(ctx, messages); err != nil {
			slog.Warn("wechatkf: processing messages failed; cursor not advanced", "error", err)
			return
		}

		if nextCursor != "" && nextCursor != cursor {
			if err := a.state.UpdateWeChatKFCursor(a.openKfID, nextCursor); err != nil {
				slog.Warn("wechatkf: persist cursor failed", "cursor", nextCursor, "error", err)
				return
			}
			slog.Debug("wechatkf: cursor advanced", "open_kf_id", a.openKfID, "cursor", nextCursor)
			cursor = nextCursor
		} else if hasMore != 0 {
			slog.Warn("wechatkf: next_cursor did not advance while has_more=1", "open_kf_id", a.openKfID, "cursor", cursor)
			return
		}

		if hasMore == 0 {
			return
		}
		if pages >= 19 {
			slog.Warn("wechatkf: reached max pages per poll", "open_kf_id", a.openKfID, "pages", pages+1)
			return
		}
	}
}

// KF API types

type kfSyncRequest struct {
	Cursor   string `json:"cursor,omitempty"`
	Token    string `json:"token,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	OpenKfID string `json:"open_kfid,omitempty"`
}

type kfSyncResponse struct {
	ErrCode    int         `json:"errcode"`
	ErrMsg     string      `json:"errmsg"`
	NextCursor string      `json:"next_cursor"`
	HasMore    int         `json:"has_more"`
	MsgList    []kfMessage `json:"msg_list"`
}

type kfMessage struct {
	MsgID          string        `json:"msgid"`
	OpenKfID       string        `json:"open_kfid"`
	ExternalUserID string        `json:"external_userid"`
	SendTime       int64         `json:"send_time"`
	Origin         int           `json:"origin"` // 3=customer, 4=system, 5=servicer
	MsgType        string        `json:"msgtype"`
	Text           kfTextContent `json:"text,omitempty"`
	Image          kfMediaRef    `json:"image,omitempty"`
	File           kfMediaRef    `json:"file,omitempty"`
	Voice          kfMediaRef    `json:"voice,omitempty"`
	Video          kfMediaRef    `json:"video,omitempty"`
}

type kfTextContent struct {
	Content string `json:"content"`
}

type kfMediaRef struct {
	MediaID string `json:"media_id"`
}

type kfSendRequest struct {
	ToUser   string      `json:"touser"`
	OpenKfID string      `json:"open_kfid"`
	MsgType  string      `json:"msgtype"`
	Text     *kfSendText `json:"text,omitempty"`
	Image    *kfMediaRef `json:"image,omitempty"`
	Voice    *kfMediaRef `json:"voice,omitempty"`
	Video    *kfMediaRef `json:"video,omitempty"`
	File     *kfMediaRef `json:"file,omitempty"`
}

type kfSendText struct {
	Content string `json:"content"`
}

type kfSendResponse struct {
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
	MsgID   string `json:"msgid"`
}

// syncMessages calls the WeCom KF sync message API.
func (a *Adapter) syncMessages(ctx context.Context, cursor, callbackToken string) ([]kfMessage, string, int, error) {
	token, err := a.getToken(ctx)
	if err != nil {
		return nil, "", 0, fmt.Errorf("get access token: %w", err)
	}

	reqBody := kfSyncRequest{
		Cursor:   cursor,
		Token:    callbackToken,
		Limit:    100,
		OpenKfID: a.openKfID,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, "", 0, fmt.Errorf("marshal request: %w", err)
	}

	url := fmt.Sprintf("https://qyapi.weixin.qq.com/cgi-bin/kf/sync_msg?access_token=%s", token)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, "", 0, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", 0, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", 0, fmt.Errorf("read response: %w", err)
	}

	var syncResp kfSyncResponse
	if err := json.Unmarshal(respBody, &syncResp); err != nil {
		return nil, "", 0, fmt.Errorf("unmarshal response: %w", err)
	}

	if syncResp.ErrCode != 0 {
		return nil, "", 0, fmt.Errorf("api error: %d %s", syncResp.ErrCode, syncResp.ErrMsg)
	}

	// Filter to only customer messages (origin=3)
	var customerMsgs []kfMessage
	for _, msg := range syncResp.MsgList {
		if msg.Origin == 3 {
			customerMsgs = append(customerMsgs, msg)
		}
	}

	return customerMsgs, syncResp.NextCursor, syncResp.HasMore, nil
}

func (a *Adapter) processMessages(ctx context.Context, messages []kfMessage) error {
	for _, msg := range messages {
		var content string
		var media []nanoprotocol.MediaItem

		switch msg.MsgType {
		case "text":
			content = msg.Text.Content
		case "image":
			media = append(media, nanoprotocol.MediaItem{
				Platform: "wechatkf",
				MediaID:  msg.Image.MediaID,
				MimeType: "image/jpeg",
			})
		case "file":
			media = append(media, nanoprotocol.MediaItem{
				Platform: "wechatkf",
				MediaID:  msg.File.MediaID,
				MimeType: "application/octet-stream",
			})
		case "voice":
			media = append(media, nanoprotocol.MediaItem{
				Platform: "wechatkf",
				MediaID:  msg.Voice.MediaID,
				MimeType: "audio/amr",
			})
		case "video":
			media = append(media, nanoprotocol.MediaItem{
				Platform: "wechatkf",
				MediaID:  msg.Video.MediaID,
				MimeType: "video/mp4",
			})
		default:
			slog.Debug("wechatkf: ignoring unsupported message type", "type", msg.MsgType, "from", msg.ExternalUserID)
			continue
		}

		if content == "" && len(media) == 0 {
			continue
		}

		if a.handler != nil {
			if err := a.handler(ctx, channel.InboundMessage{
				Channel:   "wechatkf",
				SenderID:  msg.ExternalUserID,
				ChatID:    msg.ExternalUserID,
				MessageID: msg.MsgID,
				Content:   content,
				Media:     media,
				Metadata: map[string]any{
					"msg_id":     msg.MsgID,
					"open_kf_id": msg.OpenKfID,
				},
			}); err != nil {
				return fmt.Errorf("handle message %s: %w", msg.MsgID, err)
			}
		}
	}
	return nil
}

// sendKFTextMessage sends a text message via the KF send message API.
func (a *Adapter) sendKFTextMessage(ctx context.Context, externalUserID, content string) error {
	return a.sendKFMessage(ctx, kfSendRequest{
		ToUser:   externalUserID,
		OpenKfID: a.openKfID,
		MsgType:  "text",
		Text:     &kfSendText{Content: content},
	})
}

// sendKFMediaMessage sends a media message (image, voice, video, or file) via the KF send message API.
func (a *Adapter) sendKFMediaMessage(ctx context.Context, externalUserID string, item nanoprotocol.MediaItem) error {
	media := &kfMediaRef{MediaID: item.MediaID}
	req := kfSendRequest{ToUser: externalUserID, OpenKfID: a.openKfID}
	switch {
	case strings.HasPrefix(item.MimeType, "image/"):
		req.MsgType, req.Image = "image", media
	case strings.HasPrefix(item.MimeType, "audio/"):
		req.MsgType, req.Voice = "voice", media
	case strings.HasPrefix(item.MimeType, "video/"):
		req.MsgType, req.Video = "video", media
	default:
		req.MsgType, req.File = "file", media
	}
	return a.sendKFMessage(ctx, req)
}

// sendKFMessage sends a message via the KF send message API.
func (a *Adapter) sendKFMessage(ctx context.Context, reqBody kfSendRequest) error {
	token, err := a.getToken(ctx)
	if err != nil {
		return fmt.Errorf("get access token: %w", err)
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	slog.Info("wechatkf: sending message", "to", reqBody.ToUser, "msg_type", reqBody.MsgType, "request_body", string(body))

	url := fmt.Sprintf("https://qyapi.weixin.qq.com/cgi-bin/kf/send_msg?access_token=%s", token)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	slog.Info("wechatkf: send response", "http_status", resp.StatusCode, "response_body", string(respBody))

	var sendResp kfSendResponse
	if err := json.Unmarshal(respBody, &sendResp); err != nil {
		return fmt.Errorf("unmarshal response: %w", err)
	}

	if sendResp.ErrCode != 0 {
		return fmt.Errorf("send error: %d %s", sendResp.ErrCode, sendResp.ErrMsg)
	}

	slog.Debug("wechatkf: message sent", "to", reqBody.ToUser, "msg_type", reqBody.MsgType, "msg_id", sendResp.MsgID)
	return nil
}
