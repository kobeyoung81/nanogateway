// Package portalclient reads the agent rows and the conversation projection the
// portal owns. The gateway still talks to the bus for messages; these two reads
// moved with their data.
package portalclient

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gitee.com/kobeyoung81/nanogateway/internal/config"
	"gitee.com/kobeyoung81/nanoprotocol"
)

// Client reads the portal's internal API.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New returns a client for the configured portal.
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Default returns the client for the deployment's portal.
func Default() *Client { return New(config.PortalURL(), config.InternalToken()) }

// ListAgents returns the agents belonging to a user, in the wire shape the
// browser consumes.
func (c *Client) ListAgents(userID string) ([]nanoprotocol.AgentInfo, error) {
	var out struct {
		Agents []nanoprotocol.AgentInfo `json:"agents"`
	}
	path := "/internal/v1/agents?user_id=" + url.QueryEscape(userID)
	if err := c.get(path, &out); err != nil {
		return nil, err
	}
	if out.Agents == nil {
		out.Agents = []nanoprotocol.AgentInfo{}
	}
	return out.Agents, nil
}

// SessionMeta is a conversation's metadata without its history.
type SessionMeta struct {
	SessionID string `json:"session_id"`
	AgentID   string `json:"agent_id"`
	AccountID string `json:"account_id"`
	ChatID    string `json:"chat_id"`
	Status    string `json:"status"`
}

// SessionMeta reads a conversation's identity and ownership. Nil means the
// session does not exist, which the resume path treats as "start a new one".
func (c *Client) SessionMeta(sessionID string) (*SessionMeta, error) {
	var out SessionMeta
	err := c.get("/internal/v1/sessions/"+url.PathEscape(sessionID), &out)
	if err != nil {
		var se *statusError
		if asStatus(err, &se) && se.code == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &out, nil
}

func (c *Client) get(path string, out any) error {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("portal %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusBadRequest {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &statusError{code: resp.StatusCode, body: strings.TrimSpace(string(detail))}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type statusError struct {
	code int
	body string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("portal answered %d: %s", e.code, e.body)
}

func asStatus(err error, target **statusError) bool {
	se, ok := err.(*statusError)
	if ok {
		*target = se
	}
	return ok
}
