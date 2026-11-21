package api

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// CORS (MVP-31, docs/AUTH.md "CORS"). Deny by default: without an allowlist
// no response carries Access-Control-Allow-Origin, so browsers on other
// origins cannot read API responses (the Web POS and the backoffice are
// served by this server, same origin, and need no CORS). Allowed origins are
// exact scheme://host[:port] values from CORS_ALLOWED_ORIGINS; "*" is never
// accepted. Auth uses bearer tokens, so credentials are never allowed.

// ErrInvalidCORSOrigin is returned for an allowlist entry that is not an
// origin (scheme://host[:port], no path) or is "*".
var ErrInvalidCORSOrigin = errors.New("CORS origin must be scheme://host[:port] (no path, no wildcard)")

// ParseCORSOrigins parses a comma-separated allowlist into normalized origins.
func ParseCORSOrigins(list string) ([]string, error) {
	var out []string
	for _, raw := range strings.Split(list, ",") {
		o := strings.TrimSpace(raw)
		if o == "" {
			continue
		}
		n, ok := normalizeOrigin(o)
		if !ok {
			return nil, ErrInvalidCORSOrigin
		}
		out = append(out, n)
	}
	return out, nil
}

func normalizeOrigin(o string) (string, bool) {
	if strings.Contains(o, "*") {
		return "", false
	}
	u, err := url.Parse(o)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	return strings.ToLower(u.Scheme + "://" + u.Host), true
}

// SetCORSAllowedOrigins installs the allowlist (normalized origins).
func (s *Server) SetCORSAllowedOrigins(origins []string) {
	m := make(map[string]bool, len(origins))
	for _, o := range origins {
		if n, ok := normalizeOrigin(o); ok {
			m[n] = true
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.corsOrigins = m
}

const (
	corsAllowMethods  = "GET, POST, PUT, DELETE, OPTIONS"
	corsAllowHeaders  = "Authorization, Content-Type, Idempotency-Key"
	corsExposeHeaders = "Idempotent-Replayed, Retry-After"
	corsMaxAge        = "600"
)

// corsMiddleware answers preflights and decorates responses for allowed
// origins; preflights from other origins get 403 without CORS headers.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Add("Vary", "Origin")
		n, ok := normalizeOrigin(origin)
		s.mu.RLock()
		allowed := ok && s.corsOrigins[n]
		s.mu.RUnlock()
		preflight := r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
		if !allowed {
			if preflight {
				writeError(w, http.StatusForbidden, "CORS_ORIGIN_NOT_ALLOWED")
				return
			}
			next.ServeHTTP(w, r) // same-origin requests carry Origin too
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		if preflight {
			w.Header().Add("Vary", "Access-Control-Request-Method")
			w.Header().Add("Vary", "Access-Control-Request-Headers")
			w.Header().Set("Access-Control-Allow-Methods", corsAllowMethods)
			w.Header().Set("Access-Control-Allow-Headers", corsAllowHeaders)
			w.Header().Set("Access-Control-Max-Age", corsMaxAge)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Access-Control-Expose-Headers", corsExposeHeaders)
		next.ServeHTTP(w, r)
	})
}
