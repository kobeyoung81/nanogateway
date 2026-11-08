package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"gitee.com/kobeyoung81/nanogateway/internal/auth"
	"gitee.com/kobeyoung81/nanogateway/internal/channel"
	"gitee.com/kobeyoung81/nanogateway/internal/config"
	"gitee.com/kobeyoung81/nanoprotocol"
	"github.com/google/uuid"
)

// Gateway manages platform adapters and routes messages to/from the bus.
type Gateway struct {
	busURL       string
	channels     []channel.Channel
	channelMap   map[string]channel.Channel
	gatewayID    string
	ackEnabled   bool
	ackText      string
	jwtValidator *auth.JWKSValidator
	// errorMessages maps "<channel>.<category>" to a user-facing notice for
	// failed turns (batch channels only; the web channel renders its own).
	// Optional — built-in per-language defaults apply when unset.
	errorMessages map[string]string
}

// SetErrorMessages installs per-channel turn-failure notice templates.
func (g *Gateway) SetErrorMessages(m map[string]string) {
	g.errorMessages = m
}

// New creates a new Gateway instance.
func New(busURL string, channels []channel.Channel, ackEnabled bool, ackText string, jwtValidator *auth.JWKSValidator) *Gateway {
	chMap := make(map[string]channel.Channel, len(channels))
	for _, ch := range channels {
		chMap[ch.Name()] = ch
	}
	return &Gateway{
		busURL:       busURL,
		channels:     channels,
		channelMap:   chMap,
		gatewayID:    uuid.New().String(),
		ackEnabled:   ackEnabled,
		ackText:      ackText,
		jwtValidator: jwtValidator,
	}
}

// Run starts the gateway service.
func (g *Gateway) Run(ctx context.Context) error {
	slog.Info("gateway started", "id", g.gatewayID, "channels", len(g.channels))

	go g.subscribeOutbound(ctx)

	for _, ch := range g.channels {
		ch := ch
		go func() {
			slog.Info("starting channel", "name", ch.Name())
			if err := ch.Start(ctx, g.handleInbound); err != nil {
				slog.Error("channel failed", "name", ch.Name(), "error", err)
			}
		}()
	}

	<-ctx.Done()
	return ctx.Err()
}

// handleInbound processes a message received from a channel adapter.
func (g *Gateway) handleInbound(ctx context.Context, msg channel.InboundMessage) error {
	client, err := NewBusClient(g.busURL)
	if err != nil {
		return fmt.Errorf("connect to bus: %w", err)
	}
	defer client.Close()

	resolved := &nanoprotocol.ResolveUserResponse{UserID: msg.UserID}
	if msg.UserID == "" {
		resolved, err = client.ResolveUser(msg.Channel, msg.SenderID)
		if err != nil {
			slog.Warn("unknown user", "channel", msg.Channel, "sender", msg.SenderID, "error", err)
			return nil
		}
	}

	slog.Debug("resolved user", "channel", msg.Channel, "sender", msg.SenderID, "user_id", resolved.UserID, "binding_id", resolved.BindingID)

	// Channels that don't manage sessions themselves (wecom, wechatkf, feishu,
	// cli) derive a stable session key from the channel and chat identity so
	// their conversations map to a consistent session instead of collapsing
	// into an empty id.
	msg.SessionID = deriveSessionID(msg.SessionID, msg.Channel, msg.ChatID)

	// Channels that don't name an agent explicitly (wecom, wechatkf, feishu,
	// cli) talk to the binding's default agent (explicit preference, else the
	// owner's first active agent). The web channel always names an agent.
	if msg.AgentID == "" && resolved.BindingID > 0 {
		agentID, err := client.GetBindingAgent(resolved.BindingID)
		if err != nil {
			slog.Warn("resolve binding agent failed", "binding_id", resolved.BindingID, "error", err)
		} else if agentID != "" {
			msg.AgentID = agentID
			slog.Debug("resolved binding agent", "binding_id", resolved.BindingID, "agent", agentID)
		}
	}

	// Send ack before posting to bus
	if g.ackEnabled && g.ackText != "" {
		if ch := g.channelMap[msg.Channel]; ch != nil {
			if err := ch.Send(ctx, channel.OutboundMessage{ChatID: msg.ChatID, Content: g.ackText}); err != nil {
				slog.Warn("ack send failed", "channel", msg.Channel, "error", err)
			}
		}
	}

	err = client.PostInbound(nanoprotocol.InboundMessage{
		UserID:    resolved.UserID,
		BindingID: resolved.BindingID,
		AgentID:   msg.AgentID,
		SessionID: msg.SessionID,
		Channel:   msg.Channel,
		ChatID:    msg.ChatID,
		SenderID:  msg.SenderID,
		MessageID: msg.MessageID,
		Content:   msg.Content,
		Media:     msg.Media,
		Metadata:  msg.Metadata,
		Timestamp: time.Now(),
	})
	if err != nil {
		return fmt.Errorf("post inbound: %w", err)
	}

	slog.Debug("posted inbound to bus", "user_id", resolved.UserID, "binding_id", resolved.BindingID, "channel", msg.Channel, "content_len", len(msg.Content))
	return nil
}

