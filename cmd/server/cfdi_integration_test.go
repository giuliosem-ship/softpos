package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"softpos/pkg/api"
	"softpos/pkg/auth"
	"softpos/pkg/fiscal/fiscaltest"
	"softpos/pkg/store/pgtest"
	"softpos/pkg/stp"
)

func csdMultipart(t *testing.T, c fiscaltest.CSD) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("cer", "csd.cer")
	_, _ = fw.Write(c.CertDER)
	fw, _ = mw.CreateFormFile("key", "csd.key")
	_, _ = fw.Write(c.EncryptedKey)
	_ = mw.WriteField("password", string(c.Password))
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

// TestPGIntegration_CSDUploadAndFiscalSettingsSurviveRestart: a CSD uploaded
// over HTTP and the fiscal settings persist in PostgreSQL across a reboot.
func TestPGIntegration_CSDUploadAndFiscalSettingsSurviveRestart(t *testing.T) {
	cfg := Config{SQLDriver: "pgx", DatabaseURL: pgtest.URL(t), STPHMACSecret: "test_secret", JWTSecret: testJWTSecret, VaultMasterKeyHex: testVaultKeyHex}
	srv, db, _, err := BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	const m, rfc = "merch_pg_cfdi", "CACX7605101P8"
	if err := srv.RegisterMerchant(m, rfc, "Xochilt Casas Chavez", true); err != nil {
		t.Fatal(err)
	}
	owner, _ := srv.AuthService().MintAccessToken("usr_"+m, m, auth.RoleOwner)
	c, err := fiscaltest.NewCSD(fiscaltest.CSDOptions{RFC: rfc})
	if err != nil {
		t.Fatal(err)
	}
	body, ct := csdMultipart(t, c)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/merchants/"+m+"/csd", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer "+owner)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	b, _ := json.Marshal(map[string]string{"clave_prod_serv": "90101501", "lugar_expedicion": "44100"})
	req = httptest.NewRequest(http.MethodPut, "/api/v1/merchants/"+m+"/fiscal-settings", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+owner)
	rec = httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("settings: %d %s", rec.Code, rec.Body.String())
	}
	_ = db.Close()

	srv2, db2, _, err := BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	owner2, _ := srv2.AuthService().MintAccessToken("usr_"+m, m, auth.RoleOwner)
	for path, want := range map[string]string{"/csd": `"serial":"30001000000500003416"`, "/fiscal-settings": `"lugar_expedicion":"44100"`} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/merchants/"+m+path, nil)
		req.Header.Set("Authorization", "Bearer "+owner2)
		rec := httptest.NewRecorder()
		srv2.Router().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(want)) {
			t.Fatalf("GET %s after restart: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	var n int
	if err := db2.QueryRow(`SELECT COUNT(*) FROM merchants WHERE id = $1 AND encrypted_csd_json::text NOT LIKE '%12345678a%'`, m).Scan(&n); err != nil || n != 1 {
		t.Fatalf("envelope must not hold the clear password: n=%d %v", n, err)
	}
}

func invoiceReq(t *testing.T, srv *api.Server, tok, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/merchants/merch_pg_inv/invoices", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Idempotency-Key", key)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	return rec
}

