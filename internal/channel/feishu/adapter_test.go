package feishu

import (
	"context"
	"testing"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"gitee.com/kobeyoung81/nanogateway/internal/channel"
)

func TestNew_Valid(t *testing.T) {
	a, err := New("app_id", "app_secret", "ek", "vt")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if a.Name() != "feishu" {
		t.Errorf("Name() = %q, want %q", a.Name(), "feishu")
	}
}

func TestNew_MissingAppID(t *testing.T) {
	_, err := New("", "secret", "", "")
	if err == nil {
		t.Fatal("New() expected error for empty appID")
	}
}

func TestNew_MissingAppSecret(t *testing.T) {
	_, err := New("app_id", "", "", "")
	if err == nil {
		t.Fatal("New() expected error for empty appSecret")
	}
}

func TestExtractSenderID(t *testing.T) {
	tests := []struct {
		name   string
		sender *larkim.EventSender
		want   string
	}{
		{"nil sender", nil, ""},
		{"nil sender id", &larkim.EventSender{}, ""},
		{"user id", &larkim.EventSender{
			SenderId: &larkim.UserId{UserId: strPtr("u123")},
		}, "u123"},
		{"open id fallback", &larkim.EventSender{
			SenderId: &larkim.UserId{OpenId: strPtr("ou_abc")},
		}, "ou_abc"},
		{"union id fallback", &larkim.EventSender{
			SenderId: &larkim.UserId{UnionId: strPtr("on_xyz")},
		}, "on_xyz"},
		{"user id takes priority", &larkim.EventSender{
			SenderId: &larkim.UserId{
				UserId:  strPtr("u123"),
				OpenId:  strPtr("ou_abc"),
				UnionId: strPtr("on_xyz"),
			},
		}, "u123"},
		{"empty user id falls to open id", &larkim.EventSender{
			SenderId: &larkim.UserId{
				UserId: strPtr(""),
				OpenId: strPtr("ou_abc"),
			},
		}, "ou_abc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractSenderID(tt.sender)
			if got != tt.want {
				t.Errorf("extractSenderID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractMessageContent(t *testing.T) {
	textType := larkim.MsgTypeText
	imgType := "image"

	tests := []struct {
		name    string
		message *larkim.EventMessage
		want    string
	}{
		{"nil message", nil, ""},
		{"nil content", &larkim.EventMessage{}, ""},
		{"empty content", &larkim.EventMessage{Content: strPtr("")}, ""},
		{"text message", &larkim.EventMessage{
			MessageType: &textType,
			Content:     strPtr(`{"text":"hello world"}`),
		}, "hello world"},
		{"non-text returns raw", &larkim.EventMessage{
			MessageType: &imgType,
			Content:     strPtr(`{"image_key":"img_123"}`),
		}, `{"image_key":"img_123"}`},
		{"invalid json text", &larkim.EventMessage{
			MessageType: &textType,
			Content:     strPtr(`not json`),
		}, "not json"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractMessageContent(tt.message)
			if got != tt.want {
				t.Errorf("extractMessageContent() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStringValue(t *testing.T) {
	if got := stringValue(nil); got != "" {
		t.Errorf("stringValue(nil) = %q, want empty", got)
	}
	s := "hello"
	if got := stringValue(&s); got != "hello" {
		t.Errorf("stringValue(&s) = %q, want %q", got, "hello")
	}
}

func TestHandleMessageReceive(t *testing.T) {
	a, err := New("app_id", "app_secret", "", "")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	var received channel.InboundMessage
	a.handler = func(_ context.Context, msg channel.InboundMessage) error {
		received = msg
		return nil
	}

	textType := larkim.MsgTypeText
	chatType := "p2p"
	event := &larkim.P2MessageReceiveV1{}
	event.Event = &larkim.P2MessageReceiveV1Data{
		Sender: &larkim.EventSender{
			SenderId: &larkim.UserId{UserId: strPtr("u_sender")},
		},
		Message: &larkim.EventMessage{
			ChatId:      strPtr("chat_123"),
			MessageId:   strPtr("msg_001"),
			MessageType: &textType,
			ChatType:    &chatType,
			Content:     strPtr(`{"text":"Hi bot"}`),
		},
	}

	if err := a.handleMessageReceive(context.Background(), event); err != nil {
		t.Fatalf("handleMessageReceive() error = %v", err)
	}

	if received.Channel != "feishu" {
		t.Errorf("Channel = %q, want %q", received.Channel, "feishu")
	}
	if received.SenderID != "u_sender" {
		t.Errorf("SenderID = %q, want %q", received.SenderID, "u_sender")
	}
	if received.ChatID != "chat_123" {
		t.Errorf("ChatID = %q, want %q", received.ChatID, "chat_123")
	}
	if received.Content != "Hi bot" {
		t.Errorf("Content = %q, want %q", received.Content, "Hi bot")
	}
	if received.MessageID != "msg_001" {
		t.Errorf("MessageID = %q, want %q", received.MessageID, "msg_001")
	}
	if received.Metadata["message_id"] != "msg_001" {
		t.Errorf("metadata[message_id] = %v, want %q", received.Metadata["message_id"], "msg_001")
	}
}

func TestHandleMessageReceive_NilEvent(t *testing.T) {
	a, _ := New("app_id", "app_secret", "", "")
	called := false
	a.handler = func(_ context.Context, _ channel.InboundMessage) error {
		called = true
		return nil
	}

	// nil event
	if err := a.handleMessageReceive(context.Background(), nil); err != nil {
		t.Fatalf("handleMessageReceive(nil) error = %v", err)
	}
	if called {
		t.Error("handler should not be called for nil event")
	}
}

func TestHandleMessageReceive_EmptyChatID(t *testing.T) {
	a, _ := New("app_id", "app_secret", "", "")
	called := false
	a.handler = func(_ context.Context, _ channel.InboundMessage) error {
		called = true
		return nil
	}

	textType := larkim.MsgTypeText
	event := &larkim.P2MessageReceiveV1{}
	event.Event = &larkim.P2MessageReceiveV1Data{
		Message: &larkim.EventMessage{
			ChatId:      strPtr(""),
			MessageType: &textType,
			Content:     strPtr(`{"text":"test"}`),
		},
	}

	if err := a.handleMessageReceive(context.Background(), event); err != nil {
		t.Fatalf("handleMessageReceive() error = %v", err)
	}
	if called {
		t.Error("handler should not be called for empty chatID")
	}
}

func TestHandleMessageReceive_EmptyContent(t *testing.T) {
	a, _ := New("app_id", "app_secret", "", "")
	called := false
	a.handler = func(_ context.Context, _ channel.InboundMessage) error {
		called = true
		return nil
	}

	event := &larkim.P2MessageReceiveV1{}
	event.Event = &larkim.P2MessageReceiveV1Data{
		Message: &larkim.EventMessage{
			ChatId:  strPtr("chat_123"),
			Content: strPtr(""),
		},
	}

	if err := a.handleMessageReceive(context.Background(), event); err != nil {
		t.Fatalf("handleMessageReceive() error = %v", err)
	}
	if called {
		t.Error("handler should not be called for empty content")
	}
}

func strPtr(s string) *string { return &s }
