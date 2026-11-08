package auth

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type JWKSValidator struct {
	jwksURL   string
	client    *http.Client
	keyCache  map[string]*rsa.PublicKey
	lastFetch time.Time
	cacheTTL  time.Duration
	mu        sync.RWMutex
}

func NewJWKSValidator(jwksURL string) *JWKSValidator {
	return &JWKSValidator{
		jwksURL:  jwksURL,
		client:   &http.Client{Timeout: 10 * time.Second},
		keyCache: make(map[string]*rsa.PublicKey),
		cacheTTL: 5 * time.Minute,
	}
}

func (v *JWKSValidator) ValidateToken(tokenStr string) (map[string]any, error) {
	token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (any, error) {
		kid, ok := t.Header["kid"].(string)
		if !ok {
			return nil, fmt.Errorf("missing kid in token header")
		}
		key, err := v.fetchKey(kid)
		if err != nil {
			return nil, err
		}
		return key, nil
	})
	if err != nil {
		return nil, err
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token")
	}

	result := make(map[string]any)
	for k, v := range claims {
		result[k] = v
	}
	return result, nil
}

func (v *JWKSValidator) fetchKey(kid string) (*rsa.PublicKey, error) {
	v.mu.RLock()
	if key, ok := v.keyCache[kid]; ok && time.Since(v.lastFetch) < v.cacheTTL {
		v.mu.RUnlock()
		return key, nil
	}
	v.mu.RUnlock()

	v.mu.Lock()
	defer v.mu.Unlock()

	if key, ok := v.keyCache[kid]; ok && time.Since(v.lastFetch) < v.cacheTTL {
		return key, nil
	}

	resp, err := v.client.Get(v.jwksURL)
	if err != nil {
		return nil, fmt.Errorf("fetch jwks: %w", err)
	}
	defer resp.Body.Close()

	var jwks struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return nil, fmt.Errorf("decode jwks: %w", err)
	}

	for _, key := range jwks.Keys {
		if key.Kty != "RSA" {
			continue
		}
		nBytes, err := base64.RawURLEncoding.DecodeString(key.N)
		if err != nil {
			continue
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(key.E)
		if err != nil {
			continue
		}

		pubKey := &rsa.PublicKey{
			N: new(big.Int).SetBytes(nBytes),
			E: int(new(big.Int).SetBytes(eBytes).Int64()),
		}
		v.keyCache[key.Kid] = pubKey
	}
	v.lastFetch = time.Now()

	if key, ok := v.keyCache[kid]; ok {
		return key, nil
	}
	return nil, fmt.Errorf("key %q not found in JWKS", kid)
}
