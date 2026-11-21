package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCORS (MVP-31): deny by default; explicit origins get the CORS headers
// and preflights; others get 403 on preflight and no Allow-Origin.
func TestCORS(t *testing.T) {
	s, _ := setupTestServer(t)
	preflight := func(origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodOptions, "/api/v1/card-payments/intents", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", "POST")
		req.Header.Set("Access-Control-Request-Headers", "authorization, content-type, idempotency-key")
		return serve(s, req)
	}
	// Default: nobody is allowed.
	if rec := preflight("https://pos.kerno.mx"); rec.Code != http.StatusForbidden || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("deny by default: %d %v", rec.Code, rec.Header())
	}
	origins, err := ParseCORSOrigins(" https://POS.kerno.mx , https://backoffice.kerno.mx:8443 ")
	if err != nil || len(origins) != 2 || origins[0] != "https://pos.kerno.mx" {
		t.Fatalf("parse: %v %v", origins, err)
	}
	s.SetCORSAllowedOrigins(origins)
	rec := preflight("https://pos.kerno.mx")
	if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "https://pos.kerno.mx" ||
		rec.Header().Get("Access-Control-Allow-Headers") == "" || rec.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("allowed preflight: %d %v", rec.Code, rec.Header())
	}
	if rec := preflight("https://evil.example"); rec.Code != http.StatusForbidden || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("other origin preflight: %d %v", rec.Code, rec.Header())
	}
	if rec := preflight("https://backoffice.kerno.mx"); rec.Code != http.StatusForbidden {
		t.Fatalf("the port is part of the origin: %d", rec.Code)
	}
	// Actual request from an allowed origin: decorated (auth still applies).
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Origin", "https://pos.kerno.mx")
	rec = serve(s, req)
	if rec.Header().Get("Access-Control-Allow-Origin") != "https://pos.kerno.mx" || rec.Header().Get("Vary") == "" {
		t.Fatalf("allowed request: %v", rec.Header())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/merchants/x", nil)
	req.Header.Set("Origin", "https://pos.kerno.mx")
	if rec := serve(s, req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("CORS never bypasses auth: %d", rec.Code)
	}
	// Other origin: served (same-origin POSTs carry Origin too) but no CORS headers.
	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Origin", "https://evil.example")
	if rec := serve(s, req); rec.Code != http.StatusOK || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("other origin request: %d %v", rec.Code, rec.Header())
	}
	for _, bad := range []string{"*", "https://*.kerno.mx", "pos.kerno.mx", "https://pos.kerno.mx/app", "ftp://x"} {
		if _, err := ParseCORSOrigins(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}
