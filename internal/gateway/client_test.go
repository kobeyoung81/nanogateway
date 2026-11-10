package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"gitee.com/kobeyoung81/nanoprotocol"
)

func TestBusClientSendsTheInternalToken(t *testing.T) {
	t.Setenv("NANOSEC_BUS_INTERNAL_TOKEN", "s3cret")

	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, `{"session_id":"web:chat-1","chat_id":"chat-1","messages":"[]"}`)
	}))
	defer srv.Close()

	client, err := NewBusClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	if err := client.PostInbound(nanoprotocol.InboundMessage{
		UserID: "u", SessionID: "web:c", Channel: "web", ChatID: "c", Content: "hi",
	}); err != nil {
		t.Fatalf("PostInbound: %v", err)
	}

	if len(seen) != 1 {
		t.Fatalf("expected 1 request, got %d", len(seen))
	}
	for i, header := range seen {
		if header != "Bearer s3cret" {
			t.Errorf("request %d: Authorization = %q, want the configured token", i, header)
		}
	}
}

// No token configured means nothing to send; the bus rejects such a caller and
// the client must not invent a header.
func TestBusClientWithoutTokenSendsNoAuthorization(t *testing.T) {
	t.Setenv("NANOSEC_BUS_INTERNAL_TOKEN", "")

	var header string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"session_id":"web:chat-1","chat_id":"chat-1"}`)
	}))
	defer srv.Close()

	client, err := NewBusClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	if err := client.PostInbound(nanoprotocol.InboundMessage{
		UserID: "u", SessionID: "web:c", Channel: "web", ChatID: "c", Content: "hi",
	}); err != nil {
		t.Fatalf("PostInbound: %v", err)
	}
	if header != "" {
		t.Errorf("Authorization = %q, want none", header)
	}
}
