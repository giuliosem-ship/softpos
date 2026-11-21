package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"softpos/pkg/acquirer"
	"softpos/pkg/fiscal/fiscaltest"
	"softpos/pkg/store/pgtest"
	"softpos/pkg/stp"
)

// TestPGIntegration_CreditNoteJobsExactlyOnce: on PostgreSQL, a refund
// webhook delivered concurrently (same refund under several event ids)
// enqueues one credit-note job in the money transaction; concurrent worker
// runs issue exactly one CFDI de egreso; a chargeback opened and reversed
// before the worker ran is flagged and needs none.
func TestPGIntegration_CreditNoteJobsExactlyOnce(t *testing.T) {
	srv, m, tok, db := buildCardServerDB(t, func(c *Config) { c.AcquirerWebhookSecret = pgAcquirerSecret })
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
	_, intent := cardIntent(t, srv, tok, "pg-cn-1", 11600)
	for _, e := range mock.SimulateTap(intent.ProviderPaymentID, intent.CardPaymentID, 11600) {
		if code := deliverSigned(t, srv, e); code != http.StatusOK {
			t.Fatalf("tap %s: %d", e.Type, code)
		}
	}
	b, _ := json.Marshal(map[string]any{"sale_ids": []string{intent.CardPaymentID}, "receptor": map[string]string{
		"rfc": "EKU9003173C9", "nombre": "ESCUELA KEMPER URGATE", "regimen_fiscal": "601", "codigo_postal": "26015", "uso_cfdi": "G03"}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/merchants/"+m+"/invoices", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Idempotency-Key", "pg-cn-inv")
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated || !bytes.Contains(rec.Body.Bytes(), []byte(`"STAMPED"`)) {
		t.Fatalf("invoice: %d %s", rec.Code, rec.Body.String())
	}

	runConcurrently(6, func(i int) int {
		return deliverSigned(t, srv, acquirer.Event{ID: "evt_pg_cn_rf_" + string(rune('a'+i%3)), Type: acquirer.EventRefunded,
			ProviderPaymentID: intent.ProviderPaymentID, RefundID: "re_pg_cn", AmountCents: 5800})
	})
	if n := countRows(t, db, `SELECT COUNT(*) FROM credit_note_jobs WHERE sale_id = $1 AND source_kind = 'REFUND' AND source_id = 're_pg_cn'
		AND status = 'PENDING' AND amount_cents = 5800 AND net_after_cents = 5800`, intent.CardPaymentID); n != 1 {
		t.Fatalf("one job per refund, in the money transaction (got %d)", n)
	}
	// A refund rejected by the state machine (above the remainder) rolls
	// back with its job.
	if code := deliverSigned(t, srv, acquirer.Event{ID: "evt_pg_cn_over", Type: acquirer.EventRefunded,
		ProviderPaymentID: intent.ProviderPaymentID, RefundID: "re_pg_over", AmountCents: 99999}); code == http.StatusOK {
		t.Fatal("over-refund accepted")
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM credit_note_jobs WHERE source_id = 're_pg_over'`); n != 0 {
		t.Fatal("a rolled-back refund must not leave a job")
	}

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = srv.ProcessCreditNotes(context.Background(), time.Now())
		}()
	}
	wg.Wait()
	if n := countRows(t, db, `SELECT COUNT(*) FROM invoices WHERE kind = 'CREDIT_NOTE' AND status = 'STAMPED' AND serie = 'E'
		AND subtotal_cents = 5000 AND iva_cents = 800 AND isr_retenido_cents = 63 AND related_invoice_id <> ''`); n != 1 {
		t.Fatalf("exactly one stamped egreso (got %d)", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM credit_note_jobs j JOIN invoices i ON i.id = j.credit_note_invoice_id
		WHERE j.status = 'ISSUED' AND j.credited_cents = 5800`); n != 1 {
		t.Fatal("job not marked ISSUED with its credit note")
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM invoice_sales s JOIN invoices i ON i.id = s.invoice_id WHERE s.sale_id = $1 AND s.active`, intent.CardPaymentID); n != 1 {
		t.Fatal("the credit note must not hold the sale")
	}

	for _, typ := range []acquirer.EventType{acquirer.EventChargebackOpened, acquirer.EventChargebackReversed} {
		if code := deliverSigned(t, srv, acquirer.Event{ID: "evt_pg_cb_" + string(typ), Type: typ, ProviderPaymentID: intent.ProviderPaymentID,
			ChargebackID: "cb_pg", AmountCents: 1000}); code != http.StatusOK {
			t.Fatalf("%s: %d", typ, code)
		}
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM credit_note_jobs WHERE source_kind = 'CHARGEBACK' AND source_id = 'cb_pg'
		AND status = 'NOT_REQUIRED' AND flag = 'CHARGEBACK_REVERSED'`); n != 1 {
		t.Fatal("reversed chargeback job must be flagged and not required")
	}
}

// TestPGIntegration_GlobalInvoiceParts: on PostgreSQL a period with more
// sales than fit in one CFDI gets several live globals (one per part);
// concurrent worker runs never link a sale twice.
func TestPGIntegration_GlobalInvoiceParts(t *testing.T) {
	cfg := Config{SQLDriver: "pgx", DatabaseURL: pgtest.URL(t), STPHMACSecret: "test_secret", JWTSecret: testJWTSecret, VaultMasterKeyHex: testVaultKeyHex}
	srv, db, _, err := BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	const m, clabe, rfc = "merch_pg_parts", "646180123400000007", "CACX7605101P8"
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
	signer := stp.NewSTPSigner(nil, nil, cfg.STPHMACSecret)
	for i := 0; i < 5; i++ {
		clave := fmt.Sprintf("PGPART%04d", i)
		canonical := signer.BuildAbonoCanonicalString(clabe, 11600, clave, "40012", "012180001234567890")
		rec := doJSON(t, srv, "", http.MethodPost, "/api/v1/webhooks/stp/abono", map[string]any{
			"id": i + 1, "fechaOperacion": 20261120, "institucionOrdenante": "40012", "institucionBeneficiaria": "646",
			"claveRastreo": clave, "monto": json.Number("116.00"), "cuentaOrdenante": "012180001234567890",
			"cuentaBeneficiario": clabe, "firma": signer.SignHMAC(canonical)})
		if rec.Code != http.StatusOK {
			t.Fatalf("abono: %d %s", rec.Code, rec.Body.String())
		}
	}
	srv.SetGlobalInvoiceMaxConcepts(2)
	next := time.Now().AddDate(0, 1, 2)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = srv.RunGlobalInvoices(context.Background(), next)
		}()
	}
	wg.Wait()
	_, _ = srv.RunGlobalInvoices(context.Background(), next) // picks up parts a racing run skipped
	if n := countRows(t, db, `SELECT COUNT(DISTINCT global_part) FROM invoices WHERE merchant_id = $1 AND kind = 'GLOBAL' AND status = 'STAMPED'`, m); n != 3 {
		t.Fatalf("expected 3 stamped parts, got %d", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM invoice_sales WHERE merchant_id = $1 AND active`, m); n != 5 {
		t.Fatalf("each sale in exactly one active global (got %d links)", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM (SELECT sale_id FROM invoice_sales WHERE merchant_id = $1 AND active GROUP BY sale_id HAVING COUNT(*) > 1) d`, m); n != 0 {
		t.Fatal("a sale linked twice")
	}
}
