// Package feishu implements the Channel interface for Feishu (Lark).
// It uses a WebSocket long connection so no public IP is required.
package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkdispatcher "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"gitee.com/kobeyoung81/nanogateway/internal/channel"
)

// Adapter implements the Channel interface for Feishu.
type Adapter struct {
	appID             string
	appSecret         string
	encryptKey        string
	verificationToken string

	client   *lark.Client
	wsClient *larkws.Client
	handler  channel.InboundHandler

	mu     sync.Mutex
	cancel context.CancelFunc
}

// New creates a new Feishu adapter.
func New(appID, appSecret, encryptKey, verificationToken string) (*Adapter, error) {
	if appID == "" {
		return nil, fmt.Errorf("feishu: appID is required")
	}
	if appSecret == "" {
		return nil, fmt.Errorf("feishu: appSecret is required")
	}

	return &Adapter{
		appID:             appID,
		appSecret:         appSecret,
		encryptKey:        encryptKey,
		verificationToken: verificationToken,
		client:            lark.NewClient(appID, appSecret),
	}, nil
}

func (a *Adapter) Name() string { return "feishu" }

func (a *Adapter) Start(ctx context.Context, handler channel.InboundHandler) error {
	a.handler = handler

	dispatcher := larkdispatcher.NewEventDispatcher(a.verificationToken, a.encryptKey).
		OnP2MessageReceiveV1(a.handleMessageReceive)

	runCtx, cancel := context.WithCancel(ctx)

	a.mu.Lock()
	a.cancel = cancel
	a.wsClient = larkws.NewClient(
		a.appID,
		a.appSecret,
		larkws.WithEventHandler(dispatcher),
	)
	wsClient := a.wsClient
	a.mu.Unlock()

	slog.Info("feishu: channel started (websocket mode)")

	// wsClient.Start blocks until context is cancelled or fatal error.
	err := wsClient.Start(runCtx)
	if err != nil && ctx.Err() == nil {
		return fmt.Errorf("feishu: websocket stopped: %w", err)
	}
	return ctx.Err()
}

func (a *Adapter) Send(ctx context.Context, msg channel.OutboundMessage) error {
	if msg.ChatID == "" {
		return fmt.Errorf("feishu: chat ID is empty")
	}

	payload, err := json.Marshal(map[string]string{"text": msg.Content})
	if err != nil {
		return fmt.Errorf("feishu: marshal content: %w", err)
	}

	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.ReceiveIdTypeChatId).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(msg.ChatID).
			MsgType(larkim.MsgTypeText).
			Content(string(payload)).
			Uuid(fmt.Sprintf("nanosec-%d", time.Now().UnixNano())).
			Build()).
		Build()

	resp, err := a.client.Im.V1.Message.Create(ctx, req)
	if err != nil {
		return fmt.Errorf("feishu: send message: %w", err)
	}

	if !resp.Success() {
		return fmt.Errorf("feishu: API error: code=%d msg=%s", resp.Code, resp.Msg)
	}

	slog.Debug("feishu: message sent", "chat_id", msg.ChatID)
	return nil
}

func (a *Adapter) Stop() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	a.wsClient = nil
	slog.Info("feishu: channel stopped")
	return nil
}

func (a *Adapter) handleMessageReceive(_ context.Context, event *larkim.P2MessageReceiveV1) error {
	if event == nil || event.Event == nil || event.Event.Message == nil {
		return nil
	}

	message := event.Event.Message
	sender := event.Event.Sender

	chatID := stringValue(message.ChatId)
	if chatID == "" {
		return nil
	}

	senderID := extractSenderID(sender)
	if senderID == "" {
		senderID = "unknown"
	}

	content := extractMessageContent(message)
	if content == "" {
		return nil
	}

	metadata := map[string]any{}
	if messageID := stringValue(message.MessageId); messageID != "" {
		metadata["message_id"] = messageID
	}
	if messageType := stringValue(message.MessageType); messageType != "" {
		metadata["message_type"] = messageType
	}
	if chatType := stringValue(message.ChatType); chatType != "" {
		metadata["chat_type"] = chatType
	}

	slog.Info("feishu: message received", "sender_id", senderID, "chat_id", chatID)

	if a.handler != nil {
		return a.handler(context.Background(), channel.InboundMessage{
			Channel:   "feishu",
			SenderID:  senderID,
			ChatID:    chatID,
			MessageID: stringValue(message.MessageId),
			Content:   content,
			Metadata:  metadata,
		})
	}
	return nil
}

// extractSenderID extracts the sender identity, falling back through UserId → OpenId → UnionId.
func extractSenderID(sender *larkim.EventSender) string {
	if sender == nil || sender.SenderId == nil {
		return ""
	}
	if v := stringValue(sender.SenderId.UserId); v != "" {
		return v
	}
	if v := stringValue(sender.SenderId.OpenId); v != "" {
		return v
	}
	if v := stringValue(sender.SenderId.UnionId); v != "" {
		return v
	}
	return ""
}

// extractMessageContent extracts text content from a Feishu event message.
func extractMessageContent(message *larkim.EventMessage) string {
	if message == nil || message.Content == nil || *message.Content == "" {
		return ""
	}

	if message.MessageType != nil && *message.MessageType == larkim.MsgTypeText {
		var textPayload struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(*message.Content), &textPayload); err == nil {
			return textPayload.Text
		}
	}

	return *message.Content
}

// stringValue safely dereferences a string pointer.
func stringValue(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
