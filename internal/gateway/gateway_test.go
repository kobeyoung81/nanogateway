package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gitee.com/kobeyoung81/nanogateway/internal/channel"
	"gitee.com/kobeyoung81/nanoprotocol"
)

func TestDeriveSessionID(t *testing.T) {
	cases := []struct {
		name      string
		sessionID string
		channel   string
		chatID    string
		want      string
	}{
		{"web session id is preserved", "web:abc-123", "web", "abc-123", "web:abc-123"},
		{"wecom derives from channel+chat", "", "wecom", "user-X", "wecom:user-X"},
		{"wechatkf derives from channel+chat", "", "wechatkf", "kf-user-1", "wechatkf:kf-user-1"},
		{"empty chat yields empty", "", "wecom", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveSessionID(tc.sessionID, tc.channel, tc.chatID); got != tc.want {
				t.Fatalf("deriveSessionID(%q,%q,%q) = %q, want %q", tc.sessionID, tc.channel, tc.chatID, got, tc.want)
			}
		})
	}
}

type fakeBatchChannel struct {
	sent []channel.OutboundMessage
}

func (f *fakeBatchChannel) Name() string                                        { return "wecom" }
func (f *fakeBatchChannel) Start(context.Context, channel.InboundHandler) error { return nil }
func (f *fakeBatchChannel) Send(_ context.Context, msg channel.OutboundMessage) error {
	f.sent = append(f.sent, msg)
	return nil
}
func (f *fakeBatchChannel) Stop() error { return nil }

// TestDeliverEvent_BatchAggregatesContentOnly verifies that batch channels
// receive only the accumulated answer content — intermediate thought / tool
// events are discarded.
func TestDeliverEvent_BatchAggregatesContentOnly(t *testing.T) {
	g := &Gateway{}
	agg := newChunkAggregator()
	ch := &fakeBatchChannel{}

	g.deliverEvent(context.Background(), ch, agg, "chunk", `{"kind":"thought","chat_id":"c1","content":"hidden thinking"}`)
	g.deliverEvent(context.Background(), ch, agg, "chunk", `{"kind":"tool_use","chat_id":"c1","content":"{\"name\":\"bash\"}"}`)
	g.deliverEvent(context.Background(), ch, agg, "chunk", `{"kind":"tool_result","chat_id":"c1","content":"hidden output"}`)
	g.deliverEvent(context.Background(), ch, agg, "chunk", `{"kind":"content","chat_id":"c1","content":"Hello"}`)
	g.deliverEvent(context.Background(), ch, agg, "complete", `{"chat_id":"c1","full_content":"Hello world"}`)

	if len(ch.sent) != 1 {
		t.Fatalf("expected 1 batch message, got %d", len(ch.sent))
	}
	msg := ch.sent[0]
	if msg.Content != "Hello" {
		t.Errorf("batch message should contain only the aggregated content, got %q", msg.Content)
	}
	if contains(msg.Content, "thinking") || contains(msg.Content, "hidden") {
		t.Errorf("batch message must not leak intermediate events, got %q", msg.Content)
	}
}

// TestDeliverEvent_BatchErrorNotice verifies that a failed turn produces a
// user-facing notice on batch channels — silence is never acceptable.
func TestDeliverEvent_BatchErrorNotice(t *testing.T) {
	g := &Gateway{}
	agg := newChunkAggregator()
	ch := &fakeBatchChannel{}

	// A hard LLM failure: no chunks, only an error completion.
	complete := `{"chat_id":"c1","full_content":"","error":"provider_unavailable","error_detail":"502 bad gateway"}`
	g.deliverEvent(context.Background(), ch, agg, "complete", complete)

	if len(ch.sent) != 1 {
		t.Fatalf("expected exactly one notice message, got %d", len(ch.sent))
	}
	if ch.sent[0].ChatID != "c1" {
		t.Errorf("chat_id = %q, want c1", ch.sent[0].ChatID)
	}
	if ch.sent[0].Content == "" {
		t.Fatal("expected non-empty error notice")
	}
}

// TestDeliverEvent_BatchErrorNoticeWithPartial verifies partial content is
// delivered before the failure notice.
func TestDeliverEvent_BatchErrorNoticeWithPartial(t *testing.T) {
	g := &Gateway{}
	agg := newChunkAggregator()
	ch := &fakeBatchChannel{}

	g.deliverEvent(context.Background(), ch, agg, "chunk",
		`{"chat_id":"c1","kind":"content","content":"partial answer"}`)
	g.deliverEvent(context.Background(), ch, agg, "complete",
		`{"chat_id":"c1","full_content":"partial answer","error":"rate_limited"}`)

	if len(ch.sent) != 1 {
		t.Fatalf("expected one message, got %d", len(ch.sent))
	}
	if !contains(ch.sent[0].Content, "partial answer") {
		t.Errorf("partial content missing: %q", ch.sent[0].Content)
	}
	if !contains(ch.sent[0].Content, "繁忙") {
		t.Errorf("Chinese notice expected for wecom: %q", ch.sent[0].Content)
	}
}

// TestErrorNoticeOverride verifies configured per-channel overrides win.
func TestErrorNoticeOverride(t *testing.T) {
	g := &Gateway{errorMessages: map[string]string{"wecom.rate_limited": "custom notice"}}
	if got := g.errorNotice("wecom", "rate_limited"); got != "custom notice" {
		t.Fatalf("errorNotice = %q, want custom notice", got)
	}
	if got := g.errorNotice("cli", "rate_limited"); contains(got, "繁忙") {
		t.Fatalf("cli should use English default, got %q", got)
	}
}

// TestDefaultErrorNotice_RequestTimeout verifies the new request_timeout
// category maps to a user-facing message rather than falling through to the
// generic "internal" notice.
func TestDefaultErrorNotice_RequestTimeout(t *testing.T) {
	zh := defaultErrorNotice("wecom", nanoprotocol.TurnErrorTimeout)
	if !contains(zh, "超时") {
		t.Fatalf("wecom request_timeout notice should mention 超时, got %q", zh)
	}
	en := defaultErrorNotice("cli", nanoprotocol.TurnErrorTimeout)
	if !contains(en, "timed out") {
		t.Fatalf("cli request_timeout notice should mention 'timed out', got %q", en)
	}
	if defaultErrorNotice("cli", nanoprotocol.TurnErrorTimeout) == defaultErrorNotice("cli", nanoprotocol.TurnErrorInternal) {
		t.Fatalf("request_timeout must not fall through to the internal notice")
	}
}

// The outbound SSE subscription is a bus internal endpoint, so its request has
// to carry the shared token like every other bus call.
func TestSubscribeChannelSendsTheInternalToken(t *testing.T) {
	t.Setenv("NANOSEC_BUS_INTERNAL_TOKEN", "s3cret")

	headers := make(chan string, 1)
	bus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case headers <- r.Header.Get("Authorization"):
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer bus.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := &Gateway{busURL: bus.URL}
	ch := &fakeBatchChannel{}
	go g.subscribeChannel(ctx, ch)

	select {
	case got := <-headers:
		if got != "Bearer s3cret" {
			t.Errorf("Authorization = %q, want the configured token", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no outbound stream request observed")
	}
}
