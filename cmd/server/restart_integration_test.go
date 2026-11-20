package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"softpos/pkg/api"
	"softpos/pkg/auth"
	"softpos/pkg/store/pgtest"
)

// validEMVTLVHex: $250.00 MXN, currency 0484, ARQC, PAN 4152313333339012, exp 2812.
const validEMVTLVHex = "9F02060000000250005F2A0204849F2701809F2608AABBCCDDEEFF00119F360200145A0841523133333390125F24022812"

var payoutKeySeq atomic.Int64

func doJSON(t *testing.T, srv *api.Server, token, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, &buf)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if method == http.MethodPost && path == "/api/v1/disbursements" {
		req.Header.Set("Idempotency-Key", fmt.Sprintf("it-payout-%d", payoutKeySeq.Add(1)))
	}
	srv.Router().ServeHTTP(rec, req)
	return rec
}

// TestPGIntegration_StateSurvivesRestart creates merchants, card payments, CoDi
// confirmations, CSDs, monthly sales and ledger balances on one server, rebuilds
// the server on the same database, and checks every piece of state persisted.
func TestPGIntegration_StateSurvivesRestart(t *testing.T) {
	cfg := Config{
		SQLDriver:         "pgx",
		DatabaseURL:       pgtest.URL(t),
		STPHMACSecret:     "test_secret",
		CoDiWebhookSecret: "restart_codi_webhook_secret_32_bytes!",
		JWTSecret:         testJWTSecret, VaultMasterKeyHex: testVaultKeyHex,
	}

	// ---- Boot 1: create state ----
	srv1, db1, _, err := BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("boot 1: %v", err)
	}
	srv1.SetLegacyCardFlowEnabled(true) // legacy EMV endpoints are DEV_MODE-only in cmd/server
	// Merchant registration needs a real user; the server generates the id and
	// makes the caller owner (membership persisted in merchant_memberships).
	owner, err := srv1.AuthService().EnsureUser(context.Background(), "+525512300001")
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	userTok, _ := srv1.AuthService().MintAccessToken(owner.ID, "", "")
	regRec := doJSON(t, srv1, userTok, http.MethodPost, "/api/v1/merchants", map[string]any{
		"rfc": "GARM850101ABC", "name": "Taqueria Persistente", "is_resico": true,
	})
	if regRec.Code != http.StatusCreated {
		t.Fatalf("register merchant: %d %s", regRec.Code, regRec.Body.String())
	}
	var created struct {
		MerchantID   string `json:"merchant_id"`
		VirtualCLABE string `json:"virtual_clabe"`
	}
	_ = json.Unmarshal(regRec.Body.Bytes(), &created)
	merchantID := created.MerchantID
	clabe := created.VirtualCLABE // server-assigned collection CLABE
	tok, _ := srv1.AuthService().MintAccessToken(owner.ID, merchantID, auth.RoleOwner)
	privDER, certDER, err := generateSelfSignedCSD()
	if err != nil {
		t.Fatalf("csd: %v", err)
	}
	if err := srv1.RegisterMerchantCSD(merchantID, privDER, certDER); err != nil {
		t.Fatalf("RegisterMerchantCSD: %v", err)
	}
	for _, id := range []string{"pay_settled", "pay_authorized"} {
		if rec := doJSON(t, srv1, tok, http.MethodPost, "/api/v1/payments/emv/authorize", map[string]any{
			"payment_id": id, "merchant_id": merchantID, "tlv_hex": validEMVTLVHex,
		}); rec.Code != http.StatusOK {
			t.Fatalf("authorize %s: %d %s", id, rec.Code, rec.Body.String())
		}
	}
	if rec := doJSON(t, srv1, tok, http.MethodPost, "/api/v1/payments/settle", map[string]any{
		"payment_id": "pay_settled", "merchant_id": merchantID, "gross_amount_cents": 25000,
	}); rec.Code != http.StatusOK {
		t.Fatalf("settle: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, srv1, tok, http.MethodPost, "/api/v1/payments/codi/qr", map[string]any{
		"merchant_id": merchantID, "amount_cents": 11600, "concept": "Tacos", "reference": "REFRESTART1",
	}); rec.Code != http.StatusOK {
		t.Fatalf("codi qr: %d %s", rec.Code, rec.Body.String())
	}
	codiBody := `{"tracking_key":"BNXRESTART0001","seller_clabe":"` + clabe + `","buyer_clabe":"012180000118359719",` +
		`"amount_cents":11600,"timestamp":"2026-11-19T12:00:00Z","status":"APROBADO","banxico_approval_code":"AUT9"}`
	ts := time.Now().Unix()
	codiReq := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/codi/confirm", strings.NewReader(codiBody))
	codiReq.Header.Set(api.WebhookTimestampHeader, strconv.FormatInt(ts, 10))
	codiReq.Header.Set(api.WebhookSignatureHeader, api.SignWebhook([]byte(cfg.CoDiWebhookSecret), ts, []byte(codiBody)))
	codiRec := httptest.NewRecorder()
	srv1.Router().ServeHTTP(codiRec, codiReq)
	if codiRec.Code != http.StatusOK {
		t.Fatalf("codi confirm: %d %s", codiRec.Code, codiRec.Body.String())
	}
	salesBefore := srv1.GetMerchantMonthlySalesCents(merchantID)
	balRec := doJSON(t, srv1, tok, http.MethodGet, "/api/v1/merchants/"+merchantID+"/balances", nil)
	if balRec.Code != http.StatusOK {
		t.Fatalf("balances boot 1: %d %s", balRec.Code, balRec.Body.String())
	}
	balancesBefore := balRec.Body.String()
	_ = db1.Close()

	// ---- Boot 2: same database, fresh process state ----
	srv2, db2, _, err := BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("boot 2: %v", err)
	}
	srv2.SetLegacyCardFlowEnabled(true) // legacy EMV endpoints are DEV_MODE-only in cmd/server
	defer func() { _ = db2.Close() }()

	if mem, found, err := srv2.AuthService().Repository().GetMembership(context.Background(), owner.ID, merchantID); err != nil || !found || mem.Role != auth.RoleOwner {
		t.Fatalf("owner membership not persisted: %+v found=%v err=%v", mem, found, err)
	}

	m, ok := srv2.GetMerchant(merchantID)
	if !ok || m.Name != "Taqueria Persistente" || m.CLABE != clabe {
		t.Fatalf("merchant not persisted (or name overwritten): %+v ok=%v", m, ok)
	}
	if byClabe, ok := srv2.GetMerchantByCLABE(clabe); !ok || byClabe.ID != merchantID {
		t.Fatalf("CLABE mapping not persisted: %+v", byClabe)
	}
	if salesBefore <= 0 {
		t.Fatalf("expected monthly sales > 0 before restart, got %d", salesBefore)
	}
	if got := srv2.GetMerchantMonthlySalesCents(merchantID); got != salesBefore {
		t.Fatalf("monthly sales: want %d after restart, got %d", salesBefore, got)
	}
	if rec := doJSON(t, srv2, tok, http.MethodGet, "/api/v1/merchants/"+merchantID+"/balances", nil); rec.Body.String() != balancesBefore {
		t.Fatalf("balances changed across restart:\nbefore %s\nafter  %s", balancesBefore, rec.Body.String())
	}

	// Payment state machine: settled payment refunds, authorized one voids, duplicate ID rejected.
	rec := doJSON(t, srv2, tok, http.MethodPost, "/api/v1/payments/emv/reverse", map[string]any{"payment_id": "pay_settled"})
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(`"REFUNDED"`)) {
		t.Fatalf("refund after restart: %d %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, srv2, tok, http.MethodPost, "/api/v1/payments/emv/reverse", map[string]any{"payment_id": "pay_authorized"})
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(`"VOIDED"`)) {
		t.Fatalf("void after restart: %d %s", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/payments/emv/authorize", bytes.NewBufferString(
		`{"payment_id":"pay_settled","merchant_id":"`+merchantID+`","tlv_hex":"`+validEMVTLVHex+`"}`))
	req.Header.Set("Idempotency-Key", "fresh-key-dup-check")
	req.Header.Set("Authorization", "Bearer "+tok) // boot-1 token still valid: same JWT_SECRET
	dup := httptest.NewRecorder()
	srv2.Router().ServeHTTP(dup, req)
	if dup.Code != http.StatusConflict {
		t.Fatalf("expected 409 for duplicate payment id after restart, got %d %s", dup.Code, dup.Body.String())
	}

	// CoDi request + confirmation rows persisted.
	var n int
	if err := db2.QueryRow(`SELECT COUNT(*) FROM codi_requests WHERE reference = 'REFRESTART1'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("codi request not persisted: n=%d err=%v", n, err)
	}
	if err := db2.QueryRow(`SELECT COUNT(*) FROM codi_confirmations WHERE tracking_key = 'BNXRESTART0001' AND amount_cents = 11600`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("codi confirmation not persisted: n=%d err=%v", n, err)
	}

	// Encrypted CSD survives: stamping works without re-registering the CSD.
	rec = doJSON(t, srv2, tok, http.MethodPost, "/api/v1/invoices/cfdi", map[string]any{
		"merchant_id": merchantID, "receptor_rfc": "B2B850101XYZ", "receptor_nombre": "EMPRESA CLIENTE SA DE CV",
		"receptor_cp": "06600", "receptor_regimen": "601", "uso_cfdi": "G03", "gross_amount_cents": 116000,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("CFDI stamp after restart (CSD not persisted?): %d %s", rec.Code, rec.Body.String())
	}
}
