package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"softpos/pkg/auth"
)

type contextKey string

const (
	merchantIDContextKey contextKey = "softpos_merchant_id"
	userIDContextKey     contextKey = "softpos_user_id"
	roleContextKey       contextKey = "softpos_role"
)

var (
	ErrInvalidToken  = auth.ErrTokenInvalid
	ErrTokenExpired  = auth.ErrTokenExpired
	ErrUnauthorized  = errors.New("unauthorized")
	ErrForbiddenIDOR = errors.New("forbidden: cross-tenant access violation")
)

type jwtClaims = auth.Claims

// GenerateTestJWT creates an owner-scoped HS256 access token for merchantID
// (sub = merchantID). Intended for tests; real tokens come from pkg/auth.
func GenerateTestJWT(merchantID string, secret string, ttl time.Duration) (string, error) {
	now := time.Now()
	return auth.SignHS256(auth.Claims{
		Sub:        merchantID,
		MerchantID: merchantID,
		Role:       auth.RoleOwner,
		Typ:        auth.TokenTypeAccess,
		Iat:        now.Unix(),
		Exp:        now.Add(ttl).Unix(),
	}, []byte(secret))
}

// ValidateJWT verifies an HS256 access token (signature, alg, exp, typ).
func ValidateJWT(tokenStr string, secret string) (*jwtClaims, error) {
	c, err := auth.ParseHS256(tokenStr, []byte(secret), time.Now())
	if err != nil {
		return nil, err
	}
	if c.Typ != auth.TokenTypeAccess {
		return nil, ErrInvalidToken
	}
	return c, nil
}

// MerchantIDFromContext retrieves the authenticated tenant from the request context.
func MerchantIDFromContext(ctx context.Context) (string, bool) {
	val, ok := ctx.Value(merchantIDContextKey).(string)
	return val, ok && val != ""
}

// UserIDFromContext retrieves the authenticated user (JWT sub).
func UserIDFromContext(ctx context.Context) (string, bool) {
	val, ok := ctx.Value(userIDContextKey).(string)
	return val, ok && val != ""
}

// RoleFromContext retrieves the authenticated role inside the tenant.
func RoleFromContext(ctx context.Context) auth.Role {
	val, _ := ctx.Value(roleContextKey).(auth.Role)
	return val
}

// AuthMiddleware verifies Bearer JWTs signed with secret and enforces tenant scoping.
func AuthMiddleware(secret string) func(http.Handler) http.Handler {
	return authMiddleware(func(tok string) (*auth.Claims, error) { return ValidateJWT(tok, secret) })
}

// pathMerchantID extracts the merchant id a request targets via its path
// ({id}/{merchant_id} pattern values, or the segment after "merchants").
func pathMerchantID(r *http.Request) string {
	if id := r.PathValue("id"); id != "" {
		return id
	}
	if id := r.PathValue("merchant_id"); id != "" {
		return id
	}
	segments := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	for i, seg := range segments {
		if seg == "merchants" && i+1 < len(segments) {
			return segments[i+1]
		}
	}
	return ""
}

func authMiddleware(parse func(string) (*auth.Claims, error)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
				w.Header().Set("WWW-Authenticate", `Bearer realm="kerno"`)
				http.Error(w, `{"error":"unauthorized","message":"missing or invalid Authorization header"}`, http.StatusUnauthorized)
				return
			}

			tokenStr := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
			claims, err := parse(tokenStr)
			if err != nil {
				w.Header().Set("WWW-Authenticate", `Bearer realm="kerno", error="invalid_token"`)
				http.Error(w, `{"error":"unauthorized","message":"invalid or expired token"}`, http.StatusUnauthorized)
				return
			}

			// Anti-IDOR: a path that names a merchant must name the token's tenant.
			if pathID := pathMerchantID(r); pathID != "" && pathID != claims.MerchantID {
				http.Error(w, `{"error":"forbidden","message":"anti-IDOR violation: access denied to foreign merchant resource"}`, http.StatusForbidden)
				return
			}

			ctx := context.WithValue(r.Context(), merchantIDContextKey, claims.MerchantID)
			ctx = context.WithValue(ctx, userIDContextKey, claims.Sub)
			ctx = context.WithValue(ctx, roleContextKey, claims.Role)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// publicRoutes is the explicit allowlist of routes reachable without a JWT.
// Everything else (every /api/v1/* route, and unknown paths) requires a token.
var publicRoutes = map[string]bool{
	"GET /healthz":                         true,
	"GET /":                                true,
	"POST /api/v1/auth/otp/request":        true,
	"POST /api/v1/auth/otp/verify":         true,
	"POST /api/v1/auth/refresh":            true,
	"POST /api/v1/auth/logout":             true,
	"GET /api/v1/auth/attestation-nonce":   true,
	"POST /api/v1/auth/attestation-nonce":  true,
	"POST /api/v1/auth/attestation/verify": true,
	"POST /api/v1/auth/dev-session":        true, // 404 unless DEV_MODE
	"POST /api/v1/webhooks/stp/abono":      true, // verified by STP signature
	"POST /api/v1/webhooks/codi/confirm":   true, // verified by HMAC signature
}

// isPublicRoute reports whether r is on the public allowlist. Static Web POS
// assets under /pos/ are public (GET/HEAD only).
func isPublicRoute(r *http.Request) bool {
	method := r.Method
	if method == http.MethodHead {
		method = http.MethodGet
	}
	if method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/pos/") {
		return true
	}
	return publicRoutes[method+" "+r.URL.Path]
}

// authGate mounts JWT authentication in front of every non-public route and
// per-IP rate limiting in front of the /api/v1/auth/* endpoints.
func (s *Server) authGate(next http.Handler) http.Handler {
	protected := authMiddleware(func(tok string) (*auth.Claims, error) {
		return s.authService().ParseAccessToken(tok)
	})(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/auth/") {
			s.mu.RLock()
			limiter := s.authLimiter
			s.mu.RUnlock()
			if ok, retryAfter := limiter.Allow(clientIP(r)); !ok {
				w.Header().Set("Retry-After", retryAfter)
				writeError(w, http.StatusTooManyRequests, "RATE_LIMITED")
				return
			}
		}
		if isPublicRoute(r) {
			next.ServeHTTP(w, r)
			return
		}
		protected.ServeHTTP(w, r)
	})
}

// requireTenant returns the authenticated tenant. If the request also names a
// merchant (body/path) it must match the token (anti-IDOR -> 403). A token
// without a selected merchant gets 403 MERCHANT_SCOPE_REQUIRED.
func requireTenant(w http.ResponseWriter, r *http.Request, claimed string) (string, bool) {
	tenant, ok := MerchantIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusForbidden, "MERCHANT_SCOPE_REQUIRED")
		return "", false
	}
	if claimed != "" && claimed != tenant {
		writeError(w, http.StatusForbidden, "TENANT_MISMATCH")
		return "", false
	}
	return tenant, true
}

// requireRole checks the caller's role inside the tenant.
func requireRole(w http.ResponseWriter, r *http.Request, allowed ...auth.Role) bool {
	role := RoleFromContext(r.Context())
	for _, a := range allowed {
		if role == a {
			return true
		}
	}
	writeError(w, http.StatusForbidden, "ROLE_FORBIDDEN")
	return false
}

// tenantIdempotencyKey scopes idempotency keys by tenant so one merchant can
// never replay another merchant's cached response.
func tenantIdempotencyKey(tenant, key string) string {
	return "m:" + tenant + ":" + key
}
