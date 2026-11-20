package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"softpos/pkg/ledger"
	"softpos/pkg/payments"
	"softpos/pkg/store"
)

func availableCents(t *testing.T, s *Server, merchantID string) int64 {
	t.Helper()
	bal, err := s.ledger.GetBalance("LIABILITY:MERCHANT:" + merchantID + ":AVAILABLE")
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	return bal.AmountCents
}

func memEvents(t *testing.T, s *Server, eventType string) []store.MemoryOutboxEvent {
	t.Helper()
	ms, ok := s.moneyStore().(*store.MemoryMoneyStore)
	if !ok {
		t.Fatalf("expected in-memory money store, got %T", s.moneyStore())
	}
	var out []store.MemoryOutboxEvent
	for _, e := range ms.Events() {
		if e.EventType == eventType {
			out = append(out, e)
		}
	}
	return out
}

func TestSettle_RequiresAuthorizedPaymentOfTenant(t *testing.T) {
	s, _ := setupTestServer(t)
	const m, other = "merch_settle_rules", "merch_settle_other"
	_ = s.RegisterMerchant(m, "GARM850101ABC", "M", true)
	_ = s.RegisterMerchant(other, "XAXX010101000", "O", true)
	authorizeTestPayment(t, s, m, "pay_ok", 11600)
	authorizeTestPayment(t, s, other, "pay_other", 11600)
	authorizeTestPayment(t, s, m, "pay_void", 11600)
	if _, err := s.paymentEngine.Void("pay_void"); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		body map[string]any
		want int
		code string
	}{
		{"unknown payment", map[string]any{"payment_id": "nope", "gross_amount_cents": 999999}, http.StatusNotFound, "PAYMENT_NOT_FOUND"},
		{"other tenant's payment", map[string]any{"payment_id": "pay_other"}, http.StatusNotFound, "PAYMENT_NOT_FOUND"},
		{"voided payment", map[string]any{"payment_id": "pay_void"}, http.StatusConflict, "INVALID_PAYMENT_STATE"},
		{"inflated gross", map[string]any{"payment_id": "pay_ok", "gross_amount_cents": 99999999}, http.StatusConflict, "AMOUNT_MISMATCH"},
		{"missing id", map[string]any{"gross_amount_cents": 100}, http.StatusBadRequest, "MISSING_PAYMENT_ID"},
	}
	for _, c := range cases {
		rec := serve(s, asTenant(t, s, m, jsonReq("POST", "/api/v1/payments/settle", c.body)))
		if rec.Code != c.want || !strings.Contains(rec.Body.String(), c.code) {
			t.Errorf("%s: want %d %s, got %d %s", c.name, c.want, c.code, rec.Code, rec.Body.String())
		}
	}
	if got := availableCents(t, s, m); got != 0 {
		t.Fatalf("rejected settles must not credit, available=%d", got)
	}
	if got := len(memEvents(t, s, "payment.settled")); got != 0 {
		t.Fatalf("rejected settles must not enqueue events, got %d", got)
	}
}

func TestSettle_DoubleSettleReturnsSameResultAndCreditsOnce(t *testing.T) {
	s, _ := setupTestServer(t)
	const m = "merch_settle_twice"
	_ = s.RegisterMerchant(m, "GARM850101ABC", "M", true)
	authorizeTestPayment(t, s, m, "pay_twice", 11600)

	first := serve(s, asTenant(t, s, m, jsonReq("POST", "/api/v1/payments/settle", map[string]any{"payment_id": "pay_twice"})))
	second := serve(s, asTenant(t, s, m, jsonReq("POST", "/api/v1/payments/settle", map[string]any{"payment_id": "pay_twice"})))
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("settle: %d %s / %d %s", first.Code, first.Body.String(), second.Code, second.Body.String())
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("double settle must return the stored result:\n%s\n%s", first.Body.String(), second.Body.String())
	}
	var resp SettlePaymentResponse
	_ = json.Unmarshal(first.Body.Bytes(), &resp)
	// 11600 gross, default 200 bps fee = 232, IVA 1600 + ISR 1% of 10000 = 100.
	if resp.PlatformFeeCents != 232 || resp.TaxEscrowCents != 1700 || resp.NetAvailableCents != 9668 {
		t.Fatalf("unexpected split: %+v", resp)
	}
	if got := availableCents(t, s, m); got != 9668 {
		t.Fatalf("available after double settle = %d, want 9668", got)
	}
	if got := s.GetMerchantMonthlySalesCents(m); got != 10000 {
		t.Fatalf("monthly accumulator = %d, want 10000", got)
	}
	ev := memEvents(t, s, "payment.settled")
	if len(ev) != 1 || ev[0].AggregateID != "pay_twice" {
		t.Fatalf("want exactly one payment.settled event, got %+v", ev)
	}
}

