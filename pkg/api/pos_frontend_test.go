package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"softpos/pkg/auth"
)

func TestPOSFrontendStaticAndHealthEndpoints(t *testing.T) {
	server, _ := setupTestServer(t)

	// Verify working directory points to workspace with web/pos
	if _, err := os.Stat("web/pos/index.html"); err != nil {
		// If running from package dir, set relative or absolute path
		abs, _ := filepath.Abs("../../web/pos")
		server.SetStaticDir(abs)
	}

	// 1. Test /healthz
	reqHealth := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	recHealth := httptest.NewRecorder()
	server.Router().ServeHTTP(recHealth, reqHealth)

	if recHealth.Code != http.StatusOK {
		t.Fatalf("expected 200 on /healthz, got %d", recHealth.Code)
	}
	var healthResp map[string]any
	_ = json.Unmarshal(recHealth.Body.Bytes(), &healthResp)
	if healthResp["status"] != "UP" {
		t.Errorf("expected health status UP, got %v", healthResp["status"])
	}

	// 2. Test /pos/ delivery
	reqPOS := httptest.NewRequest(http.MethodGet, "/pos/", nil)
	recPOS := httptest.NewRecorder()
	server.Router().ServeHTTP(recPOS, reqPOS)

	if recPOS.Code != http.StatusOK {
		t.Fatalf("expected 200 on /pos/, got %d", recPOS.Code)
	}
	if !strings.Contains(recPOS.Body.String(), "SoftPOS México - Terminal de Cobro") {
		t.Errorf("expected HTML title in response")
	}

	// 3. Test /pos/softpos_sdk.js delivery
	reqSDK := httptest.NewRequest(http.MethodGet, "/pos/softpos_sdk.js", nil)
	recSDK := httptest.NewRecorder()
	server.Router().ServeHTTP(recSDK, reqSDK)

	if recSDK.Code != http.StatusOK {
		t.Fatalf("expected 200 on /pos/softpos_sdk.js, got %d", recSDK.Code)
	}
	if !strings.Contains(recSDK.Body.String(), "TapToPhoneService") {
		t.Errorf("expected SDK JS content in response")
	}

	// 4. Test root redirect / -> /pos/
	reqRoot := httptest.NewRequest(http.MethodGet, "/", nil)
	recRoot := httptest.NewRecorder()
	server.Router().ServeHTTP(recRoot, reqRoot)

	if recRoot.Code != http.StatusFound {
		t.Errorf("expected 302 redirect from / to /pos/, got %d", recRoot.Code)
	}
}

