package main

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"softpos/pkg/api"
	"softpos/pkg/auth"
	"softpos/pkg/identity"
	"softpos/pkg/kyb"
	"softpos/pkg/store"
	"softpos/pkg/store/pgtest"
)

func kybPGApplication() map[string]any {
	return map[string]any{
		"legal_name": "Xochilt Casas Chavez", "rfc": "CACX7605101P8", "curp": "CACX760510MDFSHC09",
		"tax_regime": "RESICO_PF", "mcc": "5812", "contact_email": "dueno@example.com", "contact_phone": "+525512345678",
		"address": map[string]any{"street": "Av. Reforma", "ext_number": "10", "neighborhood": "Centro",
			"city": "Ciudad de Mexico", "state": "CDMX", "postal_code": "06000"},
	}
}

func uploadPGDoc(t *testing.T, srv *api.Server, tok, merchantID, docType string, content []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("type", docType)
	fw, _ := mw.CreateFormFile("file", "doc.png")
	_, _ = fw.Write(content)
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/merchants/"+merchantID+"/kyb/documents", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	return rec
}

// registerDraftMerchantPG runs the public alta as a fresh user and returns
// the new merchant id with an owner token for it.
func registerDraftMerchantPG(t *testing.T, srv *api.Server, phone string) (string, string) {
	t.Helper()
	ctx := context.Background()
	user, err := srv.AuthService().EnsureUser(ctx, phone)
	if err != nil {
		t.Fatal(err)
	}
	userTok, _ := srv.AuthService().MintAccessToken(user.ID, "", "")
	rec := doJSON(t, srv, userTok, http.MethodPost, "/api/v1/merchants", map[string]any{"rfc": "CACX7605101P8", "name": "Fonda Xochilt", "is_resico": true})
	if rec.Code != http.StatusCreated {
		t.Fatalf("alta: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		MerchantID string `json:"merchant_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	owner, _ := srv.AuthService().MintAccessToken(user.ID, created.MerchantID, auth.RoleOwner)
	return created.MerchantID, owner
}

// TestPGIntegration_KYBFlowSurvivesRestart: alta (DRAFT, no money moves) ->
// application + encrypted documents -> submit (IN_REVIEW) -> restart ->
// documents still decrypt -> approval assigns the CLABE and onboards at the
// processor -> card intent and CoDi QR allowed.
func TestPGIntegration_KYBFlowSurvivesRestart(t *testing.T) {
	cfg := Config{
		SQLDriver: "pgx", DatabaseURL: pgtest.URL(t), STPHMACSecret: "test_secret",
		JWTSecret: testJWTSecret, VaultMasterKeyHex: testVaultKeyHex, KYCProvider: "mock",
		AcquirerProvider: "mock", AcquirerWebhookSecret: "kyb_pg_acquirer_webhook_secret_32b!",
	}
	srv, db, _, err := BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildServer: %v", err)
	}
	m, owner := registerDraftMerchantPG(t, srv, "+525511110001")
	intent := func(s *api.Server, key string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]any{"amount_cents": 1000})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/card-payments/intents", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+owner)
		req.Header.Set("Idempotency-Key", key)
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		return rec
	}
	if rec := intent(srv, "early"); rec.Code != http.StatusForbidden {
		t.Fatalf("DRAFT merchant intent: want 403, got %d %s", rec.Code, rec.Body.String())
	}
	base := "/api/v1/merchants/" + m + "/kyb"
	if rec := doJSON(t, srv, owner, http.MethodPut, base, kybPGApplication()); rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	for _, d := range kyb.RequiredDocuments() {
		content := append([]byte("\x89PNG\r\n\x1a\n"), []byte("synthetic "+string(d))...)
		if rec := uploadPGDoc(t, srv, owner, m, string(d), content); rec.Code != http.StatusCreated {
			t.Fatalf("upload %s: %d %s", d, rec.Code, rec.Body.String())
		}
	}
	// A loaded 69-B list in sat_blacklist (without one the check is UNKNOWN).
	if err := store.NewSQLBlacklistRepository(db).AddEntry(context.Background(),
		identity.BlacklistEntry{RFC: "EFO110101XYZ", Status: identity.BlacklistDefinitivo, OficioSAT: "500-05-2023-1234"}); err != nil {
		t.Fatal(err)
	}
	rec := doJSON(t, srv, owner, http.MethodPost, base+"/submit", nil)
	var resp api.KYBResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != http.StatusOK || resp.Status != kyb.StatusInReview || !resp.Checks.DocumentsComplete || resp.Checks.List69B != kyb.List69BClear {
		t.Fatalf("submit: %d %s", rec.Code, rec.Body.String())
	}
	_ = db.Close()

	srv2, db2, _, err := BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildServer (restart): %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	kybRec, found, err := srv2.MerchantKYB(context.Background(), m)
	if err != nil || !found || kybRec.Status != kyb.StatusInReview || kybRec.IdentityCheck != kyb.IdentityPassed {
		t.Fatalf("kyb after restart: %+v %v", kybRec, err)
	}
	approved, err := srv2.ApproveMerchantKYB(context.Background(), m, "usr_operator_pg")
	if err != nil || approved.Status != kyb.StatusApproved || approved.AcquirerMerchantID == "" {
		t.Fatalf("approve: %+v %v", approved, err)
	}
	if clabe, ok := srv2.GetMerchantCLABE(m); !ok || len(clabe) != 18 {
		t.Fatalf("approval must assign the CLABE, got %q", clabe)
	}
	if rec := intent(srv2, "after"); rec.Code != http.StatusCreated {
		t.Fatalf("approved merchant intent: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, srv2, owner, http.MethodPost, "/api/v1/payments/codi/qr", map[string]any{"amount_cents": 1000, "concept": "x", "reference": "r1"}); rec.Code != http.StatusOK {
		t.Fatalf("approved merchant CoDi QR: %d %s", rec.Code, rec.Body.String())
	}
}