func TestSettle_ConcurrentSettlesCreditOnce(t *testing.T) {
	s, _ := setupTestServer(t)
	const m = "merch_settle_race"
	_ = s.RegisterMerchant(m, "GARM850101ABC", "M", true)
	authorizeTestPayment(t, s, m, "pay_race", 50000)
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = serve(s, asTenant(t, s, m, jsonReq("POST", "/api/v1/payments/settle", map[string]any{"payment_id": "pay_race"}))).Code
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("settle %d: %d", i, c)
		}
	}
	gross := int64(50000)
	base := (gross*100 + 58) / 116
	want := gross - bpsHalfUp(gross, DefaultPlatformFeeBps) - (gross - base) - bpsHalfUp(base, 100)
	if got := availableCents(t, s, m); got != want {
		t.Fatalf("available = %d, want %d (credited once)", got, want)
	}
}

func TestSettle_PostingFailureRollsBackEverything(t *testing.T) {
	s, _ := setupTestServer(t)
	const m = "merch_no_accounts"
	// Merchant profile without ledger accounts: the journal cannot post.
	if err := s.merchantRepo.SaveMerchant(context.Background(), store.MerchantRecord{ID: m, RFC: "GARM850101ABC", Name: "No accounts", IsRESICO: true}); err != nil {
		t.Fatal(err)
	}
	authorizeTestPayment(t, s, m, "pay_fail", 11600)
	rec := serve(s, asTenant(t, s, m, jsonReq("POST", "/api/v1/payments/settle", map[string]any{"payment_id": "pay_fail"})))
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "LEDGER_POSTING_FAILED") {
		t.Fatalf("want 500 LEDGER_POSTING_FAILED, got %d %s", rec.Code, rec.Body.String())
	}
	p, _ := s.paymentEngine.GetPayment("pay_fail")
	if p.Status != payments.PaymentAuthorized {
		t.Fatalf("payment status must roll back, got %s", p.Status)
	}
	if got := s.GetMerchantMonthlySalesCents(m); got != 0 {
		t.Fatalf("monthly accumulator must roll back, got %d", got)
	}
	if got := len(memEvents(t, s, "payment.settled")); got != 0 {
		t.Fatalf("no event on rollback, got %d", got)
	}
}

func signedAbono(t *testing.T, s *Server, clave, clabe string, cents int64) map[string]any {
	t.Helper()
	monto := fmt.Sprintf("%d.%02d", cents/100, cents%100)
	canonical := s.stpSigner.BuildAbonoCanonicalString(clabe, cents, clave, "40012", "012180001234567890")
	return map[string]any{
		"id": 1, "fechaOperacion": 20261120, "institucionOrdenante": "40012", "institucionBeneficiaria": "646",
		"claveRastreo": clave, "monto": json.Number(monto), "cuentaOrdenante": "012180001234567890",
		"cuentaBeneficiario": clabe, "firma": s.stpSigner.SignHMAC(canonical),
	}
}

