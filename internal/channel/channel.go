package channel

import (
	"context"

	"gitee.com/kobeyoung81/nanoprotocol"
)

// InboundMessage represents a message received from a platform (e.g., CLI, Telegram).
type InboundMessage struct {
	Channel   string
	SenderID  string
	ChatID    string
	MessageID string
	Content   string
	Media     []nanoprotocol.MediaItem
	Metadata  map[string]any
	// AgentID routes the message to a specific agent when set.
	AgentID string
	// UserID is the authenticated user ID for channels that authenticate users
	// directly (e.g., web). When set, the gateway skips sender resolution.
	UserID string
	// SessionID is the exact session to continue. When empty, the gateway
	// derives one from the channel and chat ID.
	SessionID string
}

// OutboundMessage represents a message to be sent to a platform.
type OutboundMessage struct {
	ChatID  string
	Content string
	ReplyTo string
	Media   []nanoprotocol.MediaItem
}

// InboundHandler is a function that processes incoming messages from a channel.
// It is typically the Gateway's handleInbound method.
type InboundHandler func(context.Context, InboundMessage) error

// Channel is the interface that all platform adapters must implement.
type Channel interface {
	// Name returns the unique name of the channel (e.g., "cli", "telegram").
	Name() string

	// Start begins listening for messages on the channel.
	// It should block until the context is cancelled or a fatal error occurs.
	// It calls handler for each received message.
	Start(ctx context.Context, handler InboundHandler) error

	// Send sends a message to the platform.
	Send(ctx context.Context, msg OutboundMessage) error

	// Stop gracefully shuts down the channel.
	Stop() error
}

// StreamSender is an optional interface for channels that support live
// streaming delivery of outbound chunks (e.g., the browser WebSocket channel).
type StreamSender interface {
	// SendChunk forwards a single streaming chunk to the platform.
	SendChunk(ctx context.Context, chunk nanoprotocol.OutboundChunk) error
	// SendComplete notifies the platform that a streaming turn has finished.
	SendComplete(ctx context.Context, complete nanoprotocol.OutboundComplete) error
}
