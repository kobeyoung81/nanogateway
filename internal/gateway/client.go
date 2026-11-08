package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gitee.com/kobeyoung81/nanogateway/internal/config"
	"gitee.com/kobeyoung81/nanoprotocol"
)

// BusClient handles communication with the central bus service over HTTP.
type BusClient struct {
	baseURL string
	http    *http.Client
}

// busTransport adds the shared internal token to every request the gateway
// makes: the bus authenticates its internal endpoints, and the gateway is one
// of its callers. It wraps the transport instead of each call site so no
// request builder can forget it.
type busTransport struct {
	base  http.RoundTripper
	token string
}

func (t busTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.token != "" && req.Header.Get("Authorization") == "" {
		// Clone: RoundTrip must not mutate the caller's request.
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	return t.base.RoundTrip(req)
}

// NewBusClient creates a new client connected to the bus.
// The URL may be ws://, wss://, http:// or https:// — WebSocket schemes are normalized.
func NewBusClient(rawURL string) (*BusClient, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse bus url: %w", err)
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	case "http", "https":
	default:
		return nil, fmt.Errorf("unsupported bus url scheme: %s", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/")

	return &BusClient{
		baseURL: u.String(),
		http: &http.Client{
			Timeout:   30 * time.Second,
			Transport: busTransport{base: http.DefaultTransport, token: config.InternalToken()},
		},
	}, nil
}

// postJSON performs a JSON POST to the bus and decodes the response into out.
// When status is 204, out is left untouched and a nil error is returned.
func (c *BusClient) postJSON(path string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	resp, err := c.http.Post(c.baseURL+path, "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("post %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("bus %s failed (%d): %s", path, resp.StatusCode, respBody)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode %s response: %w", path, err)
		}
	}
	return nil
}

// PostRaw performs a raw HTTP POST and returns the response status and body.
// Used for reverse-proxying requests (e.g., auth login) through the bus.
func (c *BusClient) PostRaw(path string, body []byte) (int, []byte, error) {
	resp, err := c.http.Post(c.baseURL+path, "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("post %s: %w", path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("read %s response: %w", path, err)
	}
	return resp.StatusCode, b, nil
}

// rpc sends a nanoprotocol envelope to the bus and returns the response message.
func (c *BusClient) rpc(msg nanoprotocol.Message) (*nanoprotocol.Message, error) {
	var resp nanoprotocol.Message
	if err := c.postJSON("/api/v1/rpc", msg, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ResolveUser asks the bus to resolve a platform identity to a user ID and binding ID.
func (c *BusClient) ResolveUser(channel, senderID string) (*nanoprotocol.ResolveUserResponse, error) {
	var resp struct {
		UserID    string `json:"user_id"`
		BindingID int64  `json:"binding_id"`
		IsNew     bool   `json:"is_new"`
		Status    string `json:"status"`
	}
	if err := c.postJSON("/api/v1/users/resolve", nanoprotocol.ResolveUserRequest{
		Channel:  channel,
		SenderID: senderID,
	}, &resp); err != nil {
		return nil, err
	}

	slog.Debug("gw bus client: resolved user", "channel", channel, "sender", senderID, "user_id", resp.UserID, "binding_id", resp.BindingID)
	return &nanoprotocol.ResolveUserResponse{
		UserID:    resp.UserID,
		BindingID: resp.BindingID,
		IsNew:     resp.IsNew,
		Status:    resp.Status,
	}, nil
}

// GetBindingAgent asks the bus which agent a channel binding talks to by default.
func (c *BusClient) GetBindingAgent(bindingID int64) (string, error) {
	msg, err := nanoprotocol.NewMessage(nanoprotocol.TypeGetBindingAgent, nanoprotocol.GetBindingAgentRequest{
		BindingID: bindingID,
	})
	if err != nil {
		return "", err
	}
	resp, err := c.rpc(msg)
	if err != nil {
		return "", err
	}
	if resp.Type == nanoprotocol.TypeError {
		return "", fmt.Errorf("get binding agent: %s", resp.Error)
	}
	var result nanoprotocol.GetBindingAgentResponse
	if err := resp.DecodePayload(&result); err != nil {
		return "", fmt.Errorf("decode binding agent response: %w", err)
	}
	return result.AgentID, nil
}

// SetBindingAgent assigns the default agent for a channel binding.
func (c *BusClient) SetBindingAgent(bindingID int64, agentID string) error {
	msg, err := nanoprotocol.NewMessage(nanoprotocol.TypeSetBindingAgent, nanoprotocol.SetBindingAgentRequest{
		BindingID: bindingID,
		AgentID:   agentID,
	})
	if err != nil {
		return err
	}
	resp, err := c.rpc(msg)
	if err != nil {
		return err
	}
	if resp.Type == nanoprotocol.TypeError {
		return fmt.Errorf("set binding agent: %s", resp.Error)
	}
	return nil
}

// ValidateToken asks the bus to validate a JWT (delegated to nanoauth).
func (c *BusClient) ValidateToken(token string) (*nanoprotocol.ValidateTokenResponse, error) {
	msg, err := nanoprotocol.NewMessage(nanoprotocol.TypeValidateToken, nanoprotocol.ValidateTokenRequest{Token: token})
	if err != nil {
		return nil, err
	}
	resp, err := c.rpc(msg)
	if err != nil {
		return nil, err
	}
	if resp.Type == nanoprotocol.TypeError {
		return nil, fmt.Errorf("bus error: %s", resp.Error)
	}

	var out nanoprotocol.ValidateTokenResponse
	if err := resp.DecodePayload(&out); err != nil {
		return nil, fmt.Errorf("decode validate response: %w", err)
	}
	return &out, nil
}

// ListAgents asks the bus for the agents belonging to a user.
func (c *BusClient) ListAgents(userID string) ([]nanoprotocol.AgentInfo, error) {
	msg, err := nanoprotocol.NewMessage(nanoprotocol.TypeListAgents, nanoprotocol.ListAgentsRequest{UserID: userID})
	if err != nil {
		return nil, err
	}
	resp, err := c.rpc(msg)
	if err != nil {
		return nil, err
	}
	if resp.Type == nanoprotocol.TypeError {
		return nil, fmt.Errorf("bus error: %s", resp.Error)
	}

	var out nanoprotocol.ListAgentsResponse
	if err := resp.DecodePayload(&out); err != nil {
		return nil, fmt.Errorf("decode list agents response: %w", err)
	}
	return out.Agents, nil
}

// SessionRecord is the persisted session shape returned by the bus REST API.
type SessionRecord struct {
	SessionID string `json:"session_id"`
	AgentID   string `json:"agent_id"`
	AccountID string `json:"account_id"`
	Channel   string `json:"channel"`
	ChatID    string `json:"chat_id"`
	Status    string `json:"status"`
	Messages  string `json:"messages"`
	Summary   string `json:"summary"`
}

// GetSession fetches a persisted session by ID. Returns nil when not found.
func (c *BusClient) GetSession(sessionID string) (*SessionRecord, error) {
	return c.getSession(sessionID, "")
}

// GetSessionMeta fetches a session's metadata without its message history. The
// gateway only needs identity and ownership to resume a conversation, and a
// stored conversation can reach several MiB, so the resume path uses this
// instead of pulling (and decoding) the whole blob.
func (c *BusClient) GetSessionMeta(sessionID string) (*SessionRecord, error) {
	return c.getSession(sessionID, "?meta=1")
}

func (c *BusClient) getSession(sessionID, query string) (*SessionRecord, error) {
	resp, err := c.http.Get(c.baseURL + "/api/v1/sessions/" + url.PathEscape(sessionID) + query)
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("bus get session failed (%d): %s", resp.StatusCode, body)
	}

	var rec SessionRecord
	if err := json.NewDecoder(resp.Body).Decode(&rec); err != nil {
		return nil, fmt.Errorf("decode get session response: %w", err)
	}
	return &rec, nil
}

// PostInbound sends a message from a user to the bus.
func (c *BusClient) PostInbound(msg nanoprotocol.InboundMessage) error {
	slog.Debug("gw bus client: posting inbound", "user_id", msg.UserID, "channel", msg.Channel)
	return c.postJSON("/api/v1/inbound", msg, nil)
}

// PollOutbound asks the bus for any pending messages for a channel.
func (c *BusClient) PollOutbound(channel, gatewayID string) (*nanoprotocol.OutboundMessage, error) {
	msg, err := nanoprotocol.NewMessage(nanoprotocol.TypePollOutbound, nanoprotocol.PollOutboundRequest{
		Channel:   channel,
		GatewayID: gatewayID,
	})
	if err != nil {
		return nil, err
	}

	resp, err := c.rpc(msg)
	if err != nil {
		return nil, err
	}
	if resp.Type == nanoprotocol.TypeError {
		return nil, fmt.Errorf("bus error: %s", resp.Error)
	}
	if resp.Payload == nil {
		return nil, nil
	}

	var outbound nanoprotocol.OutboundMessage
	if err := resp.DecodePayload(&outbound); err != nil {
		return nil, fmt.Errorf("decode outbound message: %w", err)
	}

	slog.Debug("gw bus client: polled outbound message", "channel", channel, "chat_id", outbound.ChatID)
	return &outbound, nil
}

// Close releases idle connections.
func (c *BusClient) Close() error {
	if c.http != nil {
		c.http.CloseIdleConnections()
	}
	return nil
}
