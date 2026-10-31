package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAuthMiddleware_MissingOrMalformedHeader(t *testing.T) {
	secret := "test_jwt_secret_32_bytes_long_key_123"
	mw := AuthMiddleware(secret)

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"OK"}`))
	}))

	// 1. Missing Authorization header
	req := httptest.NewRequest("GET", "/api/v1/merchants/m1/balances", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for missing auth header, got %d", rec.Code)
	}

	// 2. Malformed scheme (Basic instead of Bearer)
	req2 := httptest.NewRequest("GET", "/api/v1/merchants/m1/balances", nil)
	req2.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for Basic scheme, got %d", rec2.Code)
	}
}

func TestAuthMiddleware_ValidTokenAndContextInjection(t *testing.T) {
	secret := "test_jwt_secret_32_bytes_long_key_123"
	token, err := GenerateTestJWT("merch_pos_01", secret, 1*time.Hour)
	if err != nil {
		t.Fatalf("GenerateTestJWT failed: %v", err)
	}

	mw := AuthMiddleware(secret)
	var capturedMerchantID string

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mID, ok := MerchantIDFromContext(r.Context())
		if ok {
			capturedMerchantID = mID
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/v1/payments", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 OK, got %d", rec.Code)
	}
	if capturedMerchantID != "merch_pos_01" {
		t.Errorf("expected merchant_id 'merch_pos_01' in context, got %q", capturedMerchantID)
	}
}

func TestAuthMiddleware_AntiIDORTenantIsolation(t *testing.T) {
	secret := "test_jwt_secret_32_bytes_long_key_123"
	// Token is for merchant 'merch_pos_01'
	token, err := GenerateTestJWT("merch_pos_01", secret, 1*time.Hour)
	if err != nil {
		t.Fatalf("GenerateTestJWT failed: %v", err)
	}

	mux := http.NewServeMux()
	mw := AuthMiddleware(secret)

	mux.Handle("GET /api/v1/merchants/{id}/balances", mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"authorized"}`))
	})))

	// 1. Requesting own balances -> 200 OK
	reqOwn := httptest.NewRequest("GET", "/api/v1/merchants/merch_pos_01/balances", nil)
	reqOwn.Header.Set("Authorization", "Bearer "+token)
	recOwn := httptest.NewRecorder()
	mux.ServeHTTP(recOwn, reqOwn)

	if recOwn.Code != http.StatusOK {
		t.Errorf("expected 200 for matching merchant ID, got %d: %s", recOwn.Code, recOwn.Body.String())
	}

	// 2. IDOR Attack: Requesting another merchant's balances ('merch_victim_99') -> 403 Forbidden
	reqAttack := httptest.NewRequest("GET", "/api/v1/merchants/merch_victim_99/balances", nil)
	reqAttack.Header.Set("Authorization", "Bearer "+token)
	recAttack := httptest.NewRecorder()
	mux.ServeHTTP(recAttack, reqAttack)

	if recAttack.Code != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden on IDOR cross-tenant access, got %d: %s", recAttack.Code, recAttack.Body.String())
	}
}

func TestAuthMiddleware_ExpiredTokenFails(t *testing.T) {
	secret := "test_jwt_secret_32_bytes_long_key_123"
	// Token expired 10 minutes ago
	token, err := GenerateTestJWT("merch_pos_01", secret, -10*time.Minute)
	if err != nil {
		t.Fatalf("GenerateTestJWT failed: %v", err)
	}

	mw := AuthMiddleware(secret)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/v1/payments", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for expired token, got %d", rec.Code)
	}
}
