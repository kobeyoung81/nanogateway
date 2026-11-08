// Package wecom implements the Channel interface for WeCom (Enterprise WeChat).
// It starts an HTTP callback server to receive messages from WeCom.
package wecom

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"

	"github.com/xen0n/go-workwx/v2"

	"gitee.com/kobeyoung81/nanogateway/internal/channel"
	"gitee.com/kobeyoung81/nanoprotocol"
)

// Adapter implements the Channel interface for WeCom.
type Adapter struct {
	corpID         string
	corpSecret     string
	agentID        int64
	token          string
	encodingAESKey string
	callbackAddr   string

	app     *workwx.WorkwxApp
	server  *http.Server
	handler channel.InboundHandler

	mu sync.Mutex
}

// New creates a new WeCom adapter.
func New(corpID, corpSecret string, agentID int64, token, encodingAESKey, callbackAddr string) (*Adapter, error) {
	if corpID == "" {
		return nil, fmt.Errorf("wecom: corpID is required")
	}
	if corpSecret == "" {
		return nil, fmt.Errorf("wecom: corpSecret is required")
	}
	if agentID <= 0 {
		return nil, fmt.Errorf("wecom: agentID must be positive")
	}
	if token == "" {
		return nil, fmt.Errorf("wecom: token is required for callback verification")
	}
	if encodingAESKey == "" {
		return nil, fmt.Errorf("wecom: encodingAESKey is required for callback verification")
	}
	if callbackAddr == "" {
		callbackAddr = ":8081"
	}

	client := workwx.New(corpID)
	app := client.WithApp(corpSecret, agentID)

	return &Adapter{
		corpID:         corpID,
		corpSecret:     corpSecret,
		agentID:        agentID,
		token:          token,
		encodingAESKey: encodingAESKey,
		callbackAddr:   callbackAddr,
		app:            app,
	}, nil
}

func (a *Adapter) Name() string { return "wecom" }

