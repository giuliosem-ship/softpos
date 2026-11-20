package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"softpos/pkg/auth"
	"softpos/pkg/kyb"
	"softpos/pkg/store"
	"softpos/pkg/store/pgtest"
	"softpos/pkg/stp"
)

// TestPGIntegration_BackofficeReviewBlockAndAudit: the env-seeded bootstrap
// operator reviews a real onboarding on PostgreSQL (detail, document
// download, approval), blocks the merchant, searches its payments and ledger;
// every action lands in operator_audit_log.
func TestPGIntegration_BackofficeReviewBlockAndAudit(t *testing.T) {
	cfg := Config{
		SQLDriver: "pgx", DatabaseURL: pgtest.URL(t), STPHMACSecret: "test_secret", JWTSecret: testJWTSecret,
		VaultMasterKeyHex: testVaultKeyHex, KYCProvider: "mock", AcquirerProvider: "mock",
		AcquirerWebhookSecret: "bo_pg_acquirer_webhook_secret_32bytes", BootstrapOperatorPhone: "+525599990000",
	}
	srv, db, _, err := BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildServer: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	op, found, err := srv.AuthService().Repository().GetUserByPhone(ctx, "+525599990000")
	if err != nil || !found {
		t.Fatalf("bootstrap operator user: %v %v", found, err)
	}
	if ok, _ := srv.AuthService().IsPlatformOperator(ctx, op.ID); !ok {
		t.Fatal("bootstrap operator not granted")
	}
	opTok, _ := srv.AuthService().MintAccessToken(op.ID, auth.BackofficeScope, auth.RoleOperator)

	m, owner := registerDraftMerchantPG(t, srv, "+525511110002")
	base := "/api/v1/merchants/" + m + "/kyb"
	doJSON(t, srv, owner, http.MethodPut, base, kybPGApplication())
	for _, d := range kyb.RequiredDocuments() {
		if rec := uploadPGDoc(t, srv, owner, m, string(d), append([]byte("\x89PNG\r\n\x1a\n"), []byte("bo "+string(d))...)); rec.Code != http.StatusCreated {
			t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
		}
	}
	if rec := doJSON(t, srv, owner, http.MethodPost, base+"/submit", nil); rec.Code != http.StatusOK {
		t.Fatalf("submit: %d %s", rec.Code, rec.Body.String())
	}

	rec := doJSON(t, srv, opTok, http.MethodGet, "/api/v1/backoffice/merchants?kyb_status=IN_REVIEW", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), m) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, srv, opTok, http.MethodGet, "/api/v1/backoffice/merchants/"+m, nil)
	var detail struct {
		Documents []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"documents"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &detail)
	if rec.Code != http.StatusOK || len(detail.Documents) != 6 {
		t.Fatalf("detail: %d %s", rec.Code, rec.Body.String())
	}
	dl := doJSON(t, srv, opTok, http.MethodGet, "/api/v1/backoffice/merchants/"+m+"/documents/"+detail.Documents[0].ID, nil)
	if dl.Code != http.StatusOK || !strings.HasPrefix(dl.Body.String(), "\x89PNG") {
		t.Fatalf("download: %d", dl.Code)
	}
	if rec := doJSON(t, srv, opTok, http.MethodPost, "/api/v1/backoffice/merchants/"+m+"/kyb/approve", map[string]any{"note": "ok"}); rec.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	clabe, ok := srv.GetMerchantCLABE(m)
	if !ok || clabe == "" {
		t.Fatal("approval must assign the virtual CLABE")
	}

	// Money arrives, then the merchant is blocked: payouts and QR refused.
	signer := stp.NewSTPSigner(nil, nil, cfg.STPHMACSecret)
	canonical := signer.BuildAbonoCanonicalString(clabe, 40000, "SPEIBOPG0001", "40012", "012180001234567890")
	if rec := doJSON(t, srv, "", http.MethodPost, "/api/v1/webhooks/stp/abono", map[string]any{
		"id": 3, "claveRastreo": "SPEIBOPG0001", "monto": json.Number("400.00"), "institucionOrdenante": "40012",
		"cuentaOrdenante": "012180001234567890", "cuentaBeneficiario": clabe, "firma": signer.SignHMAC(canonical),
	}); rec.Code != http.StatusOK {
		t.Fatalf("abono: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, srv, opTok, http.MethodPost, "/api/v1/backoffice/merchants/"+m+"/block", map[string]any{"reason": "PLD alert"}); rec.Code != http.StatusOK {
		t.Fatalf("block: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, srv, owner, http.MethodPost, "/api/v1/payments/codi/qr", map[string]any{"amount_cents": 1000, "concept": "x", "reference": "r"}); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "MERCHANT_BLOCKED") {
		t.Fatalf("QR while blocked: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, srv, opTok, http.MethodGet, "/api/v1/backoffice/payments?merchant_id="+m, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "SPEIBOPG0001") {
		t.Fatalf("payments: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, srv, opTok, http.MethodGet, "/api/v1/backoffice/merchants/"+m+"/ledger", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"available_cents":40000`) {
		t.Fatalf("ledger: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, srv, opTok, http.MethodGet, "/api/v1/backoffice/payouts", nil); rec.Code != http.StatusOK {
		t.Fatalf("payouts: %d %s", rec.Code, rec.Body.String())
	}

	rows, err := store.NewSQLAuditRepository(db).ListAudit(ctx, store.AuditQuery{MerchantID: m})
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for i := len(rows) - 1; i >= 0; i-- {
		actions = append(actions, rows[i].Action)
		if rows[i].OperatorID != op.ID {
			t.Fatalf("audit operator = %s", rows[i].OperatorID)
		}
	}
	if got := strings.Join(actions, ","); got != "kyb.view,document.download,kyb.approve,merchant.block,ledger.view" {
		t.Fatalf("audit trail = %s", got)
	}
	var blockAfter string
	if err := db.QueryRowContext(ctx, `SELECT after_json::text FROM operator_audit_log WHERE action = 'merchant.block'`).Scan(&blockAfter); err != nil || !strings.Contains(blockAfter, `"blocked": true`) {
		t.Fatalf("block after_json = %q %v", blockAfter, err)
	}

	// The bootstrap is idempotent across restarts.
	srv2, db2, _, err := BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	_ = db2.Close()
	_ = srv2
}
