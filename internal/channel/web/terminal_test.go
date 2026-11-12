package web

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gitee.com/kobeyoung81/nanogateway/internal/channel"
	"gitee.com/kobeyoung81/nanoprotocol"
	"github.com/gorilla/websocket"
)

// mockBusTerminal serves the bus HTTP plus a fake /ws/terminal bridge that
// echoes each received frame back as an output frame so round-trips are
// observable.
func mockBusTerminal(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/api/v1/rpc", func(w http.ResponseWriter, r *http.Request) {
		var msg nanoprotocol.Message
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		switch msg.Type {
		case nanoprotocol.TypeValidateToken:
			var req nanoprotocol.ValidateTokenRequest
			if err := msg.DecodePayload(&req); err != nil {
				http.Error(w, "bad token request", http.StatusBadRequest)
				return
			}
			valid := req.Token == "abc"
			resp, _ := nanoprotocol.NewMessage(nanoprotocol.TypeTokenValid, nanoprotocol.ValidateTokenResponse{
				Valid:  valid,
				UserID: "user-1",
				Role:   "user",
			})
			json.NewEncoder(w).Encode(resp)
		default:
			json.NewEncoder(w).Encode(nanoprotocol.NewErrorMessage("unsupported: " + msg.Type))
		}
	})

	mux.HandleFunc("/ws/terminal", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("user_id") != "user-1" || r.URL.Query().Get("agent_id") != "agent-1" {
			http.Error(w, "access denied", http.StatusForbidden)
			return
		}
		conn, err := websocket.Upgrade(w, r, nil, 1<<20, 1<<20)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var f terminalFrame
			if err := conn.ReadJSON(&f); err != nil {
				return
			}
			// Echo every frame back as output so the test can observe the relay.
			if err := conn.WriteJSON(terminalFrame{Type: "output", TerminalID: f.TerminalID, Data: []byte("relay:" + f.Type)}); err != nil {
				return
			}
		}
	})

	return httptest.NewServer(mux)
}

func startAdapter(t *testing.T, busURL string) *Adapter {
	t.Helper()
	addr := freePort(t)
	adapter := New(busURL, addr)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_ = adapter.Start(ctx, func(ctx context.Context, msg channel.InboundMessage) error { return nil })
	}()
	// Wait until the adapter is listening.
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("adapter not listening: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() { cancel(); _ = adapter.Stop() })
	return adapter
}

func TestAdapter_TerminalRoundTrip(t *testing.T) {
	bus := mockBusTerminal(t)
	defer bus.Close()

	adapter := startAdapter(t, bus.URL)

	wsURL := "ws://" + adapter.listenAddr + "/ws/agent/agent-1/terminal?token=abc"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial terminal ws: %v", err)
	}
	defer conn.Close()

	_ = conn.WriteJSON(terminalFrame{Type: "bind", TerminalID: "t1"})
	_ = conn.WriteJSON(terminalFrame{Type: "input", TerminalID: "t1", Data: []byte("ls\r")})

	var got terminalFrame
	if err := conn.ReadJSON(&got); err != nil {
		t.Fatalf("read output: %v", err)
	}
	if got.Type != "output" || got.TerminalID != "t1" {
		t.Fatalf("unexpected frame: %+v", got)
	}
	if !strings.Contains(string(got.Data), "bind") && !strings.Contains(string(got.Data), "input") {
		t.Fatalf("expected echoed frame data, got %q", got.Data)
	}
}

func TestAdapter_TerminalRejectsBadToken(t *testing.T) {
	bus := mockBusTerminal(t)
	defer bus.Close()

	adapter := startAdapter(t, bus.URL)

	wsURL := "ws://" + adapter.listenAddr + "/ws/agent/agent-1/terminal?token=bogus"
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		t.Fatal("expected dial to fail with bad token")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %v", resp)
	}
}

func TestToWSURL(t *testing.T) {
	cases := map[string]string{
		"http://nanobus:9000":  "ws://nanobus:9000",
		"https://nanobus:9000": "wss://nanobus:9000",
		"ws://nanobus:9000":    "ws://nanobus:9000",
		"wss://nanobus:9000":   "wss://nanobus:9000",
		"nanobus:9000":         "ws://nanobus:9000",
	}
	for in, want := range cases {
		if got := toWSURL(in); got != want {
			t.Errorf("toWSURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestTerminalURLs_ProvisionerFirstThenBus pins the rollout order: the
// provisioner owns tunnels now, and the bus stays as the fallback for containers
// whose image still dials it.
func TestTerminalURLs_ProvisionerFirstThenBus(t *testing.T) {
	t.Setenv("NANOSEC_PROVISIONER_URL", "http://runner.internal:8080")
	a := New("ws://bus.internal:9000", "")

	urls := a.terminalURLs("user-1", "agent-1")
	if len(urls) != 2 {
		t.Fatalf("expected provisioner then bus, got %v", urls)
	}
	if !strings.HasPrefix(urls[0], "ws://runner.internal:8080/ws/terminal?") {
		t.Fatalf("the first endpoint is not the provisioner: %q", urls[0])
	}
	if !strings.HasPrefix(urls[1], "ws://bus.internal:9000/ws/terminal?") {
		t.Fatalf("the fallback endpoint is not the bus: %q", urls[1])
	}
}

// TestDialTerminal_FallsBackWhenTheFirstRefuses: a 404 from the provisioner means
// "no tunnel registered here", and the bridge has to try the bus so a terminal
// keeps working for containers that have not been restarted onto the new image.
func TestDialTerminal_FallsBackWhenTheFirstRefuses(t *testing.T) {
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"tunnel is not registered"}`, http.StatusNotFound)
	}))
	defer refusing.Close()

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	accepting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer accepting.Close()

	conn, err := dialTerminal([]string{
		toWSURL(refusing.URL) + "/ws/terminal",
		toWSURL(accepting.URL) + "/ws/terminal",
	})
	if err != nil {
		t.Fatalf("dialTerminal: %v", err)
	}
	_ = conn.Close()
}

// TestDialTerminal_ReportsTheLastFailure keeps a total outage from looking like
// a successful dial.
func TestDialTerminal_ReportsTheLastFailure(t *testing.T) {
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusNotFound)
	}))
	defer refusing.Close()

	target := toWSURL(refusing.URL) + "/ws/terminal"
	if _, err := dialTerminal([]string{target, target}); err == nil {
		t.Fatal("dialTerminal reported success when both endpoints refused")
	}
}