func (a *Adapter) Start(ctx context.Context, handler channel.InboundHandler) error {
	a.handler = handler

	// Start the background access token refresher. WeCom tokens expire every
	// 2 hours; this goroutine renews the token proactively and stops when ctx
	// is cancelled.
	a.app.SpawnAccessTokenRefresherWithContext(ctx)

	// Create the HTTP handler for WeCom callback verification and message receiving.
	rxHandler, err := workwx.NewHTTPHandler(a.token, a.encodingAESKey, a)
	if err != nil {
		return fmt.Errorf("wecom: create http handler: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/callback", rxHandler)

	a.mu.Lock()
	a.server = &http.Server{
		Addr:    a.callbackAddr,
		Handler: mux,
	}
	server := a.server
	a.mu.Unlock()

	slog.Info("wecom: channel started", "addr", a.callbackAddr)

	// Start server in a goroutine, and block on context cancellation.
	errCh := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("wecom: http server: %w", err)
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

func (a *Adapter) Send(_ context.Context, msg channel.OutboundMessage) error {
	if msg.ChatID == "" {
		return fmt.Errorf("wecom: chat ID (user ID) is empty")
	}

	recipient := &workwx.Recipient{
		UserIDs: []string{msg.ChatID},
	}

	// Send media items if present
	for _, item := range msg.Media {
		if item.Platform != "wecom" {
			continue
		}
		if err := a.sendMediaMessage(recipient, item); err != nil {
			slog.Warn("wecom: send media failed", "media_id", item.MediaID, "error", err)
		}
	}

	// Send text/markdown content
	if msg.Content != "" {
		if err := a.app.SendMarkdownMessage(recipient, msg.Content, false); err != nil {
			return fmt.Errorf("wecom: send message: %w", err)
		}
	}

	slog.Debug("wecom: message sent", "chat_id", msg.ChatID)
	return nil
}

// sendMediaMessage sends a media item using the appropriate WeCom API.
func (a *Adapter) sendMediaMessage(recipient *workwx.Recipient, item nanoprotocol.MediaItem) error {
	switch {
	case item.MimeType == "" || isImageMime(item.MimeType):
		return a.app.SendImageMessage(recipient, item.MediaID, false)
	default:
		return a.app.SendFileMessage(recipient, item.MediaID, false)
	}
}

func isImageMime(mime string) bool {
	return mime == "image/jpeg" || mime == "image/png" || mime == "image/gif"
}

func (a *Adapter) Stop() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.server != nil {
		if err := a.server.Shutdown(context.Background()); err != nil {
			return fmt.Errorf("wecom: shutdown: %w", err)
		}
		a.server = nil
	}
	slog.Info("wecom: channel stopped")
	return nil
}

// OnIncomingMessage implements workwx.RxMessageHandler.
// It is called by the go-workwx HTTP handler when a message is received.
func (a *Adapter) OnIncomingMessage(msg *workwx.RxMessage) error {
	if msg == nil {
		return nil
	}

	if msg.Event != "LOCATION" {
		slog.Info("wecom: raw message",
			"from_user_id", msg.FromUserID,
			"msg_type", msg.MsgType,
			"msg_id", msg.MsgID,
			"agent_id", msg.AgentID,
			"event", msg.Event,
			"send_time", msg.SendTime,
			"full", msg.String(),
		)
	}

	senderID := msg.FromUserID
	if senderID == "" {
		senderID = "unknown"
	}

	// Extract text content
	content := extractTextContent(msg)

	// Extract media items
	var media []nanoprotocol.MediaItem
	media = extractMediaItems(msg)

	slog.Info("wecom: extracted message parts", "content_len", len(content), "media_count", len(media), "msg_type", msg.MsgType)
	for i, item := range media {
		slog.Info("wecom: media item", "index", i, "platform", item.Platform, "media_id", item.MediaID, "filename", item.Filename, "mime_type", item.MimeType)
	}

	if content == "" && len(media) == 0 {
		slog.Warn("wecom: ignoring message with no content and no media",
			"msg_type", msg.MsgType,
			"msg_id", msg.MsgID,
			"from_user_id", msg.FromUserID,
		)
		return nil
	}

	slog.Info("wecom: message received", "sender_id", senderID, "msg_id", msg.MsgID, "media_count", len(media))

	if a.handler != nil {
		return a.handler(context.Background(), channel.InboundMessage{
			Channel:   "wecom",
			SenderID:  senderID,
			ChatID:    senderID, // For 1-to-1, ChatID = FromUserID for direct replies
			MessageID: strconv.FormatInt(msg.MsgID, 10),
			Content:   content,
			Media:     media,
			Metadata: map[string]any{
				"msg_id":   msg.MsgID,
				"agent_id": msg.AgentID,
			},
		})
	}
	return nil
}

// extractTextContent extracts text from a WeCom RxMessage.
func extractTextContent(msg *workwx.RxMessage) string {
	if msg == nil {
		return ""
	}
	extras, ok := msg.Text()
	if !ok {
		return ""
	}
	return extras.GetContent()
}

// extractMediaItems extracts media items from a WeCom RxMessage.
func extractMediaItems(msg *workwx.RxMessage) []nanoprotocol.MediaItem {
	if msg == nil {
		return nil
	}

	slog.Debug(msg.String())

	var items []nanoprotocol.MediaItem

	// Image message
	if img, ok := msg.Image(); ok {
		items = append(items, nanoprotocol.MediaItem{
			Platform: "wecom",
			MediaID:  img.GetMediaID(),
			MimeType: "image/jpeg",
		})
	}

	// Voice message
	if voice, ok := msg.Voice(); ok {
		items = append(items, nanoprotocol.MediaItem{
			Platform: "wecom",
			MediaID:  voice.GetMediaID(),
			MimeType: "audio/amr",
		})
	}

	// Video message
	if video, ok := msg.Video(); ok {
		items = append(items, nanoprotocol.MediaItem{
			Platform: "wecom",
			MediaID:  video.GetMediaID(),
			MimeType: "video/mp4",
		})
	}

	// File message
	if file, ok := msg.File(); ok {
		items = append(items, nanoprotocol.MediaItem{
			Platform: "wecom",
			MediaID:  file.GetMediaID(),
			Filename: file.GetFileName(),
			MimeType: "application/octet-stream",
		})
	}

	return items
}
