package gateway

// Per-conversation subscriptions: one stream per chat, the channel-wide stream
// as the fallback, and no event delivered twice while both exist.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gitee.com/kobeyoung81/nanogateway/internal/channel"
)

type recordingChannel struct {
	name string
	mu   sync.Mutex
	sent []channel.OutboundMessage
}

func (c *recordingChannel) Name() string { return c.name }
func (c *recordingChannel) Start(context.Context, channel.InboundHandler) error {
	return nil
}
func (c *recordingChannel) Send(_ context.Context, msg channel.OutboundMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, msg)
	return nil
}
func (c *recordingChannel) Stop() error { return nil }

func (c *recordingChannel) messages() []channel.OutboundMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]channel.OutboundMessage(nil), c.sent...)
}

func TestConversationSubscriptionsTrackAndEvict(t *testing.T) {
	m := newConversationSubscriptions(2)
	cancelled := 0
	cancel := func() { cancelled++ }

	m.add(conversationKey{channel: "wecom", chatID: "a"}, cancel)
	m.add(conversationKey{channel: "wecom", chatID: "b"}, cancel)
	if !m.active("wecom", "a") || !m.active("wecom", "b") {
		t.Fatal("subscriptions were not recorded")
	}

	// A third one exceeds the cap: the oldest goes, and its stream is cancelled.
	m.add(conversationKey{channel: "wecom", chatID: "c"}, cancel)
	if m.active("wecom", "a") {
		t.Fatal("the oldest subscription was not evicted")
	}
	if !m.active("wecom", "c") {
		t.Fatal("the newest subscription was not recorded")
	}
	if cancelled != 1 {
		t.Fatalf("cancel was called %d times, want 1", cancelled)
	}

	m.remove(conversationKey{channel: "wecom", chatID: "c"})
	if m.active("wecom", "c") {
		t.Fatal("remove did not drop the subscription")
	}
}

// TestChannelStreamSkipsSubscribedConversations pins the no-duplicate rule: the
// legacy channel-wide stream must not deliver a conversation that has its own.
func TestChannelStreamSkipsSubscribedConversations(t *testing.T) {
	ch := &recordingChannel{name: "wecom"}
	g := &Gateway{subs: newConversationSubscriptions(10), channelMap: map[string]channel.Channel{"wecom": ch}}
	g.subs.add(conversationKey{channel: "wecom", chatID: "c1"}, func() {})

	agg := newChunkAggregator()
	g.deliverEvent(context.Background(), ch, agg, "chunk", `{"kind":"content","chat_id":"c1","content":"dedicated"}`)
	g.deliverEvent(context.Background(), ch, agg, "chunk", `{"kind":"content","chat_id":"c2","content":"fallback"}`)
	g.deliverEvent(context.Background(), ch, agg, "complete", `{"chat_id":"c2","full_content":"fallback"}`)

	msgs := ch.messages()
	if len(msgs) != 1 || msgs[0].Content != "fallback" {
		t.Fatalf("sent = %+v, want only the unsubscribed conversation", msgs)
	}
}

// TestNeverConnectedSubscriptionReleasesTheFallback: if the bus never serves the
// conversation stream, the key must be released so the channel-wide stream keeps
// delivering. Holding it would silently drop the reply.
func TestNeverConnectedSubscriptionReleasesTheFallback(t *testing.T) {
	bus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer bus.Close()

	old := conversationConnectGrace
	conversationConnectGrace = 50 * time.Millisecond
	defer func() { conversationConnectGrace = old }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := &Gateway{
		busURL:     bus.URL,
		subs:       newConversationSubscriptions(10),
		channelMap: map[string]channel.Channel{"wecom": &recordingChannel{name: "wecom"}},
		runCtx:     ctx,
	}
	g.ensureConversation(ctx, "wecom", "c1", "")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !g.subs.active("wecom", "c1") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("a subscription that never connected kept the fallback away")
}

// TestConversationSubscriptionDeliversAndCloses runs the real SSE loop against
// a fake bus: a chunk and a complete arrive, the channel gets the answer, and
// the subscription closes itself.
func TestConversationSubscriptionDeliversAndCloses(t *testing.T) {
	bus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("chat_id") != "c1" {
			t.Errorf("stream was not scoped to the conversation: %s", r.URL.RawQuery)
		}
		if strings.HasSuffix(r.URL.Path, "/cursor") {
			fmt.Fprint(w, `{"id":"7"}`)
			return
		}
		// A fresh subscription starts after the current end, so the previous
		// turn is not replayed into the client that just asked a new question.
		if got := r.Header.Get("Last-Event-ID"); got != "7" {
			t.Errorf("Last-Event-ID = %q, want the cursor", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, "event: chunk\nid: 1\ndata: {\"kind\":\"content\",\"chat_id\":\"c1\",\"content\":\"hi\"}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "event: complete\nid: 2\ndata: {\"chat_id\":\"c1\",\"full_content\":\"hi\"}\n\n")
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer bus.Close()

	ch := &recordingChannel{name: "wecom"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := &Gateway{
		busURL:     bus.URL,
		subs:       newConversationSubscriptions(10),
		channelMap: map[string]channel.Channel{"wecom": ch},
		runCtx:     ctx,
	}

	g.ensureConversation(ctx, "wecom", "c1", "7")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(ch.messages()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	msgs := ch.messages()
	if len(msgs) != 1 || msgs[0].Content != "hi" {
		t.Fatalf("sent = %+v", msgs)
	}

	// `complete` closes the subscription, so a second inbound message starts a
	// fresh stream instead of reusing a finished one.
	closed := false
	for time.Now().Before(deadline) {
		if !g.subs.active("wecom", "c1") {
			closed = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !closed {
		t.Fatal("the subscription stayed open after complete")
	}
}
