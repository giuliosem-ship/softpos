package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"softpos/pkg/auth"
)

const apiTestJWTSecret = "api_test_jwt_secret_key_at_least_32b"

// withTestAuth installs an auth service with a capturing OTP sender.
func withTestAuth(t *testing.T, s *Server) *auth.MemorySender {
	t.Helper()
	sender := auth.NewMemorySender()
	svc, err := auth.NewService(auth.NewMemoryRepository(), sender, auth.Config{JWTSecret: []byte(apiTestJWTSecret)})
	if err != nil {
		t.Fatal(err)
	}
	s.SetAuthService(svc)
	return sender
}

func postJSON(t *testing.T, h http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.RemoteAddr = "192.0.2.10:5000"
	h.ServeHTTP(rec, req)
	return rec
}

// httpLogin runs the OTP flow over HTTP and returns the token pair.
func httpLogin(t *testing.T, s *Server, sender *auth.MemorySender, phone, merchantID string) auth.TokenPair {
	t.Helper()
	h := s.Router()
	if rec := postJSON(t, h, "/api/v1/auth/otp/request", map[string]string{"phone": phone}); rec.Code != http.StatusAccepted {
		t.Fatalf("otp/request: %d %s", rec.Code, rec.Body.String())
	}
	code, _ := sender.LastCode(phone)
	rec := postJSON(t, h, "/api/v1/auth/otp/verify", map[string]string{"phone": phone, "code": code, "merchant_id": merchantID})
	if rec.Code != http.StatusOK {
		t.Fatalf("otp/verify: %d %s", rec.Code, rec.Body.String())
	}
	var tp auth.TokenPair
	if err := json.Unmarshal(rec.Body.Bytes(), &tp); err != nil {
		t.Fatal(err)
	}
	return tp
}

func TestAuthHTTP_OTPLoginRefreshLogout(t *testing.T) {
	s, _ := setupTestServer(t)
	sender := withTestAuth(t, s)
	h := s.Router()

	if rec := postJSON(t, h, "/api/v1/auth/otp/request", map[string]string{"phone": "12345"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid phone: want 400, got %d", rec.Code)
	}

	phone := "+525512312312"
	if rec := postJSON(t, h, "/api/v1/auth/otp/request", map[string]string{"phone": phone}); rec.Code != http.StatusAccepted {
		t.Fatalf("otp/request: %d %s", rec.Code, rec.Body.String())
	}
	rec := postJSON(t, h, "/api/v1/auth/otp/request", map[string]string{"phone": phone})
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("resend: want 429 with Retry-After, got %d %v", rec.Code, rec.Header())
	}
	if rec := postJSON(t, h, "/api/v1/auth/otp/verify", map[string]string{"phone": phone, "code": "not-it"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong code: want 401, got %d", rec.Code)
	}
	code, _ := sender.LastCode(phone)
	rec = postJSON(t, h, "/api/v1/auth/otp/verify", map[string]string{"phone": phone, "code": code})
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("verify: %d %s", rec.Code, rec.Body.String())
	}
	var tp auth.TokenPair
	_ = json.Unmarshal(rec.Body.Bytes(), &tp)
	if tp.AccessToken == "" || tp.RefreshToken == "" || tp.UserID == "" {
		t.Fatalf("token pair = %+v", tp)
	}

	rec = postJSON(t, h, "/api/v1/auth/refresh", map[string]string{"refresh_token": tp.RefreshToken})
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", rec.Code, rec.Body.String())
	}
	var rotated auth.TokenPair
	_ = json.Unmarshal(rec.Body.Bytes(), &rotated)

	// Reuse of the rotated-out token -> 401 REFRESH_TOKEN_REUSED, family revoked.
	rec = postJSON(t, h, "/api/v1/auth/refresh", map[string]string{"refresh_token": tp.RefreshToken})
	if rec.Code != http.StatusUnauthorized || !bytes.Contains(rec.Body.Bytes(), []byte("REFRESH_TOKEN_REUSED")) {
		t.Fatalf("reuse: want 401 REFRESH_TOKEN_REUSED, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := postJSON(t, h, "/api/v1/auth/refresh", map[string]string{"refresh_token": rotated.RefreshToken}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("family must be revoked after reuse, got %d", rec.Code)
	}

	// Fresh session, then logout revokes it.
	tp2 := httpLogin(t, s, sender, "+525598798798", "")
	if rec := postJSON(t, h, "/api/v1/auth/logout", map[string]string{"refresh_token": tp2.RefreshToken}); rec.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", rec.Code)
	}
	if rec := postJSON(t, h, "/api/v1/auth/refresh", map[string]string{"refresh_token": tp2.RefreshToken}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("refresh after logout: want 401, got %d", rec.Code)
	}

	// Selecting a merchant the user does not belong to -> 403.
	if _, err := s.AuthService().RequestOTP(t.Context(), "+525511122233"); err != nil {
		t.Fatal(err)
	}
	c, _ := sender.LastCode("+525511122233")
	if rec := postJSON(t, h, "/api/v1/auth/otp/verify", map[string]string{"phone": "+525511122233", "code": c, "merchant_id": "merch_foreign"}); rec.Code != http.StatusForbidden {
		t.Fatalf("foreign merchant: want 403, got %d", rec.Code)
	}
}
