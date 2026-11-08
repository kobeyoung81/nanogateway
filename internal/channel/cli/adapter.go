package cli

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"gitee.com/kobeyoung81/nanogateway/internal/channel"
)

// Adapter implements the Channel interface for the command line.
type Adapter struct {
	userID  string
	reader  *bufio.Reader
	oneShot string
}

// New creates a new CLI adapter.
func New(userID string) *Adapter {
	return &Adapter{
		userID: userID,
		reader: bufio.NewReader(os.Stdin),
	}
}

// SetOneShotMessage configures the adapter to send a single message and then wait for a reply.
func (a *Adapter) SetOneShotMessage(msg string) {
	a.oneShot = msg
}

func (a *Adapter) Name() string { return "cli" }

func (a *Adapter) Start(ctx context.Context, handler channel.InboundHandler) error {
	if a.oneShot != "" {
		fmt.Printf("Sending one-shot message: %s\n", a.oneShot)
		if err := handler(ctx, channel.InboundMessage{
			Channel:  "cli",
			SenderID: a.userID,
			ChatID:   "terminal",
			Content:  a.oneShot,
		}); err != nil {
			return fmt.Errorf("handle input: %w", err)
		}
		// Wait for context cancellation (which should happen after response/timeout in main)
		<-ctx.Done()
		return ctx.Err()
	}

	fmt.Printf("CLI Gateway started for user '%s'. Type your message (Ctrl+C to exit):\n", a.userID)
	fmt.Print("> ")

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			// Read input
			line, err := a.reader.ReadString('\n')
			if err != nil {
				return fmt.Errorf("read stdin: %w", err)
			}

			line = strings.TrimSpace(line)
			if line == "" {
				fmt.Print("> ")
				continue
			}

			// Handle input
			slog.Debug("cli: received user input", "content_len", len(line))
			err = handler(ctx, channel.InboundMessage{
				Channel:  "cli",
				SenderID: a.userID, // In CLI mode, senderID is the userID
				ChatID:   "terminal",
				Content:  line,
			})
			if err != nil {
				fmt.Printf("Error: %v\n", err)
			}

			// Re-prompt if we expect a long wait, but usually we just wait for response
			// We don't print "> " here immediately because we might get a response printed first
		}
	}
}

func (a *Adapter) Send(ctx context.Context, msg channel.OutboundMessage) error {
	slog.Debug("cli: sending response", "content_len", len(msg.Content))
	// Print response with color or formatting if desired
	fmt.Printf("\n🤖: %s\n> ", msg.Content)
	return nil
}

func (a *Adapter) Stop() error {
	return nil
}
