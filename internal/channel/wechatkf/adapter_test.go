package wechatkf

import (
	"testing"
)

// Valid test encodingAESKey: 43 base64 chars that decode (with trailing "=") to 32 bytes.
// "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG" is 43 chars.
const testEncodingAESKey = "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"

func TestNew_Valid(t *testing.T) {
	a, err := New("corp1", "secret1", "kf1", "tok1", testEncodingAESKey, ":9090", "ws://bus")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.corpID != "corp1" {
		t.Errorf("corpID = %q, want %q", a.corpID, "corp1")
	}
	if a.kfSecret != "secret1" {
		t.Errorf("kfSecret = %q, want %q", a.kfSecret, "secret1")
	}
	if a.openKfID != "kf1" {
		t.Errorf("openKfID = %q, want %q", a.openKfID, "kf1")
	}
	if a.callbackToken != "tok1" {
		t.Errorf("callbackToken = %q, want %q", a.callbackToken, "tok1")
	}
	if a.encodingAESKey != testEncodingAESKey {
		t.Errorf("encodingAESKey = %q, want %q", a.encodingAESKey, testEncodingAESKey)
	}
	if a.callbackAddr != ":9090" {
		t.Errorf("callbackAddr = %q, want %q", a.callbackAddr, ":9090")
	}
	if len(a.aesKey) != 32 {
		t.Errorf("aesKey length = %d, want 32", len(a.aesKey))
	}
	if a.notifyCh == nil {
		t.Error("notifyCh is nil")
	}
}

func TestNew_MissingRequired(t *testing.T) {
	tests := []struct {
		name                                              string
		corpID, kfSecret, openKfID, token, encodingAESKey string
	}{
		{"missing corpID", "", "s", "k", "t", testEncodingAESKey},
		{"missing kfSecret", "c", "", "k", "t", testEncodingAESKey},
		{"missing openKfID", "c", "s", "", "t", testEncodingAESKey},
		{"missing token", "c", "s", "k", "", testEncodingAESKey},
		{"missing encodingAESKey", "c", "s", "k", "t", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.corpID, tt.kfSecret, tt.openKfID, tt.token, tt.encodingAESKey, "", "ws://bus")
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestNew_InvalidEncodingAESKey(t *testing.T) {
	// Bad base64
	_, err := New("c", "s", "k", "t", "!!!invalid!!!", "", "ws://bus")
	if err == nil {
		t.Fatal("expected error for bad base64")
	}

	// Wrong length (too short — only 10 base64 chars)
	_, err = New("c", "s", "k", "t", "AAAAAAAAAA", "", "ws://bus")
	if err == nil {
		t.Fatal("expected error for wrong key length")
	}
}

func TestNew_DefaultCallbackAddr(t *testing.T) {
	a, err := New("c", "s", "k", "t", testEncodingAESKey, "", "ws://bus")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.callbackAddr != ":8082" {
		t.Errorf("callbackAddr = %q, want %q", a.callbackAddr, ":8082")
	}
}
