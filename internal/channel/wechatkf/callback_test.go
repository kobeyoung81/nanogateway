package wechatkf

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"gitee.com/kobeyoung81/nanogateway/internal/channel"
	"gitee.com/kobeyoung81/nanoprotocol"
)

// testAdapter creates an Adapter with known crypto keys for testing.
func testAdapter(t *testing.T) *Adapter {
	t.Helper()
	a, err := New("corp1", "secret1", "kf1", "testtoken", testEncodingAESKey, ":0", "ws://bus")
	if err != nil {
		t.Fatalf("failed to create test adapter: %v", err)
	}
	a.state = &mockSyncStateClient{}
	return a
}

type mockSyncStateClient struct {
	state *nanoprotocol.WeChatKFSyncStateResponse
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func (m *mockSyncStateClient) GetWeChatKFSyncState(openKfID string) (*nanoprotocol.WeChatKFSyncStateResponse, error) {
	if m.state == nil {
		return nil, nil
	}
	return m.state, nil
}

func (m *mockSyncStateClient) UpdateWeChatKFCursor(openKfID, cursor string) error {
	if m.state == nil {
		m.state = &nanoprotocol.WeChatKFSyncStateResponse{Found: true, OpenKfID: openKfID}
	}
	m.state.Cursor = cursor
	return nil
}

func (m *mockSyncStateClient) UpdateWeChatKFCallbackToken(openKfID, callbackToken string, tokenExpiresAt time.Time) error {
	if m.state == nil {
		m.state = &nanoprotocol.WeChatKFSyncStateResponse{Found: true, OpenKfID: openKfID}
	}
	m.state.CallbackToken = callbackToken
	m.state.TokenExpiresAt = tokenExpiresAt
	return nil
}

func (m *mockSyncStateClient) Close() error { return nil }

// computeSignature computes a WeChat-style SHA1 signature for testing.
func computeSignature(token, timestamp, nonce, encrypt string) string {
	parts := []string{token, timestamp, nonce, encrypt}
	sort.Strings(parts)
	h := sha1.New()
	h.Write([]byte(strings.Join(parts, "")))
	return fmt.Sprintf("%x", h.Sum(nil))
}

// encryptTestMsg encrypts a message payload using the adapter's AES key for testing.
func encryptTestMsg(t *testing.T, aesKey []byte, msg, receiveid string) string {
	t.Helper()

	// Build payload: 16-byte random + 4-byte msg_len (big-endian) + msg + receiveid
	random := make([]byte, 16)
	rng := rand.New(rand.NewSource(42))
	rng.Read(random)

	msgBytes := []byte(msg)
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(msgBytes)))

	payload := make([]byte, 0, 16+4+len(msgBytes)+len(receiveid))
	payload = append(payload, random...)
	payload = append(payload, lenBuf...)
	payload = append(payload, msgBytes...)
	payload = append(payload, []byte(receiveid)...)

	// PKCS7 pad with 32-byte block size (WeChat standard)
	padLen := 32 - (len(payload) % 32)
	padding := make([]byte, padLen)
	for i := range padding {
		padding[i] = byte(padLen)
	}
	payload = append(payload, padding...)

	// AES-256-CBC encrypt with IV = key[:16]
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		t.Fatalf("create cipher: %v", err)
	}
	iv := aesKey[:aes.BlockSize]
	mode := cipher.NewCBCEncrypter(block, iv)

	ciphertext := make([]byte, len(payload))
	mode.CryptBlocks(ciphertext, payload)

	return base64.StdEncoding.EncodeToString(ciphertext)
}

func TestVerifySignature_Valid(t *testing.T) {
	a := testAdapter(t)
	timestamp := "1234567890"
	nonce := "nonce123"
	encrypt := "someEncryptedData"

	sig := computeSignature("testtoken", timestamp, nonce, encrypt)

	if !a.verifySignature(sig, timestamp, nonce, encrypt) {
		t.Error("expected valid signature to pass")
	}
}

func TestVerifySignature_Invalid(t *testing.T) {
	a := testAdapter(t)
	if a.verifySignature("badsig", "1234567890", "nonce123", "data") {
		t.Error("expected invalid signature to fail")
	}
}

func TestDecryptMsg_Valid(t *testing.T) {
	a := testAdapter(t)
	original := "hello world"
	encrypted := encryptTestMsg(t, a.aesKey, original, "corp1")

	decrypted, err := a.decryptMsg(encrypted)
	if err != nil {
		t.Fatalf("decrypt failed: %v", err)
	}
	if decrypted != original {
		t.Errorf("decrypted = %q, want %q", decrypted, original)
	}
}

func TestDecryptMsg_BadBase64(t *testing.T) {
	a := testAdapter(t)
	_, err := a.decryptMsg("!!!notbase64!!!")
	if err == nil {
		t.Fatal("expected error for bad base64")
	}
}

