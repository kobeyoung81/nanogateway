package gateway

// Per-conversation outbound subscriptions.
//
// The bus stores chunks per (channel, chat_id) and serves them on a
// conversation-scoped SSE. The gateway used to keep one stream per channel and
// fan out by chat id, which shares one buffer and one id sequence between every
// conversation on that channel. Now a conversation gets its own subscription,
// opened when an inbound message is dispatched and closed by its `complete` (or
// after an idle window); the channel-wide stream stays as the fallback for
// conversations nobody has written to yet (a cron turn, for example), and it
// skips any conversation that has its own subscription so nothing is delivered
// twice.

import (
	"bufio"
	"context"
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
	"gitee.com/kobeyoung81/nanogateway/internal/metrics"
)

// conversationKey identifies one subscription.
type conversationKey struct {
	channel string
	chatID  string
}

// conversationSub is one live subscription.
type conversationSub struct {
	key    conversationKey
	cancel context.CancelFunc

	mu           sync.Mutex
	lastActivity time.Time
}

func (s *conversationSub) touch() {
	s.mu.Lock()
	s.lastActivity = time.Now()
	s.mu.Unlock()
}

func (s *conversationSub) idleFor() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastActivity)
}

// conversationSubscriptions tracks the live subscriptions and bounds them per
// adapter: a busy channel with hundreds of chats must not open an unbounded
// number of streams.
type conversationSubscriptions struct {
	mu    sync.Mutex
	subs  map[conversationKey]*conversationSub
	order []conversationKey // creation order, for evicting the oldest
	max   int
}

func newConversationSubscriptions(max int) *conversationSubscriptions {
	if max <= 0 {
		max = 200
	}
	return &conversationSubscriptions{
		subs: make(map[conversationKey]*conversationSub),
		max:  max,
	}
}

// add registers a subscription. created=false means one already existed and its
// activity was merely refreshed.
func (m *conversationSubscriptions) add(key conversationKey, cancel context.CancelFunc) (*conversationSub, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.subs[key]; ok {
		existing.touch()
		return existing, false
	}
	sub := &conversationSub{key: key, cancel: cancel, lastActivity: time.Now()}
	m.subs[key] = sub
	m.order = append(m.order, key)
	evicted := ""
	if len(m.order) > m.max {
		oldest := m.order[0]
		m.order = m.order[1:]
		if old, ok := m.subs[oldest]; ok {
			old.cancel()
			delete(m.subs, oldest)
			evicted = oldest.chatID
		}
	}
	if evicted != "" {
		slog.Warn("too many conversation subscriptions; evicted the oldest", "channel", key.channel, "chat_id", evicted)
	}
	return sub, true
}

