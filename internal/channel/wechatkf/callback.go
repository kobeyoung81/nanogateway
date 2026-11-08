package wechatkf

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"
)

// xmlEncryptEnvelope is the XML envelope WeChat sends for encrypted callback messages.
type xmlEncryptEnvelope struct {
	XMLName    xml.Name `xml:"xml"`
	ToUserName string   `xml:"ToUserName"`
	Encrypt    string   `xml:"Encrypt"`
	AgentID    string   `xml:"AgentID"`
}

type xmlKFEvent struct {
	XMLName    xml.Name `xml:"xml"`
	MsgType    string   `xml:"MsgType"`
	Event      string   `xml:"Event"`
	Token      string   `xml:"Token"`
	OpenKfID   string   `xml:"OpenKfId"`
	ToUserName string   `xml:"ToUserName"`
}

// handleCallback routes GET requests to URL verification and POST requests to event notification.
func (a *Adapter) handleCallback(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.handleVerification(w, r)
	case http.MethodPost:
		a.handleEventNotification(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleVerification handles WeChat URL verification (GET).
// WeChat sends msg_signature, timestamp, nonce, echostr as query params.
// We verify the signature, decrypt echostr, and return the plaintext.
func (a *Adapter) handleVerification(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	msgSignature := q.Get("msg_signature")
	timestamp := q.Get("timestamp")
	nonce := q.Get("nonce")
	echostr := q.Get("echostr")

	if msgSignature == "" || timestamp == "" || nonce == "" || echostr == "" {
		http.Error(w, "missing required parameters", http.StatusBadRequest)
		return
	}

	if !a.verifySignature(msgSignature, timestamp, nonce, echostr) {
		http.Error(w, "invalid signature", http.StatusForbidden)
		return
	}

	plaintext, err := a.decryptMsg(echostr)
	if err != nil {
		slog.Error("wechatkf: callback verification decrypt failed", "error", err)
		http.Error(w, "decrypt failed", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(plaintext))
}

// handleEventNotification handles WeChat event notifications (POST).
// WeChat sends an XML body with an encrypted message. We verify the signature,
// then signal the polling loop via notifyCh.
func (a *Adapter) handleEventNotification(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	msgSignature := q.Get("msg_signature")
	timestamp := q.Get("timestamp")
	nonce := q.Get("nonce")

	if msgSignature == "" || timestamp == "" || nonce == "" {
		http.Error(w, "missing required parameters", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var envelope xmlEncryptEnvelope
	if err := xml.Unmarshal(body, &envelope); err != nil {
		http.Error(w, "invalid XML", http.StatusBadRequest)
		return
	}

	if !a.verifySignature(msgSignature, timestamp, nonce, envelope.Encrypt) {
		http.Error(w, "invalid signature", http.StatusForbidden)
		return
	}

	plaintext, err := a.decryptMsg(envelope.Encrypt)
	if err != nil {
		slog.Error("wechatkf: callback decrypt failed", "error", err)
		http.Error(w, "decrypt failed", http.StatusBadRequest)
		return
	}

	var event xmlKFEvent
	if err := xml.Unmarshal([]byte(plaintext), &event); err != nil {
		slog.Error("wechatkf: callback event parse failed", "error", err)
		http.Error(w, "invalid event XML", http.StatusBadRequest)
		return
	}

	if event.OpenKfID != "" && event.OpenKfID != a.openKfID {
		slog.Warn("wechatkf: callback open_kfid mismatch", "configured", a.openKfID, "received", event.OpenKfID)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("success"))
		return
	}

	if event.Token != "" && a.state != nil {
		tokenExpiresAt := time.Now().Add(9 * time.Minute)
		if err := a.state.UpdateWeChatKFCallbackToken(a.openKfID, event.Token, tokenExpiresAt); err != nil {
			slog.Warn("wechatkf: persist callback token failed", "open_kf_id", a.openKfID, "error", err)
		}
	}

	// Non-blocking signal to the polling loop
	select {
	case a.notifyCh <- struct{}{}:
		slog.Debug("wechatkf: callback notification received, signaled poll")
	default:
		slog.Debug("wechatkf: callback notification received, poll already pending")
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("success"))
}

// verifySignature checks the SHA1 signature from WeChat.
// sig = SHA1(sort([token, timestamp, nonce, encrypt]))
func (a *Adapter) verifySignature(msgSig, timestamp, nonce, encrypt string) bool {
	parts := []string{a.callbackToken, timestamp, nonce, encrypt}
	sort.Strings(parts)
	joined := strings.Join(parts, "")

	h := sha1.New()
	h.Write([]byte(joined))
	computed := fmt.Sprintf("%x", h.Sum(nil))

	return subtle.ConstantTimeCompare([]byte(computed), []byte(msgSig)) == 1
}

// decryptMsg decrypts a base64-encoded AES-256-CBC encrypted message from WeChat.
// The payload format after decryption is: 16-byte random + 4-byte msg_len (big-endian) + msg + receiveid
func (a *Adapter) decryptMsg(base64Ciphertext string) (string, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(base64Ciphertext)
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}

	block, err := aes.NewCipher(a.aesKey)
	if err != nil {
		return "", fmt.Errorf("create cipher: %w", err)
	}

	if len(ciphertext) < aes.BlockSize || len(ciphertext)%aes.BlockSize != 0 {
		return "", fmt.Errorf("invalid ciphertext length: %d", len(ciphertext))
	}

	// IV is the first 16 bytes of the AES key
	iv := a.aesKey[:aes.BlockSize]
	mode := cipher.NewCBCDecrypter(block, iv)

	plaintext := make([]byte, len(ciphertext))
	mode.CryptBlocks(plaintext, ciphertext)

	plaintext, err = pkcs7Unpad(plaintext)
	if err != nil {
		return "", fmt.Errorf("pkcs7 unpad: %w", err)
	}

	// Payload: 16-byte random + 4-byte msg_len (big-endian) + msg + receiveid
	if len(plaintext) < 20 {
		return "", fmt.Errorf("payload too short: %d bytes", len(plaintext))
	}

	msgLen := binary.BigEndian.Uint32(plaintext[16:20])
	if 20+msgLen > uint32(len(plaintext)) {
		return "", fmt.Errorf("message length %d exceeds payload %d", msgLen, len(plaintext)-20)
	}

	msg := string(plaintext[20 : 20+msgLen])
	return msg, nil
}

// wechatPKCS7BlockSize is the block size WeChat uses for PKCS7 padding.
// WeChat's encryption uses 32-byte PKCS7 padding (not the AES block size of 16).
const wechatPKCS7BlockSize = 32

// pkcs7Unpad removes PKCS7 padding from decrypted data.
func pkcs7Unpad(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty data")
	}

	padLen := int(data[len(data)-1])
	if padLen == 0 || padLen > wechatPKCS7BlockSize || padLen > len(data) {
		return nil, fmt.Errorf("invalid padding length: %d", padLen)
	}

	for i := len(data) - padLen; i < len(data); i++ {
		if data[i] != byte(padLen) {
			return nil, fmt.Errorf("invalid padding byte at position %d", i)
		}
	}

	return data[:len(data)-padLen], nil
}