func TestDecryptMsg_TruncatedPayload(t *testing.T) {
	a := testAdapter(t)
	// Encrypt a minimal valid block, then truncate the base64 to produce invalid ciphertext
	// Use a ciphertext that's too short (less than one block)
	short := base64.StdEncoding.EncodeToString([]byte("short"))
	_, err := a.decryptMsg(short)
	if err == nil {
		t.Fatal("expected error for truncated payload")
	}
}

func TestPkcs7Unpad(t *testing.T) {
	tests := []struct {
		name    string
		input   []byte
		want    []byte
		wantErr bool
	}{
		{
			name:  "valid padding 3",
			input: []byte{1, 2, 3, 3, 3, 3},
			want:  []byte{1, 2, 3},
		},
		{
			name:  "valid padding 1",
			input: []byte{1, 2, 3, 1},
			want:  []byte{1, 2, 3},
		},
		{
			name:  "valid padding 28 (wechat 32-byte block)",
			input: func() []byte {
				data := []byte{1, 2, 3, 4}
				for i := 0; i < 28; i++ {
					data = append(data, 28)
				}
				return data
			}(),
			want: []byte{1, 2, 3, 4},
		},
		{
			name:    "empty data",
			input:   []byte{},
			wantErr: true,
		},
		{
			name:    "zero padding",
			input:   []byte{1, 2, 0},
			wantErr: true,
		},
		{
			name:    "padding too large (>32)",
			input:   []byte{1, 2, 33},
			wantErr: true,
		},
		{
			name:    "inconsistent padding",
			input:   []byte{1, 2, 3, 2},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := pkcs7Unpad(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got len %d, want len %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("byte %d: got %d, want %d", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestHandleVerification_Success(t *testing.T) {
	a := testAdapter(t)
	echoMsg := "verification_echo_string"
	echostr := encryptTestMsg(t, a.aesKey, echoMsg, "corp1")

	timestamp := "1234567890"
	nonce := "nonce123"
	sig := computeSignature("testtoken", timestamp, nonce, echostr)

	url := fmt.Sprintf("/callback?msg_signature=%s&timestamp=%s&nonce=%s&echostr=%s",
		sig, timestamp, nonce, echostr)

	req := httptest.NewRequest(http.MethodGet, url, nil)
	w := httptest.NewRecorder()

	a.handleCallback(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if w.Body.String() != echoMsg {
		t.Errorf("body = %q, want %q", w.Body.String(), echoMsg)
	}
}

func TestHandleVerification_BadSignature(t *testing.T) {
	a := testAdapter(t)
	echostr := encryptTestMsg(t, a.aesKey, "test", "corp1")

	url := fmt.Sprintf("/callback?msg_signature=badsig&timestamp=123&nonce=abc&echostr=%s", echostr)
	req := httptest.NewRequest(http.MethodGet, url, nil)
	w := httptest.NewRecorder()

	a.handleCallback(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestHandleVerification_MissingParams(t *testing.T) {
	a := testAdapter(t)

	req := httptest.NewRequest(http.MethodGet, "/callback", nil)
	w := httptest.NewRecorder()

	a.handleCallback(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleEventNotification_Success(t *testing.T) {
	a := testAdapter(t)
	encrypted := encryptTestMsg(t, a.aesKey, "<xml><MsgType>event</MsgType><Event>kf_msg_or_event</Event><Token>sync-token-1</Token><OpenKfId>kf1</OpenKfId></xml>", "corp1")

	timestamp := "1234567890"
	nonce := "nonce123"
	sig := computeSignature("testtoken", timestamp, nonce, encrypted)

	xmlBody := fmt.Sprintf(`<xml><ToUserName>corp1</ToUserName><Encrypt>%s</Encrypt></xml>`, encrypted)

	url := fmt.Sprintf("/callback?msg_signature=%s&timestamp=%s&nonce=%s", sig, timestamp, nonce)
	req := httptest.NewRequest(http.MethodPost, url, strings.NewReader(xmlBody))
	w := httptest.NewRecorder()

	a.handleCallback(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Verify notifyCh was signaled
	select {
	case <-a.notifyCh:
		// good
	default:
		t.Error("expected notifyCh to be signaled")
	}

	state := a.state.(*mockSyncStateClient).state
	if state == nil {
		t.Fatal("expected callback token state to be persisted")
	}
	if state.CallbackToken != "sync-token-1" {
		t.Errorf("CallbackToken = %q, want %q", state.CallbackToken, "sync-token-1")
	}
}

func TestHandleEventNotification_BadSignature(t *testing.T) {
	a := testAdapter(t)
	encrypted := encryptTestMsg(t, a.aesKey, "test", "corp1")

	xmlBody := fmt.Sprintf(`<xml><ToUserName>corp1</ToUserName><Encrypt>%s</Encrypt></xml>`, encrypted)

	url := "/callback?msg_signature=badsig&timestamp=123&nonce=abc"
	req := httptest.NewRequest(http.MethodPost, url, strings.NewReader(xmlBody))
	w := httptest.NewRecorder()

	a.handleCallback(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestHandleCallback_MethodNotAllowed(t *testing.T) {
	a := testAdapter(t)

	req := httptest.NewRequest(http.MethodPut, "/callback", nil)
	w := httptest.NewRecorder()

	a.handleCallback(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestAdapter_StartStop(t *testing.T) {
	a := testAdapter(t)

	// Use a random available port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	a.callbackAddr = fmt.Sprintf("127.0.0.1:%d", port)

	ctx, cancel := context.WithCancel(context.Background())

	// Start in a goroutine (Start blocks). Use a mock handler since we won't have a real token.
	startErr := make(chan error, 1)
	go func() {
		// Override refreshToken to avoid real API call by pre-setting token
		a.mu.Lock()
		a.token = "test-token"
		a.tokenExpAt = time.Now().Add(1 * time.Hour)
		a.mu.Unlock()

		// We need to bypass refreshToken in Start, so we call the components directly
		a.handler = func(ctx context.Context, msg channel.InboundMessage) error { return nil }

		mux := http.NewServeMux()
		mux.HandleFunc("/callback", a.handleCallback)
		a.server = &http.Server{Addr: a.callbackAddr, Handler: mux}

		errCh := make(chan error, 1)
		go func() {
			if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				errCh <- err
			}
			close(errCh)
		}()

		select {
		case <-ctx.Done():
			startErr <- nil
		case err := <-errCh:
			startErr <- err
		}
	}()

	// Wait briefly for server to start
	time.Sleep(100 * time.Millisecond)

	// Verify the server is listening by making a request
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/callback", port))
	if err != nil {
		t.Fatalf("server not listening: %v", err)
	}
	resp.Body.Close()

	// Stop should shut down cleanly
	cancel()
	if err := a.Stop(); err != nil {
		t.Fatalf("Stop error: %v", err)
	}

	// Verify server is no longer listening
	time.Sleep(100 * time.Millisecond)
	_, err = http.Get(fmt.Sprintf("http://127.0.0.1:%d/callback", port))
	if err == nil {
		t.Error("expected connection error after stop, but request succeeded")
	}
}

func TestPollMessages_PaginatesAndPersistsCursor(t *testing.T) {
	a := testAdapter(t)
	state := &mockSyncStateClient{
		state: &nanoprotocol.WeChatKFSyncStateResponse{
			Found:          true,
			OpenKfID:       "kf1",
			Cursor:         "cursor-0",
			CallbackToken:  "sync-token-1",
			TokenExpiresAt: time.Now().Add(5 * time.Minute),
		},
	}
	a.state = state
	a.handler = func(ctx context.Context, msg channel.InboundMessage) error { return nil }

	a.mu.Lock()
	a.token = "access-token"
	a.tokenExpAt = time.Now().Add(1 * time.Hour)
	a.mu.Unlock()

	var requests []kfSyncRequest
	origClient := http.DefaultClient
	http.DefaultClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			var syncReq kfSyncRequest
			if err := json.NewDecoder(req.Body).Decode(&syncReq); err != nil {
				return nil, err
			}
			requests = append(requests, syncReq)

			var body string
			switch len(requests) {
			case 1:
				body = `{"errcode":0,"errmsg":"ok","next_cursor":"cursor-1","has_more":1,"msg_list":[{"msgid":"msg-1","open_kfid":"kf1","external_userid":"user-1","send_time":1,"origin":3,"msgtype":"text","text":{"content":"hello"}}]}`
			case 2:
				body = `{"errcode":0,"errmsg":"ok","next_cursor":"cursor-2","has_more":0,"msg_list":[{"msgid":"msg-2","open_kfid":"kf1","external_userid":"user-1","send_time":2,"origin":3,"msgtype":"text","text":{"content":"world"}}]}`
			default:
				return nil, fmt.Errorf("unexpected sync_msg call %d", len(requests))
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		}),
	}
	defer func() {
		http.DefaultClient = origClient
	}()

	a.pollMessages(context.Background())

	if len(requests) != 2 {
		t.Fatalf("syncMessages call count = %d, want 2", len(requests))
	}
	if requests[0].Cursor != "cursor-0" {
		t.Errorf("first cursor = %q, want %q", requests[0].Cursor, "cursor-0")
	}
	if requests[0].Token != "sync-token-1" {
		t.Errorf("first token = %q, want %q", requests[0].Token, "sync-token-1")
	}
	if requests[1].Cursor != "cursor-1" {
		t.Errorf("second cursor = %q, want %q", requests[1].Cursor, "cursor-1")
	}
	if state.state.Cursor != "cursor-2" {
		t.Errorf("persisted cursor = %q, want %q", state.state.Cursor, "cursor-2")
	}
}
