package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"gitee.com/kobeyoung81/nanoprotocol"
)

// TestGetSessionMetaRequestsMetadataOnly pins the resume path's contract: the
// gateway must ask the bus for metadata, never for the (multi-MiB) history, and
// must decode what comes back.
func TestGetSessionMetaRequestsMetadataOnly(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		_, _ = io.WriteString(w, `{"session_id":"web:chat-1","agent_id":"agent-1",
			"account_id":"user-1","channel":"web","chat_id":"chat-1","status":"active"}`)
	}))
	defer srv.Close()

	client, err := NewBusClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	rec, err := client.GetSessionMeta("web:chat-1")
	if err != nil {
		t.Fatalf("GetSessionMeta: %v", err)
	}
	if gotPath != "/api/v1/sessions/web:chat-1" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotQuery != "meta=1" {
		t.Fatalf("query = %q, want meta=1 (the blob must not be requested)", gotQuery)
	}
	if rec == nil || rec.ChatID != "chat-1" || rec.AgentID != "agent-1" || rec.AccountID != "user-1" {
		t.Fatalf("unexpected record: %+v", rec)
	}
	if rec.Messages != "" {
		t.Fatalf("metadata response must not carry messages, got %d bytes", len(rec.Messages))
	}
}

// TestGetSessionStillRequestsTheFullRow guards the other half of the contract:
// the history path keeps asking for the whole session.
func TestGetSessionStillRequestsTheFullRow(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = io.WriteString(w, `{"session_id":"web:chat-1","chat_id":"chat-1","messages":"[]"}`)
	}))
	defer srv.Close()

	client, err := NewBusClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	if _, err := client.GetSession("web:chat-1"); err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if gotQuery != "" {
		t.Fatalf("query = %q, want no query", gotQuery)
	}
}

// The bus authenticates its internal endpoints, so every request the gateway
// makes has to carry the shared token — including the plain http.Get/Post paths
// that do not build a request explicitly.
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

	if _, err := client.GetSessionMeta("web:chat-1"); err != nil {
		t.Fatalf("GetSessionMeta: %v", err)
	}
	if err := client.PostInbound(nanoprotocol.InboundMessage{
		UserID: "u", SessionID: "web:c", Channel: "web", ChatID: "c", Content: "hi",
	}); err != nil {
		t.Fatalf("PostInbound: %v", err)
	}

	if len(seen) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(seen))
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

	if _, err := client.GetSessionMeta("web:chat-1"); err != nil {
		t.Fatalf("GetSessionMeta: %v", err)
	}
	if header != "" {
		t.Errorf("Authorization = %q, want none", header)
	}
}