// chunkAggregator buffers streaming chunks per chat ID for batch-oriented platforms.
type chunkAggregator struct {
	mu      sync.Mutex
	partial map[string]string // chatID → accumulated content
}

func newChunkAggregator() *chunkAggregator {
	return &chunkAggregator{partial: make(map[string]string)}
}

func (a *chunkAggregator) add(chatID, content string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.partial[chatID] += content
	return a.partial[chatID]
}

func (a *chunkAggregator) take(chatID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	content := a.partial[chatID]
	delete(a.partial, chatID)
	return content
}

// subscribeOutbound opens one SSE stream per channel for outbound delivery.
// This replaces the old 500ms polling loop with push-based streaming.
func (g *Gateway) subscribeOutbound(ctx context.Context) {
	for _, ch := range g.channels {
		ch := ch
		go g.subscribeChannel(ctx, ch)
	}
}

func (g *Gateway) subscribeChannel(ctx context.Context, ch channel.Channel) {
	agg := newChunkAggregator()
	backoff := time.Second
	lastEventID := ""
	for {
		url := g.busBaseURL() + "/api/v1/outbound/stream?channel=" + ch.Name()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			slog.Warn("sse request build error", "channel", ch.Name(), "error", err)
			return
		}
		req.Header.Set("Accept", "text/event-stream")
		if lastEventID != "" {
			req.Header.Set("Last-Event-ID", lastEventID)
		}
		// The SSE subscription is a bus internal endpoint like the rest.
		if token := config.InternalToken(); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			slog.Warn("sse connect error", "channel", ch.Name(), "error", err)
			time.Sleep(backoff)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			slog.Warn("sse unexpected status", "channel", ch.Name(), "status", resp.StatusCode)
			resp.Body.Close()
			time.Sleep(backoff)
			continue
		}

		slog.Info("sse subscribed", "channel", ch.Name())
		g.drainStream(ctx, ch, agg, resp.Body, &lastEventID)
		resp.Body.Close()
		slog.Warn("sse stream ended, reconnecting", "channel", ch.Name())
		time.Sleep(backoff)
	}
}

// drainStream reads SSE events and dispatches them to the channel adapter.
func (g *Gateway) drainStream(ctx context.Context, ch channel.Channel, agg *chunkAggregator, body io.Reader, lastEventID *string) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var eventType, eventID, eventData string
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			eventType = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "id:"):
			eventID = strings.TrimSpace(line[len("id:"):])
		case strings.HasPrefix(line, "data:"):
			eventData += strings.TrimSpace(line[len("data:"):])
		case line == "" && eventData != "":
			if eventID != "" {
				*lastEventID = eventID
			}
			g.deliverEvent(ctx, ch, agg, eventType, eventData)
			eventType, eventID, eventData = "", "", ""
		}
	}
}

