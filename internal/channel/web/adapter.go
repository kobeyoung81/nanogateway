// Package web implements the browser agent channel for the gateway.
//
// It exposes an HTTP/WebSocket server that authenticated browsers use to talk
// with agents. Messages flow: browser → gateway → bus → agent → bus (SSE) →
// gateway → browser, with incremental chunk streaming.
package web

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"gitee.com/kobeyoung81/nanogateway/internal/channel"
	"gitee.com/kobeyoung81/nanogateway/internal/config"
	"gitee.com/kobeyoung81/nanogateway/internal/gateway"
	"gitee.com/kobeyoung81/nanoprotocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// Adapter is the web agent channel adapter.
type Adapter struct {
	busURL     string
	listenAddr string
	upgrader   websocket.Upgrader
	mu         sync.Mutex
	// clients holds every browser connection bound to a chat. One chat can have
	// several: the same conversation opened in two tabs, on two devices, or a tab
	// whose duplicate has not been collapsed yet. Events go to all of them.
	clients   map[string]map[*client]struct{}
	onInbound channel.InboundHandler
	server    *http.Server
}

type client struct {
	chatID    string
	userID    string
	agentID   string
	sessionID string
	conn      *websocket.Conn
	mu        sync.Mutex
	done      chan struct{}
	doneOnce  sync.Once
}

// Heartbeat timing. The gateway pings the browser every pingInterval so the
// connection never sits idle past the proxy_read_timeout of nginx (60s default)
// in front of the channel. Browsers answer pings with pongs automatically; the
// pong handler resets the read deadline so dead peers are detected and dropped.
const (
	pongWait     = 75 * time.Second
	pingInterval = 45 * time.Second
	// maxWSMessageBytes caps a single inbound browser frame. It sits above the
	// bus content cap (16 MiB) to leave room for JSON framing while still
	// bounding memory a malicious client can force.
	maxWSMessageBytes = 24 << 20
)

// New creates a web agent adapter.
func New(busURL, listenAddr string) *Adapter {
	if listenAddr == "" {
		listenAddr = ":8080"
	}
	return &Adapter{
		busURL:     busURL,
		listenAddr: listenAddr,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		clients: make(map[string]map[*client]struct{}),
	}
}

// Name returns the channel name.
func (a *Adapter) Name() string { return "web" }

// wsMessage is a message sent from the browser over the WebSocket.
type wsMessage struct {
	Type     string                   `json:"type"`
	Content  string                   `json:"content"`
	Media    []nanoprotocol.MediaItem `json:"media,omitempty"`
	Metadata map[string]any           `json:"metadata,omitempty"`
}

