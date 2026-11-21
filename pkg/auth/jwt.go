package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var (
	ErrTokenInvalid = errors.New("auth: invalid or malformed token")
	ErrTokenExpired = errors.New("auth: token has expired")
)

// TokenTypeAccess is the "typ" claim of access tokens.
const TokenTypeAccess = "access"

// Claims are the JWT claims of a Kerno access token.
type Claims struct {
	Sub        string `json:"sub,omitempty"`         // user id
	MerchantID string `json:"merchant_id,omitempty"` // tenant; empty = no merchant selected
	Role       Role   `json:"role,omitempty"`
	Typ        string `json:"typ,omitempty"`
	Iss        string `json:"iss,omitempty"`
	Aud        string `json:"aud,omitempty"`
	Jti        string `json:"jti,omitempty"`
	Iat        int64  `json:"iat,omitempty"`
	Exp        int64  `json:"exp,omitempty"`
}

var hs256Header = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

// SignHS256 serializes and signs claims as a compact HS256 JWT.
func SignHS256(c Claims, secret []byte) (string, error) {
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	signingInput := hs256Header + "." + base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// ParseHS256 verifies signature, algorithm (only HS256 accepted, so "none" and
// algorithm confusion are rejected) and expiry (exp is mandatory).
func ParseHS256(token string, secret []byte, now time.Time) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(secret) == 0 {
		return nil, ErrTokenInvalid
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, ErrTokenInvalid
	}
	var hdr struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(headerJSON, &hdr); err != nil || hdr.Alg != "HS256" {
		return nil, ErrTokenInvalid
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, ErrTokenInvalid
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, ErrTokenInvalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, ErrTokenInvalid
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, ErrTokenInvalid
	}
	if c.Exp == 0 {
		return nil, ErrTokenInvalid
	}
	if now.Unix() >= c.Exp {
		return nil, ErrTokenExpired
	}
	return &c, nil
}