func TestMerchantRegistrationAndPOSLifecycle(t *testing.T) {
	server, _ := setupTestServer(t)
	sender := withTestAuth(t, server)

	// 0. Unauthenticated calls are rejected once auth is mounted.
	recAnon := httptest.NewRecorder()
	server.Router().ServeHTTP(recAnon, httptest.NewRequest(http.MethodPost, "/api/v1/merchants", strings.NewReader(`{}`)))
	if recAnon.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", recAnon.Code)
	}

	// 1. OTP login (new user, no merchant yet), then create merchant via POST /api/v1/merchants
	session := httpLogin(t, server, sender, "+525512340001", "")
	regPayload := RegisterMerchantRequest{
		RFC:          "TAQ850101XYZ",
		Name:         "Taqueria El Pastor Central",
		IsRESICO:     true,
		VirtualCLABE: "646180123400000007",
	}
	body, _ := json.Marshal(regPayload)
	recReg := httptest.NewRecorder()
	server.Router().ServeHTTP(recReg, withBearer(httptest.NewRequest(http.MethodPost, "/api/v1/merchants", bytes.NewReader(body)), session.AccessToken))
	if recReg.Code != http.StatusCreated {
		t.Fatalf("expected 201 on merchant creation, got %d: %s", recReg.Code, recReg.Body.String())
	}
	var created struct {
		MerchantID string `json:"merchant_id"`
	}
	_ = json.Unmarshal(recReg.Body.Bytes(), &created)
	merchantID := created.MerchantID

	// A merchant-less token cannot operate on the new merchant yet.
	recNoScope := httptest.NewRecorder()
	server.Router().ServeHTTP(recNoScope, withBearer(httptest.NewRequest(http.MethodGet, "/api/v1/merchants/"+merchantID, nil), session.AccessToken))
	if recNoScope.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for unscoped token, got %d", recNoScope.Code)
	}

	// 1b. Re-scope the session to the new merchant (caller became owner).
	recRef := postJSON(t, server.Router(), "/api/v1/auth/refresh", map[string]string{"refresh_token": session.RefreshToken, "merchant_id": merchantID})
	if recRef.Code != http.StatusOK {
		t.Fatalf("refresh with merchant_id: %d %s", recRef.Code, recRef.Body.String())
	}
	var scoped auth.TokenPair
	_ = json.Unmarshal(recRef.Body.Bytes(), &scoped)
	if scoped.MerchantID != merchantID || scoped.Role != auth.RoleOwner {
		t.Fatalf("scoped session = %+v", scoped)
	}
	token := scoped.AccessToken

	// 2. Fetch merchant profile via GET /api/v1/merchants/{id}
	recGet := httptest.NewRecorder()
	server.Router().ServeHTTP(recGet, withBearer(httptest.NewRequest(http.MethodGet, "/api/v1/merchants/"+merchantID, nil), token))
	if recGet.Code != http.StatusOK {
		t.Fatalf("expected 200 on get merchant, got %d", recGet.Code)
	}
	var merchResp Merchant
	_ = json.Unmarshal(recGet.Body.Bytes(), &merchResp)
	if merchResp.ID != merchantID || merchResp.CLABE != "646180123400000007" {
		t.Errorf("unexpected merchant response: %+v", merchResp)
	}

	// 3. Perform Tap-to-Phone EMV Authorize + Settle flow for POS (tenant from token)
	validTLVHex := "9F02060000000250005F2A0204849F2701809F2608AABBCCDDEEFF00119F360200145A0841523133333390125F24022812"
	authBody, _ := json.Marshal(EMVAuthorizeRequest{
		PaymentID: "pos_emv_001",
		TLVHex:    validTLVHex,
	})
	recAuth := httptest.NewRecorder()
	server.Router().ServeHTTP(recAuth, withBearer(httptest.NewRequest(http.MethodPost, "/api/v1/payments/emv/authorize", bytes.NewReader(authBody)), token))
	if recAuth.Code != http.StatusOK {
		t.Fatalf("expected 200 on EMV authorize, got %d: %s", recAuth.Code, recAuth.Body.String())
	}

	// Settle sale of $250.00 MXN (25,000 cents)
	settleBody, _ := json.Marshal(SettlePaymentRequest{
		PaymentID:        "pos_emv_001",
		GrossAmountCents: 25000, // optional cross-check against the authorized amount
	})
	recSettle := httptest.NewRecorder()
	server.Router().ServeHTTP(recSettle, withBearer(httptest.NewRequest(http.MethodPost, "/api/v1/payments/settle", bytes.NewReader(settleBody)), token))
	if recSettle.Code != http.StatusOK {
		t.Fatalf("expected 200 on settle, got %d: %s", recSettle.Code, recSettle.Body.String())
	}

	// Query balances to ensure Available has net proceeds
	recBal := httptest.NewRecorder()
	server.Router().ServeHTTP(recBal, withBearer(httptest.NewRequest(http.MethodGet, "/api/v1/merchants/"+merchantID+"/balances", nil), token))
	var balResp MerchantBalancesResponse
	_ = json.Unmarshal(recBal.Body.Bytes(), &balResp)

	if balResp.AvailableCents <= 0 || balResp.TaxEscrowCents <= 0 {
		t.Errorf("expected positive available and tax escrow balances, got %+v", balResp)
	}
}