func TestSTPAbono_ConcurrentDuplicatesCreditOnce(t *testing.T) {
	s, _ := setupTestServer(t)
	const m, clabe = "merch_abono_race", "646180000000000014"
	_ = s.RegisterMerchantCLABE(clabe, m, "GARM850101ABC")
	body := signedAbono(t, s, "SPEIRACE001", clabe, 50000)
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = serve(s, jsonReq("POST", "/api/v1/webhooks/stp/abono", body)).Code
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("abono %d: %d", i, c)
		}
	}
	if got := availableCents(t, s, m); got != 50000 {
		t.Fatalf("available = %d, want 50000 (credited once)", got)
	}
	if got := len(memEvents(t, s, "spei.abono_received")); got != 1 {
		t.Fatalf("want one spei.abono_received event, got %d", got)
	}
}

func TestCoDiConfirm_RetriesWithNewSignatureCreditOnce(t *testing.T) {
	s, _ := setupTestServer(t)
	const m, clabe = "merch_codi_once", "646180123400000007"
	_ = s.RegisterMerchantCLABE(clabe, m, "GARM850101ABC")
	s.SetCoDiWebhookSecret(testCoDiWebhookSecret)
	payload := `{"tracking_key":"BNXCODI2026112001","seller_clabe":"646180123400000007","buyer_clabe":"012180000118359719","amount_cents":11600,"timestamp":"2026-11-20T12:00:00Z","status":"APROBADO","banxico_approval_code":"AUT001"}`
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each retry is signed at a different second -> distinct signature.
			rec := serve(s, signedCoDiRequest(testCoDiWebhookSecret, time.Now().Add(-time.Duration(i)*time.Second), payload))
			if rec.Code != http.StatusOK {
				t.Errorf("retry %d: %d %s", i, rec.Code, rec.Body.String())
			}
		}(i)
	}
	wg.Wait()
	if got := availableCents(t, s, m); got != 9900 {
		t.Fatalf("available = %d, want 9900 (credited once)", got)
	}
	if got := s.GetMerchantMonthlySalesCents(m); got != 10000 {
		t.Fatalf("accumulator = %d, want 10000", got)
	}
	if got := len(memEvents(t, s, "codi.confirmed")); got != 1 {
		t.Fatalf("want one codi.confirmed event, got %d", got)
	}
}

func TestRefund_PostingFailureReturns5xxAndKeepsPaymentSettled(t *testing.T) {
	s, _ := setupTestServer(t)
	const m = "merch_refund_fail"
	_ = s.RegisterMerchant(m, "GARM850101ABC", "M", true)
	authorizeTestPayment(t, s, m, "pay_rf", 11600)
	if rec := serve(s, asTenant(t, s, m, jsonReq("POST", "/api/v1/payments/settle", map[string]any{"payment_id": "pay_rf"}))); rec.Code != http.StatusOK {
		t.Fatalf("settle: %d %s", rec.Code, rec.Body.String())
	}
	// The ledger refuses the reversal journal: the whole refund must roll back.
	s.SetMoneyStore(failingPostStore{inner: s.moneyStore()})
	rec := serve(s, asTenant(t, s, m, jsonReq("POST", "/api/v1/payments/emv/reverse", map[string]any{"payment_id": "pay_rf"})))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 when the reversal cannot post, got %d %s", rec.Code, rec.Body.String())
	}
	p, _ := s.paymentEngine.GetPayment("pay_rf")
	if p.Status != payments.PaymentSettled {
		t.Fatalf("payment must stay SETTLED after a failed refund, got %s", p.Status)
	}
}

// failingPostStore wraps a MoneyStore so every PostJournal fails.
type failingPostStore struct{ inner store.MoneyStore }

type failingPostTx struct{ store.MoneyTx }

func (f failingPostStore) InTx(ctx context.Context, fn func(tx store.MoneyTx) error) error {
	return f.inner.InTx(ctx, func(tx store.MoneyTx) error { return fn(failingPostTx{tx}) })
}

func (failingPostTx) PostJournal(context.Context, *ledger.JournalEntry) error {
	return errors.New("simulated ledger failure")
}