// wsEvent is a message sent to the browser over the WebSocket.
type wsEvent struct {
	Type      string `json:"type"` // "ready" | "chunk" | "complete" | "error"
	ChatID    string `json:"chat_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	Kind      string `json:"kind,omitempty"` // content | thought | tool_use | tool_result | summary
	Content   string `json:"content,omitempty"`
	// Summary carries the compaction summary a finished turn applied (empty on
	// ordinary turns). It is the authoritative copy: a client that missed the
	// streamed summary chunks still shows the result without a reload.
	Summary     string `json:"summary,omitempty"`
	Error       string `json:"error,omitempty"`
	ErrorDetail string `json:"error_detail,omitempty"`
}

// Start begins serving the web agent HTTP endpoints.
func (a *Adapter) Start(ctx context.Context, handler channel.InboundHandler) error {
	a.onInbound = handler

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/agents", a.handleListAgents)
	mux.HandleFunc("GET /ws/agent/{agentID}", a.handleAgentWS)
	mux.HandleFunc("GET /ws/agent/{agentID}/terminal", a.handleAgentTerminalWS)

	a.server = &http.Server{
		Addr:    a.listenAddr,
		Handler: withCORS(mux),
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("web channel listening", "addr", a.listenAddr)
		if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = a.server.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		return err
	}
}

// Stop shuts down the HTTP server.
func (a *Adapter) Stop() error {
	if a.server != nil {
		return a.server.Close()
	}
	return nil
}

// Send delivers a complete outbound message to the browsers on this chat.
func (a *Adapter) Send(ctx context.Context, msg channel.OutboundMessage) error {
	return a.sendToChat(msg.ChatID, wsEvent{Type: "complete", ChatID: msg.ChatID, Content: msg.Content})
}

// SendChunk forwards a streaming chunk to the browsers on this chat.
func (a *Adapter) SendChunk(ctx context.Context, chunk nanoprotocol.OutboundChunk) error {
	return a.sendToChat(chunk.ChatID, wsEvent{
		Type:      "chunk",
		ChatID:    chunk.ChatID,
		AgentID:   chunk.AgentID,
		MessageID: chunk.MessageID,
		Kind:      chunk.Kind,
		Content:   chunk.Content,
	})
}

// SendComplete forwards the final assembled message to the browsers on this chat.
func (a *Adapter) SendComplete(ctx context.Context, complete nanoprotocol.OutboundComplete) error {
	return a.sendToChat(complete.ChatID, wsEvent{
		Type:        "complete",
		ChatID:      complete.ChatID,
		AgentID:     complete.AgentID,
		MessageID:   complete.MessageID,
		SessionID:   complete.SessionID,
		Content:     complete.FullContent,
		Summary:     complete.Summary,
		Error:       complete.Error,
		ErrorDetail: complete.ErrorDetail,
	})
}

// sendToChat writes an event to every connection bound to the chat. A connection
// whose write fails is closed and dropped, so one dead client cannot block the
// others (and its own read loop notices the close).
func (a *Adapter) sendToChat(chatID string, ev wsEvent) error {
	a.mu.Lock()
	targets := make([]*client, 0, len(a.clients[chatID]))
	for c := range a.clients[chatID] {
		targets = append(targets, c)
	}
	a.mu.Unlock()

	var firstErr error
	for _, c := range targets {
		if err := c.write(ev); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			c.close()
			a.unregister(c)
		}
	}
	return firstErr
}

func (c *client) write(ev wsEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteJSON(ev)
}

// handleListAgents returns the authenticated user's agents.
func (a *Adapter) handleListAgents(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r)
	if token == "" {
		http.Error(w, `{"error":"missing token"}`, http.StatusUnauthorized)
		return
	}

	client, err := gateway.NewBusClient(a.busURL)
	if err != nil {
		http.Error(w, `{"error":"bus unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	defer client.Close()

	validated, err := client.ValidateToken(token)
	if err != nil || !validated.Valid {
		http.Error(w, `{"error":"invalid token"}`, http.StatusUnauthorized)
		return
	}

	agents, err := client.ListAgents(validated.UserID)
	if err != nil {
		http.Error(w, `{"error":"list agents failed"}`, http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"agents": agents})
}

// handleAgentWS upgrades a browser connection into an agent session.
func (a *Adapter) handleAgentWS(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agentID")
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Error(w, "missing token", http.StatusUnauthorized)
		return
	}

	busClient, err := gateway.NewBusClient(a.busURL)
	if err != nil {
		http.Error(w, "bus unavailable", http.StatusServiceUnavailable)
		return
	}
	validated, err := busClient.ValidateToken(token)
	if err != nil || !validated.Valid || validated.UserID == "" {
		busClient.Close()
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	// Optional resume: when a session_id is supplied, reuse the persisted
	// conversation for this user+agent instead of starting a brand-new conversation.
	// Only metadata is fetched: the stored history can reach several MiB and the
	// check below needs four fields.
	sessionID := r.URL.Query().Get("session_id")
	// Whether the client asked to continue a stored conversation. Logged with the
	// connection because "resumed an unversioned session" and "sent no session and
	// was given a new one" print the same chat_id/session_id pair, which makes an
	// incident impossible to read from the log alone.
	resumed := sessionID != ""
	chatID := ""
	if sessionID != "" {
		rec, err := busClient.GetSessionMeta(sessionID)
		if err != nil {
			busClient.Close()
			slog.Warn("web session lookup failed", "session_id", sessionID, "error", err)
			http.Error(w, "session lookup failed", http.StatusServiceUnavailable)
			return
		}
		if rec == nil || rec.AgentID != agentID || rec.AccountID != validated.UserID || rec.Status == "archived" {
			busClient.Close()
			slog.Warn("web session resume denied", "session_id", sessionID, "agent_id", agentID, "user_id", validated.UserID)
			http.Error(w, "session not found or access denied", http.StatusForbidden)
			return
		}
		chatID = rec.ChatID
	}
	busClient.Close()

	conn, err := a.upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("ws upgrade failed", "error", err)
		return
	}

	if chatID == "" {
		chatID = uuid.New().String()
	}
	if sessionID == "" {
		sessionID = "web:" + chatID
	}
	c := &client{chatID: chatID, userID: validated.UserID, agentID: agentID, sessionID: sessionID, conn: conn, done: make(chan struct{})}
	a.register(c)
	defer a.unregister(c)
	defer c.close()

	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetReadLimit(maxWSMessageBytes)
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	go c.keepAlive()

	if err := c.write(wsEvent{Type: "ready", ChatID: chatID, SessionID: sessionID}); err != nil {
		return
	}
	slog.Info("web agent connected", "chat_id", chatID, "session_id", sessionID, "resumed", resumed, "agent_id", agentID, "user_id", validated.UserID)

	a.readLoop(c)
}

