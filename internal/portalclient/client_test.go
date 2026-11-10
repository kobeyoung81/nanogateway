package portalclient

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The resume check asks the portal for metadata only, and ownership is decided
// from what comes back.
func TestSessionMetaReadsIdentityWithoutTheHistory(t *testing.T) {
	var gotPath, gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotToken = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"session_id":"web:chat-1","agent_id":"agent-1","account_id":"user-1","chat_id":"chat-1","status":"active"}`)
	}))
	defer srv.Close()

	rec, err := New(srv.URL, "s3cret").SessionMeta("web:chat-1")
	if err != nil {
		t.Fatalf("SessionMeta: %v", err)
	}
	if gotPath != "/internal/v1/sessions/web:chat-1" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotToken != "Bearer s3cret" {
		t.Fatalf("token = %q", gotToken)
	}
	if rec == nil || rec.ChatID != "chat-1" || rec.AgentID != "agent-1" || rec.AccountID != "user-1" {
		t.Fatalf("decoded meta = %+v", rec)
	}
}

// A missing conversation is not an error: the caller starts a new one.
func TestSessionMetaOfAMissingSessionIsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	rec, err := New(srv.URL, "").SessionMeta("web:missing")
	if err != nil {
		t.Fatalf("SessionMeta: %v", err)
	}
	if rec != nil {
		t.Fatalf("expected nil for a missing session, got %+v", rec)
	}
}

// The gateway serves this list to the browser, so the shape has to be the wire
// one — session agents and the default mode included.
func TestListAgentsKeepsTheWireShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("user_id") != "user-1" {
			t.Errorf("user_id = %q", r.URL.Query().Get("user_id"))
		}
		_, _ = io.WriteString(w, `{"agents":[{"id":"a1","user_id":"user-1","display_name":"A","status":"running","session_agents":["build"],"default_agent":"build"}]}`)
	}))
	defer srv.Close()

	agents, err := New(srv.URL, "").ListAgents("user-1")
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents) != 1 || agents[0].ID != "a1" || agents[0].DefaultAgent != "build" {
		t.Fatalf("agents = %+v", agents)
	}
	if strings.TrimSpace(agents[0].DisplayName) != "A" {
		t.Fatalf("display name lost: %+v", agents[0])
	}
}
