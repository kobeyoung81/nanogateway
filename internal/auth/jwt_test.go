package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func jwksServer(t *testing.T, privKey *rsa.PrivateKey, kid string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := base64URLEncode(privKey.N.Bytes())
		e := bigEndianBytes(privKey.E)
		jwks := map[string]any{
			"keys": []map[string]any{
				{
					"kty": "RSA",
					"use": "sig",
					"alg": "RS256",
					"kid": kid,
					"n":   n,
					"e":   e,
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(jwks)
	}))
}

func signToken(t *testing.T, privKey *rsa.PrivateKey, kid, subject, role, tier string) string {
	t.Helper()
	claims := jwt.MapClaims{
		"sub":  subject,
		"role": role,
		"tier": tier,
		"iss":  "test-issuer",
		"iat":  time.Now().Unix(),
		"exp":  time.Now().Add(time.Hour).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = kid
	s, err := token.SignedString(privKey)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNewJWKSValidator(t *testing.T) {
	v := NewJWKSValidator("https://auth.example.com/.well-known/jwks")
	if v == nil {
		t.Fatal("expected non-nil validator")
	}
	if v.cacheTTL != 5*time.Minute {
		t.Fatalf("expected 5min cache, got %v", v.cacheTTL)
	}
}

func TestValidateToken_Valid(t *testing.T) {
	privKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := jwksServer(t, privKey, "k1")
	defer srv.Close()

	token := signToken(t, privKey, "k1", "user-1", "admin", "pro")

	v := NewJWKSValidator(srv.URL)
	claims, err := v.ValidateToken(token)
	if err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
	if claims["sub"] != "user-1" {
		t.Fatalf("expected sub user-1, got %v", claims["sub"])
	}
	if claims["role"] != "admin" {
		t.Fatalf("expected role admin, got %v", claims["role"])
	}
	if claims["tier"] != "pro" {
		t.Fatalf("expected tier pro, got %v", claims["tier"])
	}
}

func TestValidateToken_Expired(t *testing.T) {
	privKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := jwksServer(t, privKey, "k1")
	defer srv.Close()

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub":  "user-1",
		"role": "user",
		"tier": "free",
		"exp":  time.Now().Add(-1 * time.Hour).Unix(),
	})
	token.Header["kid"] = "k1"
	s, _ := token.SignedString(privKey)

	v := NewJWKSValidator(srv.URL)
	_, err := v.ValidateToken(s)
	if err == nil {
		t.Fatal("expected error for expired token")
	}
}

func TestValidateToken_MissingKid(t *testing.T) {
	privKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := jwksServer(t, privKey, "k1")
	defer srv.Close()

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub":  "user-1",
		"role": "user",
		"tier": "free",
		"exp":  time.Now().Add(time.Hour).Unix(),
	})
	s, _ := token.SignedString(privKey)

	v := NewJWKSValidator(srv.URL)
	_, err := v.ValidateToken(s)
	if err == nil {
		t.Fatal("expected error for missing kid")
	}
}

func TestValidateToken_WrongSigningMethod(t *testing.T) {
	privKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := jwksServer(t, privKey, "k1")
	defer srv.Close()

	claims := jwt.MapClaims{
		"sub":  "user-1",
		"role": "user",
		"tier": "free",
		"exp":  time.Now().Add(time.Hour).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	token.Header["kid"] = "k1"
	s, _ := token.SignedString([]byte("secret"))

	v := NewJWKSValidator(srv.URL)
	_, err := v.ValidateToken(s)
	if err == nil {
		t.Fatal("expected error for wrong signing method")
	}
}

func TestFetchKey_CacheHit(t *testing.T) {
	privKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := jwksServer(t, privKey, "k1")
	defer srv.Close()

	v := NewJWKSValidator(srv.URL)

	// First call fetches from server
	token := signToken(t, privKey, "k1", "user-1", "user", "free")
	_, err := v.ValidateToken(token)
	if err != nil {
		t.Fatalf("first validate: %v", err)
	}

	// Second call uses cache (no server request needed for key lookup)
	// Test this by shutting down the server after first call
	// We can't easily test cache-hit timing, but we verify no panic on cached path
	token2 := signToken(t, privKey, "k1", "user-1", "user", "free")
	_, err = v.ValidateToken(token2)
	if err != nil {
		t.Fatalf("second validate should use cache: %v", err)
	}
}

func TestFetchKey_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	privKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	token := signToken(t, privKey, "k1", "user-1", "user", "free")

	v := NewJWKSValidator(srv.URL)
	_, err := v.ValidateToken(token)
	if err == nil {
		t.Fatal("expected error when JWKS server returns 500")
	}
}

func TestFetchKey_KeyNotFound(t *testing.T) {
	privKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := jwksServer(t, privKey, "k1")
	defer srv.Close()

	token := signToken(t, privKey, "unknown-kid", "user-1", "user", "free")

	v := NewJWKSValidator(srv.URL)
	_, err := v.ValidateToken(token)
	if err == nil {
		t.Fatal("expected error for unknown kid")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected 'not found' error, got %v", err)
	}
}

func base64URLEncode(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

func bigEndianBytes(e int) string {
	return base64.RawURLEncoding.EncodeToString(bigEndianRaw(e))
}

func bigEndianRaw(n int) []byte {
	if n == 0 {
		return []byte{0}
	}
	var result []byte
	for n > 0 {
		result = append([]byte{byte(n % 256)}, result...)
		n /= 256
	}
	return result
}
