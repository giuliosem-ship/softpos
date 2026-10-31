package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

type contextKey string

const merchantIDContextKey contextKey = "softpos_merchant_id"

var (
	ErrInvalidToken   = errors.New("invalid or malformed token")
	ErrTokenExpired   = errors.New("token has expired")
	ErrUnauthorized   = errors.New("unauthorized")
	ErrForbiddenIDOR  = errors.New("forbidden: cross-tenant access violation")
)

type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

type jwtClaims struct {
	Sub        string `json:"sub,omitempty"`
	MerchantID string `json:"merchant_id,omitempty"`
	Exp        int64  `json:"exp,omitempty"`
	Iat        int64  `json:"iat,omitempty"`
}

// GenerateTestJWT creates a signed HMAC-SHA256 JWT for testing or token issuance.
func GenerateTestJWT(merchantID string, secret string, ttl time.Duration) (string, error) {
	header := jwtHeader{Alg: "HS256", Typ: "JWT"}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	headerB64 := base64.RawURLEncoding.EncodeToString(headerJSON)

	now := time.Now()
	claims := jwtClaims{
		Sub:        merchantID,
		MerchantID: merchantID,
		Exp:        now.Add(ttl).Unix(),
		Iat:        now.Unix(),
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	payloadB64 := base64.RawURLEncoding.EncodeToString(claimsJSON)

	signingInput := headerB64 + "." + payloadB64
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	signature := mac.Sum(nil)
	sigB64 := base64.RawURLEncoding.EncodeToString(signature)

	return signingInput + "." + sigB64, nil
}

// ValidateJWT verifies the HMAC-SHA256 signature and expiration of a JWT string.
func ValidateJWT(tokenStr string, secret string) (*jwtClaims, error) {
	parts := strings.Split(tokenStr, ".")
	if len(parts) != 3 {
		return nil, ErrInvalidToken
	}

	signingInput := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		// Try standard URLEncoding with padding fallback
		sig, err = base64.URLEncoding.DecodeString(parts[2])
		if err != nil {
			return nil, ErrInvalidToken
		}
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	expectedSig := mac.Sum(nil)

	if !hmac.Equal(sig, expectedSig) {
		return nil, ErrInvalidToken
	}

	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payloadJSON, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return nil, ErrInvalidToken
		}
	}

	var claims jwtClaims
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		return nil, ErrInvalidToken
	}

	if claims.Exp > 0 && time.Now().Unix() > claims.Exp {
		return nil, ErrTokenExpired
	}

	return &claims, nil
}

// MerchantIDFromContext retrieves the authenticated merchant ID from the request context.
func MerchantIDFromContext(ctx context.Context) (string, bool) {
	val, ok := ctx.Value(merchantIDContextKey).(string)
	return val, ok && val != ""
}

// AuthMiddleware returns an HTTP middleware verifying Bearer JWTs and enforcing tenant scoping.
func AuthMiddleware(secret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
				http.Error(w, `{"error":"unauthorized","message":"missing or invalid Authorization header"}`, http.StatusUnauthorized)
				return
			}

			tokenStr := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
			claims, err := ValidateJWT(tokenStr, secret)
			if err != nil {
				http.Error(w, `{"error":"unauthorized","message":"invalid or expired token"}`, http.StatusUnauthorized)
				return
			}

			merchantID := claims.MerchantID
			if merchantID == "" {
				merchantID = claims.Sub
			}

			// Anti-IDOR Tenant Scoping check
			// Check Go 1.22+ PathValue if route has {id} or {merchant_id}
			pathID := r.PathValue("id")
			if pathID == "" {
				pathID = r.PathValue("merchant_id")
			}

			// Fallback check on URL path: /api/v1/merchants/{id}/...
			if pathID == "" {
				segments := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
				for i, seg := range segments {
					if seg == "merchants" && i+1 < len(segments) {
						pathID = segments[i+1]
						break
					}
				}
			}

			if pathID != "" && pathID != merchantID {
				http.Error(w, `{"error":"forbidden","message":"anti-IDOR violation: access denied to foreign merchant resource"}`, http.StatusForbidden)
				return
			}

			ctx := context.WithValue(r.Context(), merchantIDContextKey, merchantID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