func (c *client) keepAlive() {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			c.mu.Lock()
			err := c.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
			c.mu.Unlock()
			if err != nil {
				slog.Warn("ws ping failed, closing", "chat_id", c.chatID, "error", err)
				c.close()
				return
			}
		}
	}
}

// close tears down the connection and stops the keepalive goroutine.
func (c *client) close() {
	c.doneOnce.Do(func() {
		close(c.done)
		_ = c.conn.Close()
	})
}

func (a *Adapter) readLoop(c *client) {
	for {
		var msg wsMessage
		if err := c.conn.ReadJSON(&msg); err != nil {
			return
		}
		switch msg.Type {
		case "message":
			if msg.Content == "" && len(msg.Media) == 0 {
				continue
			}
			a.dispatch(c, msg.Content, msg.Metadata, msg.Media)
		}
	}
}

func (a *Adapter) dispatch(c *client, content string, metadata map[string]any, media []nanoprotocol.MediaItem) {
	if a.onInbound == nil {
		return
	}
	inbound := channel.InboundMessage{
		Channel:   "web",
		ChatID:    c.chatID,
		SessionID: c.sessionID,
		MessageID: uuid.New().String(),
		Content:   content,
		Media:     media,
		Metadata:  metadata,
		AgentID:   c.agentID,
		UserID:    c.userID,
	}
	if err := a.onInbound(context.Background(), inbound); err != nil {
		slog.Warn("web inbound dispatch failed", "chat_id", c.chatID, "error", err)
		// Tell the browser the turn will never start (e.g. the message was
		// rejected as too large). Without this the optimistic "running"
		// indicator would spin forever with no completion.
		_ = c.write(wsEvent{
			Type:        "complete",
			ChatID:      c.chatID,
			AgentID:     c.agentID,
			Error:       "message_rejected",
			ErrorDetail: err.Error(),
		})
	}
}

func (a *Adapter) register(c *client) {
	a.mu.Lock()
	defer a.mu.Unlock()
	set := a.clients[c.chatID]
	if set == nil {
		set = make(map[*client]struct{})
		a.clients[c.chatID] = set
	}
	set[c] = struct{}{}
}

func (a *Adapter) unregister(c *client) {
	a.mu.Lock()
	defer a.mu.Unlock()
	set := a.clients[c.chatID]
	if set == nil {
		return
	}
	delete(set, c)
	if len(set) == 0 {
		delete(a.clients, c.chatID)
	}
}