// deliverEvent dispatches a single SSE event to a channel adapter.
func (g *Gateway) deliverEvent(ctx context.Context, ch channel.Channel, agg *chunkAggregator, eventType, data string) {
	switch eventType {
	case "chunk":
		var chunk nanoprotocol.OutboundChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			slog.Warn("chunk decode error", "channel", ch.Name(), "error", err)
			return
		}
		if streamer, ok := ch.(channel.StreamSender); ok {
			if err := streamer.SendChunk(ctx, chunk); err != nil {
				slog.Warn("streaming send chunk error", "channel", ch.Name(), "error", err)
			}
			return
		}
		// Batch platforms: accumulate only the answer content. Intermediate
		// events (thought, tool_use, tool_result) are for streaming channels
		// only — batch channels receive the clean final answer.
		if chunk.Kind == "" || chunk.Kind == nanoprotocol.ChunkKindContent {
			agg.add(chunk.ChatID, chunk.Content)
		}

	case "complete":
		var complete nanoprotocol.OutboundComplete
		if err := json.Unmarshal([]byte(data), &complete); err != nil {
			slog.Warn("complete decode error", "channel", ch.Name(), "error", err)
			return
		}
		if streamer, ok := ch.(channel.StreamSender); ok {
			if err := streamer.SendComplete(ctx, complete); err != nil {
				slog.Warn("streaming send complete error", "channel", ch.Name(), "error", err)
			}
			return
		}
		// Batch platforms: flush the accumulated full message.
		content := agg.take(complete.ChatID)
		if content == "" {
			content = complete.FullContent
		}
		// A failed turn must never be silent: append (or substitute) a
		// channel-appropriate notice describing the failure.
		if complete.Error != "" {
			slog.Warn("turn failed", "channel", ch.Name(), "chat_id", complete.ChatID,
				"category", complete.Error, "detail", complete.ErrorDetail)
			notice := g.errorNotice(ch.Name(), complete.Error)
			if content != "" {
				content = content + "\n\n" + notice
			} else {
				content = notice
			}
		}
		if content == "" {
			return
		}
		slog.Info("sending outbound message", "channel", ch.Name(), "chat_id", complete.ChatID)
		if err := ch.Send(ctx, channel.OutboundMessage{ChatID: complete.ChatID, Content: content}); err != nil {
			slog.Error("send error", "channel", ch.Name(), "error", err)
		}
	}
}

// errorNotice renders the user-facing notice for a failed turn. Configured
// overrides ("<channel>.<category>") win; otherwise a built-in default is
// used, localized per channel audience (Chinese for WeChat-family channels,
// English elsewhere).
func (g *Gateway) errorNotice(channelName, category string) string {
	if g.errorMessages != nil {
		if text, ok := g.errorMessages[channelName+"."+category]; ok && text != "" {
			return text
		}
		if text, ok := g.errorMessages[channelName+".default"]; ok && text != "" {
			return text
		}
	}
	return defaultErrorNotice(channelName, category)
}

func defaultErrorNotice(channelName, category string) string {
	zh := channelName == "wecom" || channelName == "wechatkf" || channelName == "feishu"
	type text struct{ zh, en string }
	msgs := map[string]text{
		nanoprotocol.TurnErrorRateLimited:         {"助手当前繁忙，请稍后再试。", "The assistant is busy right now — please try again shortly."},
		nanoprotocol.TurnErrorProviderUnavailable: {"助手服务暂时不可用，请稍后重试。", "The assistant service is temporarily unavailable — please try again."},
		nanoprotocol.TurnErrorTimeout:             {"回复超时，请稍后重试或降低思考强度。", "The request timed out — please try again, or reduce reasoning effort."},
		nanoprotocol.TurnErrorProviderAuth:        {"助手配置有误，请联系管理员。", "The assistant is misconfigured — please contact an administrator."},
		nanoprotocol.TurnErrorProviderNotFound:    {"助手配置有误，请联系管理员。", "The assistant is misconfigured — please contact an administrator."},
		nanoprotocol.TurnErrorContextTooLong:      {"对话过长，请发送 /new 开启新会话后再试。", "The conversation is too long — start a new session with /new and try again."},
		nanoprotocol.TurnErrorToolCallIncomplete:  {"上游返回的工具调用参数不完整，模型未能重发。请把这一步拆小，或换个模型再试。", "The upstream returned a tool call with unusable arguments and the model could not re-emit it. Split this step into smaller ones, or try another model."},
		nanoprotocol.TurnErrorMaxIterations:       {"任务未能在限定步骤内完成，请尝试拆分请求。", "The task could not be completed within the step limit — try a simpler request."},
		nanoprotocol.TurnErrorInternal:            {"处理请求时出现问题，请稍后重试。", "Something went wrong while processing your request — please try again."},
	}
	t, ok := msgs[category]
	if !ok {
		t = msgs[nanoprotocol.TurnErrorInternal]
	}
	if zh {
		return t.zh
	}
	return t.en
}

// deriveSessionID returns the session key for an inbound message, deriving a
// stable one from the channel and chat identity when the platform does not
// provide one (wecom, wechatkf, feishu, cli).
func deriveSessionID(sessionID, channel, chatID string) string {
	if sessionID != "" {
		return sessionID
	}
	if chatID == "" {
		return ""
	}
	return channel + ":" + chatID
}

// busBaseURL returns the HTTP form of the bus URL.
func (g *Gateway) busBaseURL() string {
	u, err := url.Parse(g.busURL)
	if err != nil {
		return g.busURL
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String()
}
