package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"softpos/pkg/acquirer"
	"softpos/pkg/api"
	"softpos/pkg/auth"
	"softpos/pkg/fiscal/fiscaltest"
	"softpos/pkg/store/pgtest"
)

const pgReceiptSecret = "pg_receipt_token_secret_32_bytes_long!!"

// TestPGIntegration_ReceiptAndAutofactura: on PostgreSQL the card details
// of the processor webhook are stored (last 4 only), the receipt link
// survives a restart, and concurrent autofactura requests of one sale yield
// exactly one invoice.
func TestPGIntegration_ReceiptAndAutofactura(t *testing.T) {
	cfg := Config{SQLDriver: "pgx", DatabaseURL: pgtest.URL(t), STPHMACSecret: "test_secret", JWTSecret: testJWTSecret,
		VaultMasterKeyHex: testVaultKeyHex, AcquirerProvider: "mock", AcquirerWebhookSecret: pgAcquirerSecret, ReceiptTokenSecret: pgReceiptSecret}
	srv, db, _, err := BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	const m = "merch_pg_rcpt"
	if err := srv.RegisterMerchantCLABE("646180123400000007", m, "GARM850101ABC"); err != nil {
		t.Fatal(err)
	}
	tok, _ := srv.AuthService().MintAccessToken("usr_pg_rcpt", m, auth.RoleOwner)
	c, err := fiscaltest.NewCSD(fiscaltest.CSDOptions{RFC: "GARM850101ABC"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.RegisterMerchantCSD(m, c.PlainKey, c.CertDER); err != nil {
		t.Fatal(err)
	}
	if err := srv.SeedFiscalSettings(context.Background(), m, "06000"); err != nil {
		t.Fatal(err)
	}
	mock := srv.AcquirerProvider().(*acquirer.MockProvider)
	_, intent := cardIntent(t, srv, tok, "pg-rcpt-1", 11600)
	for _, e := range mock.SimulateTap(intent.ProviderPaymentID, intent.CardPaymentID, 11600) {
		if code := deliverSigned(t, srv, e); code != http.StatusOK {
			t.Fatalf("tap %s: %d", e.Type, code)
		}
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM card_payments WHERE id = $1 AND card_last4 = '4242' AND card_brand = 'visa' AND auth_code <> ''`, intent.CardPaymentID); n != 1 {
		t.Fatal("card details not stored")
	}
	rec := doJSON(t, srv, tok, http.MethodGet, "/api/v1/merchants/"+m+"/sales/"+intent.CardPaymentID, nil)
	var d api.SaleDetail
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	if rec.Code != http.StatusOK || !strings.HasPrefix(d.ReceiptURL, "/r/") {
		t.Fatalf("sale detail: %d %s", rec.Code, rec.Body.String())
	}

	receptor, _ := json.Marshal(map[string]string{"rfc": "EKU9003173C9", "nombre": "ESCUELA KEMPER URGATE", "regimen_fiscal": "601", "codigo_postal": "26015", "uso_cfdi": "G03"})
	ids := make([]string, 6)
	codes := runConcurrently(6, func(i int) int {
		req := httptest.NewRequest(http.MethodPost, d.ReceiptURL+"/invoice", bytes.NewReader(receptor))
		rec := httptest.NewRecorder()
		srv.Router().ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		ids[i], _ = out["invoice_id"].(string)
		return rec.Code
	})
	for i, code := range codes {
		if code != http.StatusCreated && code != http.StatusConflict {
			t.Fatalf("autofactura %d: %d", i, code)
		}
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM invoice_sales WHERE sale_id = $1 AND active`, intent.CardPaymentID); n != 1 {
		t.Fatalf("one active invoice per sale (got %d)", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM sale_receipts WHERE sale_id = $1 AND invoice_id <> ''`, intent.CardPaymentID); n != 1 {
		t.Fatal("receipt must record its autofactura invoice")
	}
	_ = db.Close()

	srv2, db2, _, err := BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	req := httptest.NewRequest(http.MethodGet, d.ReceiptURL+"?format=json", nil)
	rec = httptest.NewRecorder()
	srv2.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"masked_pan":"**** 4242"`) || !strings.Contains(rec.Body.String(), `"STAMPED"`) {
		t.Fatalf("receipt after restart: %d %s", rec.Code, rec.Body.String())
	}
}