// TestPGIntegration_InvoiceLifecycle: on PostgreSQL, concurrent invoices for
// the same sale yield exactly one; cancellation frees the sale; the factura
// global covers the rest; everything survives a restart.
func TestPGIntegration_InvoiceLifecycle(t *testing.T) {
	cfg := Config{SQLDriver: "pgx", DatabaseURL: pgtest.URL(t), STPHMACSecret: "test_secret", JWTSecret: testJWTSecret, VaultMasterKeyHex: testVaultKeyHex}
	srv, db, _, err := BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	const m, clabe, rfc = "merch_pg_inv", "646180123400000007", "CACX7605101P8"
	if err := srv.RegisterMerchantCLABE(clabe, m, rfc); err != nil {
		t.Fatal(err)
	}
	c, _ := fiscaltest.NewCSD(fiscaltest.CSDOptions{RFC: rfc})
	if err := srv.RegisterMerchantCSD(m, c.PlainKey, c.CertDER); err != nil {
		t.Fatal(err)
	}
	if err := srv.SeedFiscalSettings(context.Background(), m, "06000"); err != nil {
		t.Fatal(err)
	}
	owner, _ := srv.AuthService().MintAccessToken("usr_"+m, m, auth.RoleOwner)
	signer := stp.NewSTPSigner(nil, nil, cfg.STPHMACSecret)
	abono := func(clave string, cents int64) string {
		canonical := signer.BuildAbonoCanonicalString(clabe, cents, clave, "40012", "012180001234567890")
		rec := doJSON(t, srv, "", http.MethodPost, "/api/v1/webhooks/stp/abono", map[string]any{
			"id": 1, "fechaOperacion": 20261120, "institucionOrdenante": "40012", "institucionBeneficiaria": "646",
			"claveRastreo": clave, "monto": json.Number(fmt.Sprintf("%d.%02d", cents/100, cents%100)), "cuentaOrdenante": "012180001234567890",
			"cuentaBeneficiario": clabe, "firma": signer.SignHMAC(canonical)})
		if rec.Code != http.StatusOK {
			t.Fatalf("abono: %d %s", rec.Code, rec.Body.String())
		}
		return "spei_40012:" + clave
	}
	s1, s2 := abono("PGINV0001", 11600), abono("PGINV0002", 23200)
	receptor := map[string]string{"rfc": "EKU9003173C9", "nombre": "ESCUELA KEMPER URGATE", "regimen_fiscal": "601", "codigo_postal": "26015", "uso_cfdi": "G03"}

	codes := runConcurrently(8, func(i int) int {
		return invoiceReq(t, srv, owner, fmt.Sprintf("race-%d", i), map[string]any{"sale_ids": []string{s1}, "receptor": receptor}).Code
	})
	created := 0
	for _, code := range codes {
		switch code {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}
	if created != 1 || countRows(t, db, `SELECT COUNT(*) FROM invoice_sales WHERE sale_id = $1 AND active`, s1) != 1 {
		t.Fatalf("concurrent invoices of one sale: %d created %v", created, codes)
	}
	var inv api.InvoiceResponse
	var invID string
	_ = db.QueryRow(`SELECT id FROM invoices WHERE merchant_id = $1`, m).Scan(&invID)
	rec := doJSON(t, srv, owner, http.MethodPost, "/api/v1/merchants/"+m+"/invoices/"+invID+"/cancel", map[string]string{"motivo": "02"})
	_ = json.Unmarshal(rec.Body.Bytes(), &inv)
	if rec.Code != http.StatusOK || inv.Status != "CANCELED" {
		t.Fatalf("cancel (<= $1,000): %d %s", rec.Code, rec.Body.String())
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM invoice_sales WHERE sale_id = $1 AND active`, s1); n != 0 {
		t.Fatalf("canceled invoice must free the sale (%d active links)", n)
	}
	if n, err := srv.RunGlobalInvoices(context.Background(), time.Now().AddDate(0, 1, 2)); err != nil || n != 1 {
		t.Fatalf("global: %d %v", n, err)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM invoices WHERE kind = 'GLOBAL' AND status = 'STAMPED' AND serie = 'G' AND folio = 1`); n != 1 {
		t.Fatal("global invoice not stamped")
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM invoice_sales s JOIN invoices i ON i.id = s.invoice_id WHERE i.kind = 'GLOBAL' AND s.active AND s.sale_id IN ($1, $2)`, s1, s2); n != 2 {
		t.Fatalf("global links: %d", n)
	}
	_ = db.Close()

	srv2, db2, _, err := BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	owner2, _ := srv2.AuthService().MintAccessToken("usr_"+m, m, auth.RoleOwner)
	rec = doJSON(t, srv2, owner2, http.MethodGet, "/api/v1/merchants/"+m+"/invoices", nil)
	var page api.InvoicePage
	_ = json.Unmarshal(rec.Body.Bytes(), &page)
	if rec.Code != http.StatusOK || len(page.Items) != 2 || page.Items[0].Kind != "GLOBAL" {
		t.Fatalf("after restart: %d %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, srv2, owner2, http.MethodGet, "/api/v1/merchants/"+m+"/invoices/"+page.Items[0].InvoiceID+"/pdf", nil)
	if rec.Code != http.StatusOK || !bytes.HasPrefix(rec.Body.Bytes(), []byte("%PDF")) {
		t.Fatalf("pdf after restart: %d", rec.Code)
	}
	if n, _ := srv2.RunGlobalInvoices(context.Background(), time.Now().AddDate(0, 1, 2)); n != 0 {
		t.Fatal("global must not be issued twice")
	}
}