func (m *conversationSubscriptions) remove(key conversationKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.subs, key)
	for i, k := range m.order {
		if k == key {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
}

// active reports whether this conversation has its own subscription, which the
// channel-wide fallback uses to avoid delivering an event twice.
func (m *conversationSubscriptions) active(channelName, chatID string) bool {
	if m == nil || chatID == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.subs[conversationKey{channel: channelName, chatID: chatID}]
	return ok
}

// lookup returns the live subscription for a key (used by tests and the
// watchdog).
func (m *conversationSubscriptions) lookup(channelName, chatID string) *conversationSub {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.subs[conversationKey{channel: channelName, chatID: chatID}]
}

// ensureConversation starts a subscription for one conversation if there is not
// one already. It is called when an inbound message is dispatched: that is the
// moment we know a reply is coming.
func (g *Gateway) ensureConversation(parent context.Context, channelName, chatID, startCursor string) {
	if chatID == "" || g.subs == nil {
		return
	}
	ch := g.channelMap[channelName]
	if ch == nil {
		return
	}
	key := conversationKey{channel: channelName, chatID: chatID}
	if g.subs.active(channelName, chatID) {
		if sub := g.subs.lookup(channelName, chatID); sub != nil {
			sub.touch()
		}
		return
	}

	ctx, cancel := context.WithCancel(parent)
	if _, created := g.subs.add(key, cancel); !created {
		cancel()
		return
	}
	metrics.Subscriptions.Inc()
	go g.runConversationSubscription(ctx, ch, key, cancel, startCursor)
}

// runConversationSubscription keeps one conversation's SSE open until its
// `complete` arrives, the idle window passes, or the gateway stops. A dropped
// stream is reconnected with the last event id, so a reconnect inside the
// retention window loses nothing.
func (g *Gateway) runConversationSubscription(ctx context.Context, ch channel.Channel, key conversationKey, cancel context.CancelFunc, startCursor string) {
	defer g.subs.remove(key)
	defer metrics.Subscriptions.Dec()
	defer cancel()

	agg := newChunkAggregator()
	backoff := time.Second
	// The caller read this before posting the message, so the reply starts here:
	// no replay of the previous turn, no gap before the stream opens.
	lastEventID := startCursor
	everConnected := false
	started := time.Now()
	sub := g.subs.lookup(key.channel, key.chatID)

	// The watchdog closes an abandoned conversation: a client that sent one
	// message and never came back must not keep a stream open forever.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if sub != nil && sub.idleFor() > conversationIdleTimeout {
					slog.Debug("closing an idle conversation stream", "channel", key.channel, "chat_id", key.chatID)
					cancel()
					return
				}
			}
		}
	}()

	for {
		if ctx.Err() != nil {
			return
		}
		connected, done, err := g.drainConversation(ctx, ch, key, agg, &lastEventID)
		if connected {
			everConnected = true
		}
		if done {
			return
		}
		// A subscription that has never managed to connect must not keep the
		// channel-wide fallback from delivering this conversation: release the
		// key so the fallback takes over instead of losing the reply.
		if !everConnected && time.Since(started) > conversationConnectGrace {
			slog.Warn("conversation stream never connected; falling back to the channel stream",
				"channel", key.channel, "chat_id", key.chatID, "error", err)
			return
		}
		if err != nil && ctx.Err() == nil {
			metrics.SSEReconnects.WithLabelValues(key.channel).Inc()
			slog.Warn("conversation stream ended, reconnecting", "channel", key.channel, "chat_id", key.chatID, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

// conversationConnectGrace bounds how long a subscription may keep the fallback
// away without ever connecting. A variable so tests can shrink it.
var conversationConnectGrace = 30 * time.Second

// conversationIdleTimeout closes a subscription that has seen no event for this
// long. It has to exceed the longest gap between chunks of a slow turn (a tool
// call can take minutes), which is why it is not shorter.
const conversationIdleTimeout = 10 * time.Minute

// conversationCursor asks the bus where the conversation currently ends. A new
// subscription starts there so it receives the coming reply instead of the
// previous turn's replay.
func (g *Gateway) conversationCursor(ctx context.Context, channelName, chatID string) (string, error) {
	url := g.busBaseURL() + "/api/v1/outbound/cursor?channel=" + channelName + "&chat_id=" + chatID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	if token := config.InternalToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cursor request answered %d", resp.StatusCode)
	}
	var payload struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&payload); err != nil {
		return "", err
	}
	return payload.ID, nil
}

// openStream dials the bus's outbound SSE for one conversation.
func (g *Gateway) openStream(ctx context.Context, channelName, chatID, lastEventID string) (*http.Response, error) {
	url := g.busBaseURL() + "/api/v1/outbound/stream?channel=" + channelName + "&chat_id=" + chatID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	if token := config.InternalToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return http.DefaultClient.Do(req)
}

// drainConversation reads one conversation's SSE until it ends. done=true means
// the conversation is finished (a `complete` arrived) and the subscription
// should close; the error is only for the reconnect decision.
func (g *Gateway) drainConversation(ctx context.Context, ch channel.Channel, key conversationKey, agg *chunkAggregator, lastEventID *string) (connected, done bool, err error) {
	resp, err := g.openStream(ctx, key.channel, key.chatID, *lastEventID)
	if err != nil {
		return false, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, false, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	slog.Debug("conversation stream subscribed", "channel", key.channel, "chat_id", key.chatID)

	scanner := bufio.NewScanner(io.LimitReader(resp.Body, 16<<20))
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
			if sub := g.subs.lookup(key.channel, key.chatID); sub != nil {
				sub.touch()
			}
			switch eventType {
			case "reset":
				// The bus could not resume from our cursor and restarted from
				// what it still has. Clear the cursor so the next reconnect
				// follows it rather than asking for the lost range again.
				slog.Warn("conversation stream reset", "channel", key.channel, "chat_id", key.chatID, "detail", eventData)
				*lastEventID = ""
			case "chunk":
				if eventID != "" {
					*lastEventID = eventID
				}
				g.dispatchEvent(ctx, ch, agg, eventType, eventData)
			case "complete":
				if eventID != "" {
					*lastEventID = eventID
				}
				g.dispatchEvent(ctx, ch, agg, eventType, eventData)
				return true, true, nil
			}
			eventType, eventID, eventData = "", "", ""
		}
	}
	return true, false, scanner.Err()
}
