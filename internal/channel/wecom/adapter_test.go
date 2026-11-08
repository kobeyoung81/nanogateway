package wecom

import (
	"context"
	"net"
	"testing"
	"time"

	"gitee.com/kobeyoung81/nanogateway/internal/channel"
)

// validAESKey is a 43-char base64 string that decodes to 32 bytes (with "=" appended).
const validAESKey = "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"

func TestNew_Valid(t *testing.T) {
	a, err := New("corp123", "secret456", 1000, "tok", validAESKey, ":0")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if a.Name() != "wecom" {
		t.Errorf("Name() = %q, want %q", a.Name(), "wecom")
	}
}

func TestNew_MissingCorpID(t *testing.T) {
	_, err := New("", "secret", 1000, "tok", validAESKey, ":0")
	if err == nil {
		t.Fatal("New() expected error for empty corpID")
	}
}

func TestNew_MissingCorpSecret(t *testing.T) {
	_, err := New("corp", "", 1000, "tok", validAESKey, ":0")
	if err == nil {
		t.Fatal("New() expected error for empty corpSecret")
	}
}

func TestNew_InvalidAgentID(t *testing.T) {
	_, err := New("corp", "secret", 0, "tok", validAESKey, ":0")
	if err == nil {
		t.Fatal("New() expected error for zero agentID")
	}
}

func TestNew_MissingToken(t *testing.T) {
	_, err := New("corp", "secret", 1000, "", validAESKey, ":0")
	if err == nil {
		t.Fatal("New() expected error for empty token")
	}
}

func TestNew_MissingEncodingAESKey(t *testing.T) {
	_, err := New("corp", "secret", 1000, "tok", "", ":0")
	if err == nil {
		t.Fatal("New() expected error for empty encodingAESKey")
	}
}

func TestNew_DefaultCallbackAddr(t *testing.T) {
	a, err := New("corp", "secret", 1000, "tok", validAESKey, "")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if a.callbackAddr != ":8081" {
		t.Errorf("callbackAddr = %q, want %q", a.callbackAddr, ":8081")
	}
}

func TestAdapter_StartStop(t *testing.T) {
	// Use a random free port to avoid collisions
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	addr := listener.Addr().String()
	listener.Close() // free the port for the adapter

	a, err := New("corp123", "secret456", 1000, "tok", validAESKey, addr)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- a.Start(ctx, func(_ context.Context, _ channel.InboundMessage) error {
			return nil
		})
	}()

	// Give server time to start
	time.Sleep(100 * time.Millisecond)

	// Verify it's listening
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("server not listening: %v", err)
	}
	conn.Close()

	// Stop via context cancellation
	cancel()

	select {
	case err := <-errCh:
		if err != nil && err != context.Canceled {
			t.Fatalf("Start() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start() did not return after cancel")
	}

	// Stop should be idempotent
	if err := a.Stop(); err != nil {
		t.Errorf("Stop() error = %v", err)
	}
}

func TestOnIncomingMessage_Nil(t *testing.T) {
	a, _ := New("corp", "secret", 1000, "tok", validAESKey, ":0")
	called := false
	a.handler = func(_ context.Context, _ channel.InboundMessage) error {
		called = true
		return nil
	}

	if err := a.OnIncomingMessage(nil); err != nil {
		t.Fatalf("OnIncomingMessage(nil) error = %v", err)
	}
	if called {
		t.Error("handler should not be called for nil message")
	}
}

func TestExtractTextContent_Nil(t *testing.T) {
	if got := extractTextContent(nil); got != "" {
		t.Errorf("extractTextContent(nil) = %q, want empty", got)
	}
}
