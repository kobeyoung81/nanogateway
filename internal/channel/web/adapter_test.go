package web

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"gitee.com/kobeyoung81/nanogateway/internal/channel"
	"gitee.com/kobeyoung81/nanoprotocol"
	"github.com/gorilla/websocket"
)

// mockBusRPC serves the bus /api/v1/rpc endpoint for token validation and
// agent listing so the web adapter can be tested without a real bus.
func mockBusRPC(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/rpc" {
			http.NotFound(w, r)
			return
		}
		var msg nanoprotocol.Message
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		switch msg.Type {
		case nanoprotocol.TypeValidateToken:
			resp, _ := nanoprotocol.NewMessage(nanoprotocol.TypeTokenValid, nanoprotocol.ValidateTokenResponse{
				Valid:  true,
				UserID: "user-1",
				Role:   "user",
			})
			json.NewEncoder(w).Encode(resp)
		case nanoprotocol.TypeListAgents:
			resp, _ := nanoprotocol.NewMessage(nanoprotocol.TypeAgentsListed, nanoprotocol.ListAgentsResponse{
				Agents: []nanoprotocol.AgentInfo{
					{ID: "assistant", UserID: "user-1", DisplayName: "Assistant", Status: "active", GroupID: "group-1"},
				},
			})
			json.NewEncoder(w).Encode(resp)
		default:
			json.NewEncoder(w).Encode(nanoprotocol.NewErrorMessage("unsupported: " + msg.Type))
		}
	}))
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func TestAdapter_WebSocketAgentFlow(t *testing.T) {
	bus := mockBusRPC(t)
	defer bus.Close()

	addr := freePort(t)
	adapter := New(bus.URL, addr)

	var captured channel.InboundMessage
	handler := func(ctx context.Context, msg channel.InboundMessage) error {
		captured = msg
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go adapter.Start(ctx, handler)

	// Wait for the adapter to listen.
	time.Sleep(100 * time.Millisecond)

	wsURL := "ws://" + addr + "/ws/agent/assistant?token=test-token"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	defer conn.Close()

	// Read the ready event.
	var ready wsEvent
	if err := conn.ReadJSON(&ready); err != nil {
		t.Fatalf("read ready: %v", err)
	}
	if ready.Type != "ready" || ready.ChatID == "" {
		t.Fatalf("unexpected ready event: %+v", ready)
	}

	// Send a user message.
	if err := conn.WriteJSON(wsMessage{Type: "message", Content: "Hello agent"}); err != nil {
		t.Fatalf("write message: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for captured.Content == "" {
		if time.Now().After(deadline) {
			t.Fatal("timeout waiting for inbound message")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if captured.Content != "Hello agent" {
		t.Errorf("content = %q, want %q", captured.Content, "Hello agent")
	}
	if captured.UserID != "user-1" {
		t.Errorf("userID = %q, want user-1", captured.UserID)
	}
	if captured.AgentID != "assistant" {
		t.Errorf("agentID = %q, want assistant", captured.AgentID)
	}
	if captured.Channel != "web" {
		t.Errorf("channel = %q, want web", captured.Channel)
	}
}

func TestAdapter_ListAgents(t *testing.T) {
	bus := mockBusRPC(t)
	defer bus.Close()

	addr := freePort(t)
	adapter := New(bus.URL, addr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go adapter.Start(ctx, func(ctx context.Context, msg channel.InboundMessage) error { return nil })
	time.Sleep(100 * time.Millisecond)

	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/api/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer some-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET agents: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body struct {
		Agents []nanoprotocol.AgentInfo `json:"agents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Agents) != 1 || body.Agents[0].ID != "assistant" {
		t.Fatalf("unexpected agents: %+v", body.Agents)
	}
	// The grouping rides through untouched: the gateway forwards the wire type
	// rather than re-shaping it, so a browser sees the same group_id the bus
	// stored.
	if body.Agents[0].GroupID != "group-1" {
		t.Fatalf("group_id was not forwarded: %+v", body.Agents[0])
	}
}

func TestAdapter_ListAgents_Unauthorized(t *testing.T) {
	bus := mockBusRPC(t)
	defer bus.Close()

	addr := freePort(t)
	adapter := New(bus.URL, addr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go adapter.Start(ctx, func(ctx context.Context, msg channel.InboundMessage) error { return nil })
	time.Sleep(100 * time.Millisecond)

	resp, err := http.Get("http://" + addr + "/api/v1/agents")
	if err != nil {
		t.Fatalf("GET agents: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// The gateway used to expose POST /auth/v1/login, which forwarded to a nanoauth
// route that does not exist. The portal calls /auth/v1/token through the bus, so
// the endpoint is gone rather than proxied.
func TestAdapter_LoginEndpointIsRemoved(t *testing.T) {
	bus := mockBusRPC(t)
	defer bus.Close()

	addr := freePort(t)
	adapter := New(bus.URL, addr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go adapter.Start(ctx, func(ctx context.Context, msg channel.InboundMessage) error { return nil })
	time.Sleep(100 * time.Millisecond)

	resp, err := http.Post("http://"+addr+"/auth/v1/login", "application/json",
		strings.NewReader(`{"email":"alice@corp.local","password":"alicepass"}`))
	if err != nil {
		t.Fatalf("POST login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// mockBusWithSession serves token validation over /api/v1/rpc and a persisted
// session over GET /api/v1/sessions/{id}.
func mockBusWithSession(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/rpc" {
			resp, _ := nanoprotocol.NewMessage(nanoprotocol.TypeTokenValid, nanoprotocol.ValidateTokenResponse{
				Valid:  true,
				UserID: "user-1",
				Role:   "user",
			})
			json.NewEncoder(w).Encode(resp)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/v1/sessions/") {
			sessionID := strings.TrimPrefix(r.URL.Path, "/api/v1/sessions/")
			if sessionID != "web:chat-123" {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"session_id": sessionID,
				"agent_id":   "assistant",
				"account_id": "user-1",
				"channel":    "web",
				"chat_id":    "chat-123",
				"status":     "active",
				"messages":   "[]",
				"summary":    "",
			})
			return
		}
		http.NotFound(w, r)
	}))
}

func TestAdapter_WebSocketResumeSession(t *testing.T) {
	bus := mockBusWithSession(t)
	defer bus.Close()

	addr := freePort(t)
	adapter := New(bus.URL, addr)

	var captured channel.InboundMessage
	handler := func(ctx context.Context, msg channel.InboundMessage) error {
		captured = msg
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go adapter.Start(ctx, handler)
	time.Sleep(100 * time.Millisecond)

	wsURL := "ws://" + addr + "/ws/agent/assistant?token=test-token&session_id=" + url.QueryEscape("web:chat-123")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	defer conn.Close()

	var ready wsEvent
	if err := conn.ReadJSON(&ready); err != nil {
		t.Fatalf("read ready: %v", err)
	}
	if ready.Type != "ready" || ready.ChatID != "chat-123" || ready.SessionID != "web:chat-123" {
		t.Fatalf("unexpected ready event: %+v", ready)
	}

	if err := conn.WriteJSON(wsMessage{Type: "message", Content: "Resume me"}); err != nil {
		t.Fatalf("write message: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for captured.Content == "" {
		if time.Now().After(deadline) {
			t.Fatal("timeout waiting for inbound message")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if captured.Content != "Resume me" {
		t.Errorf("content = %q, want %q", captured.Content, "Resume me")
	}
	if captured.SessionID != "web:chat-123" {
		t.Errorf("sessionID = %q, want web:chat-123", captured.SessionID)
	}
	if captured.ChatID != "chat-123" {
		t.Errorf("chatID = %q, want chat-123", captured.ChatID)
	}
}

func TestAdapter_WebSocketResumeSessionDenied(t *testing.T) {
	bus := mockBusWithSession(t)
	defer bus.Close()

	addr := freePort(t)
	adapter := New(bus.URL, addr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go adapter.Start(ctx, func(ctx context.Context, msg channel.InboundMessage) error { return nil })
	time.Sleep(100 * time.Millisecond)

	// Session belongs to another user (server rejects before upgrade).
	wsURL := "ws://" + addr + "/ws/agent/assistant?token=test-token&session_id=" + url.QueryEscape("web:other-chat")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		conn.Close()
		t.Fatal("expected handshake failure for foreign session, got connection")
	}
}

// TestAdapter_WebSocketFanOutToAllConnections pins the multi-connection
// behaviour: one conversation can be open in more than one browser connection
// (two tabs, two devices, or a duplicate tab that has not been collapsed yet),
// and every one of them must receive the stream. Routing to a single
// "last registered" connection left the others silent — in the UI their turn
// looked like it was still running forever because the completion never arrived.
func TestAdapter_WebSocketFanOutToAllConnections(t *testing.T) {
	bus := mockBusWithSession(t)
	defer bus.Close()

	addr := freePort(t)
	adapter := New(bus.URL, addr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go adapter.Start(ctx, func(ctx context.Context, msg channel.InboundMessage) error { return nil })
	time.Sleep(100 * time.Millisecond)

	wsURL := "ws://" + addr + "/ws/agent/assistant?token=test-token&session_id=" + url.QueryEscape("web:chat-123")
	conns := make([]*websocket.Conn, 0, 2)
	for i := 0; i < 2; i++ {
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatalf("dial connection %d: %v", i, err)
		}
		defer conn.Close()
		conns = append(conns, conn)
	}
	for i, conn := range conns {
		var ready wsEvent
		if err := conn.ReadJSON(&ready); err != nil {
			t.Fatalf("connection %d ready: %v", i, err)
		}
		if ready.Type != "ready" || ready.ChatID != "chat-123" {
			t.Fatalf("connection %d ready = %+v, want ready for chat-123", i, ready)
		}
	}

	if err := adapter.SendChunk(ctx, nanoprotocol.OutboundChunk{
		MessageID: "m1",
		AgentID:   "assistant",
		UserID:    "user-1",
		Channel:   "web",
		ChatID:    "chat-123",
		SessionID: "web:chat-123",
		Kind:      nanoprotocol.ChunkKindContent,
		Content:   "hello",
	}); err != nil {
		t.Fatalf("send chunk: %v", err)
	}
	if err := adapter.SendComplete(ctx, nanoprotocol.OutboundComplete{
		MessageID:   "m1",
		AgentID:     "assistant",
		UserID:      "user-1",
		Channel:     "web",
		ChatID:      "chat-123",
		SessionID:   "web:chat-123",
		FullContent: "hello",
	}); err != nil {
		t.Fatalf("send complete: %v", err)
	}

	for i, conn := range conns {
		if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatalf("connection %d deadline: %v", i, err)
		}
		var chunk wsEvent
		if err := conn.ReadJSON(&chunk); err != nil {
			t.Fatalf("connection %d chunk: %v", i, err)
		}
		if chunk.Type != "chunk" || chunk.Content != "hello" {
			t.Fatalf("connection %d chunk = %+v, want a chunk carrying hello", i, chunk)
		}
		var complete wsEvent
		if err := conn.ReadJSON(&complete); err != nil {
			t.Fatalf("connection %d complete: %v", i, err)
		}
		if complete.Type != "complete" || complete.Content != "hello" {
			t.Fatalf("connection %d complete = %+v, want a completion carrying hello", i, complete)
		}
	}
}

// TestAdapter_CompleteCarriesCompactionSummary pins the summary a completion may
// carry: a compaction that ran during the turn puts its summary on the complete
// event, which is the only copy that reaches a browser whose stream missed the
// summary chunks. It used to be dropped here, so the divider stayed empty until
// the page was reloaded.
func TestAdapter_CompleteCarriesCompactionSummary(t *testing.T) {
	bus := mockBusWithSession(t)
	defer bus.Close()

	addr := freePort(t)
	adapter := New(bus.URL, addr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go adapter.Start(ctx, func(ctx context.Context, msg channel.InboundMessage) error { return nil })
	time.Sleep(100 * time.Millisecond)

	wsURL := "ws://" + addr + "/ws/agent/assistant?token=test-token&session_id=" + url.QueryEscape("web:chat-123")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	var ready wsEvent
	if err := conn.ReadJSON(&ready); err != nil {
		t.Fatalf("ready: %v", err)
	}

	if err := adapter.SendComplete(ctx, nanoprotocol.OutboundComplete{
		MessageID: "m1",
		AgentID:   "assistant",
		UserID:    "user-1",
		Channel:   "web",
		ChatID:    "chat-123",
		SessionID: "web:chat-123",
		Summary:   "COMPACTED",
	}); err != nil {
		t.Fatalf("send complete: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	var complete wsEvent
	if err := conn.ReadJSON(&complete); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if complete.Type != "complete" || complete.Summary != "COMPACTED" {
		t.Fatalf("complete = %+v, want a completion carrying the compaction summary", complete)
	}
}

// The bus's terminal bridge is an internal endpoint, and a WebSocket handshake
// built from a URL has no header, so the token travels as a query parameter.
func TestTerminalBusURLCarriesTheInternalToken(t *testing.T) {
	t.Setenv("NANOSEC_BUS_INTERNAL_TOKEN", "s3cret")
	adapter := &Adapter{busURL: "ws://nanobus:9000"}

	parsed, err := url.Parse(adapter.terminalBusURL("user-1", "agent-1"))
	if err != nil {
		t.Fatalf("terminalBusURL: %v", err)
	}
	if parsed.Path != "/ws/terminal" {
		t.Errorf("path = %q", parsed.Path)
	}
	if got := parsed.Query().Get("token"); got != "s3cret" {
		t.Errorf("token = %q, want the configured token", got)
	}
	if got := parsed.Query().Get("user_id"); got != "user-1" {
		t.Errorf("user_id = %q", got)
	}
	if got := parsed.Query().Get("agent_id"); got != "agent-1" {
		t.Errorf("agent_id = %q", got)
	}
}