// terminalFrame is the JSON envelope for terminal io on the browser <-> gateway
// WebSocket. It mirrors the gateway <-> bus bridge frame 1:1 so the adapter can
// pump frames between the two connections without inspecting payloads.
type terminalFrame struct {
	Type       string `json:"type"` // bind|input|resize|close | output|closed
	TerminalID string `json:"terminal_id"`
	Data       []byte `json:"data,omitempty"`
	Cols       int    `json:"cols,omitempty"`
	Rows       int    `json:"rows,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Message    string `json:"message,omitempty"`
}

// handleAgentTerminalWS is the browser terminal channel. After token validation
// the browser WebSocket is upgraded and bridged verbatim (frame-for-frame) to
// the bus's terminal bridge WebSocket /ws/terminal. The bus owns agent-ownership
// checks, PTY sessions, and output fan-out; the gateway is a transparent pump.
func (a *Adapter) handleAgentTerminalWS(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agentID")
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Error(w, "missing token", http.StatusUnauthorized)
		return
	}

	busClient, err := gateway.NewBusClient(a.busURL)
	if err != nil {
		http.Error(w, "bus unavailable", http.StatusServiceUnavailable)
		return
	}
	validated, err := busClient.ValidateToken(token)
	busClient.Close()
	if err != nil || !validated.Valid || validated.UserID == "" {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	browser, err := a.upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("terminal ws upgrade failed", "error", err)
		return
	}
	defer browser.Close()

	bus, _, err := websocket.DefaultDialer.Dial(a.terminalBusURL(validated.UserID, agentID), nil)
	if err != nil {
		slog.Warn("terminal bus dial failed", "user_id", validated.UserID, "agent_id", agentID, "error", err)
		_ = browser.WriteJSON(terminalFrame{Type: "error", Message: "agent terminal unavailable"})
		return
	}
	defer bus.Close()

	done := make(chan struct{})
	defer close(done)
	go a.terminalBrowserKeepalive(browser, done)

	browser.SetReadLimit(1 << 20)
	bus.SetReadLimit(1 << 20)

	// browser -> bus
	upErr := make(chan struct{})
	go func() {
		defer close(upErr)
		for {
			var f terminalFrame
			if err := browser.ReadJSON(&f); err != nil {
				return
			}
			_ = bus.WriteJSON(f)
		}
	}()

	// bus -> browser
	downErr := make(chan struct{})
	go func() {
		defer close(downErr)
		for {
			var f terminalFrame
			if err := bus.ReadJSON(&f); err != nil {
				return
			}
			if err := browser.WriteJSON(f); err != nil {
				return
			}
		}
	}()

	select {
	case <-upErr:
	case <-downErr:
	}
}

// terminalBusURL builds the bus terminal-bridge WebSocket URL. The token goes
// in the query because a WebSocket handshake built from a URL has no header to
// put it in.
func (a *Adapter) terminalBusURL(userID, agentID string) string {
	q := url.Values{"user_id": {userID}, "agent_id": {agentID}}
	if token := config.InternalToken(); token != "" {
		q.Set("token", token)
	}
	return toWSURL(a.busURL) + "/ws/terminal?" + q.Encode()
}

// terminalBrowserKeepalive pings the browser while the terminal is bridged.
func (a *Adapter) terminalBrowserKeepalive(conn *websocket.Conn, done <-chan struct{}) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				_ = conn.Close()
				return
			}
		}
	}
}

// toWSURL converts an http(s):// base URL to its ws(s):// form.
func toWSURL(raw string) string {
	switch {
	case strings.HasPrefix(raw, "wss://"):
		return raw
	case strings.HasPrefix(raw, "https://"):
		return "wss://" + strings.TrimPrefix(raw, "https://")
	case strings.HasPrefix(raw, "ws://"):
		return raw
	default:
		return "ws://" + strings.TrimPrefix(raw, "http://")
	}
}

func bearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(auth) > len(prefix) && auth[:len(prefix)] == prefix {
		return auth[len(prefix):]
	}
	return r.URL.Query().Get("token")
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// withCORS allows browser clients served from other origins (e.g., the portal)
// to call the web agent HTTP endpoints.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
