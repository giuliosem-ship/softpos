package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"softpos/pkg/acquirer"
	"softpos/pkg/auth"
	"softpos/pkg/store"
)

const cardMerchant = "merch_card_01"

func setupCardServer(t *testing.T) (*Server, *acquirer.MockProvider) {
	t.Helper()
	s, _ := setupTestServer(t)
	if err := s.RegisterMerchant(cardMerchant, "GARM850101ABC", "Card Shop", true); err != nil {
		t.Fatalf("register: %v", err)
	}
	mock := acquirer.NewMockProvider(acquirer.MockConfig{})
	s.SetAcquirerProvider(mock)
	return s, mock
}

func createIntent(t *testing.T, s *Server, merchant, key string, body map[string]any) (int, CardPaymentIntentResponse, string) {
	t.Helper()
	req := jsonReq(http.MethodPost, "/api/v1/card-payments/intents", body)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := serve(s, asTenant(t, s, merchant, req))
	var out CardPaymentIntentResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Body.String()
}

func getCardPayment(t *testing.T, s *Server, merchant, id string) (int, CardPaymentResponse) {
	t.Helper()
	rec := serve(s, asTenant(t, s, merchant, jsonReq(http.MethodGet, "/api/v1/card-payments/"+id, nil)))
	var out CardPaymentResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func applyEvents(t *testing.T, s *Server, events ...acquirer.Event) {
	t.Helper()
	for _, e := range events {
		if _, err := s.ApplyAcquirerEvent(context.Background(), acquirer.MockProviderName, e); err != nil {
			t.Fatalf("apply %s: %v", e.Type, err)
		}
	}
}

func TestCardIntent_ValidatesAndUsesTokenTenant(t *testing.T) {
	s, _ := setupCardServer(t)
	cases := []struct {
		name string
		key  string
		body map[string]any
		code int
		err  string
	}{
		{"missing idempotency key", "", map[string]any{"amount_cents": 1000}, 400, "MISSING_IDEMPOTENCY_KEY"},
		{"zero amount", "k1", map[string]any{"amount_cents": 0}, 400, "INVALID_AMOUNT"},
		{"negative amount", "k2", map[string]any{"amount_cents": -5}, 400, "INVALID_AMOUNT"},
		{"above max", "k3", map[string]any{"amount_cents": MaxCardPaymentCents + 1}, 400, "INVALID_AMOUNT"},
		{"currency", "k4", map[string]any{"amount_cents": 1000, "currency": "USD"}, 400, "UNSUPPORTED_CURRENCY"},
		{"foreign merchant in body", "k5", map[string]any{"amount_cents": 1000, "merchant_id": "merch_other"}, 403, "TENANT_MISMATCH"},
	}
	for _, c := range cases {
		code, _, body := createIntent(t, s, cardMerchant, c.key, c.body)
		if code != c.code || !contains(body, c.err) {
			t.Errorf("%s: want %d %s, got %d %s", c.name, c.code, c.err, code, body)
		}
	}
	// No token -> 401 (route is not public).
	if rec := serve(s, jsonReq(http.MethodPost, "/api/v1/card-payments/intents", map[string]any{"amount_cents": 1000})); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: want 401, got %d", rec.Code)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func TestCardIntent_CreatesProviderIntentIdempotently(t *testing.T) {
	s, mock := setupCardServer(t)
	code, first, body := createIntent(t, s, cardMerchant, "tap-001", map[string]any{"amount_cents": 25000, "reference": "mesa 4"})
	if code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d %s", code, body)
	}
	if first.CardPaymentID == "" || first.ClientSecret == "" || first.ProviderPaymentID == "" ||
		first.Provider != "mock" || first.Status != store.CardCreated || first.AmountCents != 25000 || first.Currency != "MXN" {
		t.Fatalf("unexpected intent: %+v", first)
	}
	code, again, _ := createIntent(t, s, cardMerchant, "tap-001", map[string]any{"amount_cents": 25000, "reference": "mesa 4"})
	if code != http.StatusOK || again.CardPaymentID != first.CardPaymentID || again.ProviderPaymentID != first.ProviderPaymentID || again.ClientSecret != first.ClientSecret {
		t.Fatalf("replay must return the same intent: %d %+v vs %+v", code, again, first)
	}
	if code, _, body := createIntent(t, s, cardMerchant, "tap-001", map[string]any{"amount_cents": 99900}); code != http.StatusConflict || !contains(body, "IDEMPOTENCY_KEY_REUSED") {
		t.Fatalf("same key, other amount: want 409, got %d %s", code, body)
	}
	// Another merchant using the same key gets its own payment.
	if err := s.RegisterMerchant("merch_card_02", "GARM850101ABC", "Other", true); err != nil {
		t.Fatal(err)
	}
	if code, other, _ := createIntent(t, s, "merch_card_02", "tap-001", map[string]any{"amount_cents": 25000}); code != http.StatusCreated || other.CardPaymentID == first.CardPaymentID {
		t.Fatalf("keys are merchant-scoped: %d %+v", code, other)
	}
	if mock.Calls("CreatePaymentIntent") != 3 {
		t.Fatalf("provider calls: want 3 (create, replay, other merchant), got %d", mock.Calls("CreatePaymentIntent"))
	}

	code, got := getCardPayment(t, s, cardMerchant, first.CardPaymentID)
	if code != http.StatusOK || got.Status != store.CardCreated || got.ProviderPaymentID != first.ProviderPaymentID || got.Final || got.Settlement != nil {
		t.Fatalf("get: %d %+v", code, got)
	}
	if code, _ := getCardPayment(t, s, "merch_card_02", first.CardPaymentID); code != http.StatusNotFound {
		t.Fatalf("foreign tenant get: want 404, got %d", code)
	}
}

func TestCardIntent_ProviderTimeoutKeepsPaymentCreated(t *testing.T) {
	s, mock := setupCardServer(t)
	amount := int64(10000 + acquirer.MockTimeoutSuffix)
	code, _, body := createIntent(t, s, cardMerchant, "tap-timeout", map[string]any{"amount_cents": amount})
	if code != http.StatusBadGateway || !contains(body, "ACQUIRER_UNAVAILABLE") {
		t.Fatalf("timeout: want 502 ACQUIRER_UNAVAILABLE, got %d %s", code, body)
	}
	code, _, _ = createIntent(t, s, cardMerchant, "tap-timeout", map[string]any{"amount_cents": amount})
	if code != http.StatusBadGateway || mock.Calls("CreatePaymentIntent") != 2 {
		t.Fatalf("retry with same key must re-ask the provider: %d calls=%d", code, mock.Calls("CreatePaymentIntent"))
	}
}

func TestCardIntent_NotConfigured(t *testing.T) {
	s, _ := setupTestServer(t)
	_ = s.RegisterMerchant(cardMerchant, "GARM850101ABC", "Card Shop", true)
	if code, _, _ := createIntent(t, s, cardMerchant, "k", map[string]any{"amount_cents": 1000}); code != http.StatusServiceUnavailable {
		t.Fatalf("no provider: want 503, got %d", code)
	}
}

func TestCardCapture_SettlesThroughProviderConfirmationOnly(t *testing.T) {
	s, mock := setupCardServer(t)
	_, intent, _ := createIntent(t, s, cardMerchant, "tap-cap", map[string]any{"amount_cents": 11600})
	avail := "LIABILITY:MERCHANT:" + cardMerchant + ":AVAILABLE"
	if balanceOf(t, s, avail) != 0 {
		t.Fatal("nothing may be credited before the processor confirms")
	}
	events := mock.SimulateTap(intent.ProviderPaymentID, intent.CardPaymentID, 11600)
	if len(events) != 2 || events[0].Type != acquirer.EventAuthorized || events[1].Type != acquirer.EventCaptured {
		t.Fatalf("mock approve scenario: %+v", events)
	}
	applyEvents(t, s, events[0])
	if _, got := getCardPayment(t, s, cardMerchant, intent.CardPaymentID); got.Status != store.CardAuthorized || balanceOf(t, s, avail) != 0 {
		t.Fatalf("authorized must not move money: %+v", got)
	}
	applyEvents(t, s, events[1])
	_, got := getCardPayment(t, s, cardMerchant, intent.CardPaymentID)
	if got.Status != store.CardCaptured || got.CapturedCents != 11600 || !got.Final || got.Settlement == nil {
		t.Fatalf("captured: %+v", got)
	}
	// Same split as the legacy settle: fee 2% of gross, IVA+ISR set aside.
	st := got.Settlement
	if st.GrossAmountCents != 11600 || st.PlatformFeeCents != 232 || st.NetAvailableCents != 11600-232-st.TaxEscrowCents {
		t.Fatalf("settlement split: %+v", st)
	}
	if balanceOf(t, s, avail) != st.NetAvailableCents {
		t.Fatalf("available: want %d, got %d", st.NetAvailableCents, balanceOf(t, s, avail))
	}
	fee := mock.FeeCents(11600)
	if balanceOf(t, s, AccountAcquirerFees) != fee || balanceOf(t, s, AccountAcquirerReceivable) != 11600-fee {
		t.Fatalf("processor fee: expense %d receivable %d (fee %d)", balanceOf(t, s, AccountAcquirerFees), balanceOf(t, s, AccountAcquirerReceivable), fee)
	}
	if s.GetMerchantMonthlySalesCents(cardMerchant) != st.BaseAmountCents {
		t.Fatalf("RESICO accumulator: %d", s.GetMerchantMonthlySalesCents(cardMerchant))
	}
	if n := len(memEvents(t, s, "card_payment.captured")); n != 1 {
		t.Fatalf("outbox card_payment.captured: %d", n)
	}

	// Duplicate delivery and a late (out-of-order) authorized event change nothing.
	out, err := s.ApplyAcquirerEvent(context.Background(), "mock", events[1])
	if err != nil || !contains(string(out), `"applied"`) {
		t.Fatalf("duplicate must replay the first result: %s %v", out, err)
	}
	late := events[0]
	late.ID = "evt_auth_late"
	out, err = s.ApplyAcquirerEvent(context.Background(), "mock", late)
	if err != nil || !contains(string(out), `"ignored"`) {
		t.Fatalf("regression must be ignored: %s %v", out, err)
	}
	if _, got := getCardPayment(t, s, cardMerchant, intent.CardPaymentID); got.Status != store.CardCaptured {
		t.Fatalf("status regressed: %s", got.Status)
	}
	if balanceOf(t, s, avail) != st.NetAvailableCents || len(memEvents(t, s, "card_payment.captured")) != 1 {
		t.Fatal("capture posted more than once")
	}
}

func TestCardCapture_OutOfOrderCaptureBeforeAuthorize(t *testing.T) {
	s, mock := setupCardServer(t)
	_, intent, _ := createIntent(t, s, cardMerchant, "tap-ooo", map[string]any{"amount_cents": 5000})
	ev := mock.SimulateTap(intent.ProviderPaymentID, intent.CardPaymentID, 5000)
	applyEvents(t, s, ev[1], ev[0]) // captured, then authorized
	if _, got := getCardPayment(t, s, cardMerchant, intent.CardPaymentID); got.Status != store.CardCaptured {
		t.Fatalf("want CAPTURED, got %s", got.Status)
	}
}

func TestCardCapture_DeclineAndConflicts(t *testing.T) {
	s, mock := setupCardServer(t)
	amount := int64(20000 + acquirer.MockDeclineSuffix)
	_, intent, _ := createIntent(t, s, cardMerchant, "tap-decl", map[string]any{"amount_cents": amount})
	ev := mock.SimulateTap(intent.ProviderPaymentID, intent.CardPaymentID, amount)
	if len(ev) != 1 || ev[0].Type != acquirer.EventDeclined {
		t.Fatalf("decline scenario: %+v", ev)
	}
	applyEvents(t, s, ev...)
	_, got := getCardPayment(t, s, cardMerchant, intent.CardPaymentID)
	if got.Status != store.CardDeclined || got.DeclineCode != "do_not_honor" || !got.Final {
		t.Fatalf("declined: %+v", got)
	}
	if balanceOf(t, s, "LIABILITY:MERCHANT:"+cardMerchant+":AVAILABLE") != 0 {
		t.Fatal("declined payment credited")
	}

	// Capture above the intent amount is a conflict (not applied, not consumed).
	_, i2, _ := createIntent(t, s, cardMerchant, "tap-over", map[string]any{"amount_cents": 1000})
	over := acquirer.Event{ID: "evt_over", Type: acquirer.EventCaptured, ProviderPaymentID: i2.ProviderPaymentID, AmountCents: 1001}
	if _, err := s.ApplyAcquirerEvent(context.Background(), "mock", over); !errors.Is(err, errEventConflict) {
		t.Fatalf("over-capture: want errEventConflict, got %v", err)
	}
	// Unknown payment.
	unknown := acquirer.Event{ID: "evt_unknown", Type: acquirer.EventAuthorized, ProviderPaymentID: "mock_pi_nope"}
	if _, err := s.ApplyAcquirerEvent(context.Background(), "mock", unknown); !errors.Is(err, errUnknownCardPayment) {
		t.Fatalf("unknown: want errUnknownCardPayment, got %v", err)
	}
}

func TestCardCancel(t *testing.T) {
	s, mock := setupCardServer(t)
	// Provider never created the intent (timeout): canceled locally.
	tAmount := int64(3000 + acquirer.MockTimeoutSuffix)
	createIntent(t, s, cardMerchant, "tap-c1", map[string]any{"amount_cents": tAmount})
	var localID string
	_ = s.moneyStore().InTx(context.Background(), func(tx store.MoneyTx) error {
		cp, _, _ := tx.CreateCardPayment(context.Background(), store.CardPaymentRecord{MerchantID: cardMerchant, IntentIdempotencyKey: "tap-c1"})
		localID = cp.ID
		return nil
	})
	rec := serve(s, asRole(t, s, cardMerchant, auth.RoleCashier, jsonReq(http.MethodPost, "/api/v1/card-payments/"+localID+"/cancel", nil)))
	if rec.Code != http.StatusOK || !contains(rec.Body.String(), `"CANCELED"`) {
		t.Fatalf("local cancel: %d %s", rec.Code, rec.Body.String())
	}

	// Intent at the provider: cancel is requested, status moves on the webhook.
	_, intent, _ := createIntent(t, s, cardMerchant, "tap-c2", map[string]any{"amount_cents": 3000})
	rec = serve(s, asTenant(t, s, cardMerchant, jsonReq(http.MethodPost, "/api/v1/card-payments/"+intent.CardPaymentID+"/cancel", nil)))
	if rec.Code != http.StatusAccepted || mock.Calls("Cancel") != 1 {
		t.Fatalf("provider cancel: %d %s", rec.Code, rec.Body.String())
	}
	if _, got := getCardPayment(t, s, cardMerchant, intent.CardPaymentID); got.Status != store.CardCreated {
		t.Fatalf("status must wait for the webhook, got %s", got.Status)
	}
	applyEvents(t, s, acquirer.Event{ID: "evt_cancel_1", Type: acquirer.EventCanceled, ProviderPaymentID: intent.ProviderPaymentID})
	if _, got := getCardPayment(t, s, cardMerchant, intent.CardPaymentID); got.Status != store.CardCanceled {
		t.Fatalf("canceled webhook: %s", got.Status)
	}
	// A capture after the cancellation is a conflict.
	capt := acquirer.Event{ID: "evt_cap_after_cancel", Type: acquirer.EventCaptured, ProviderPaymentID: intent.ProviderPaymentID, AmountCents: 3000}
	if _, err := s.ApplyAcquirerEvent(context.Background(), "mock", capt); !errors.Is(err, errEventConflict) {
		t.Fatalf("capture after cancel: want conflict, got %v", err)
	}
	if rec := serve(s, asTenant(t, s, cardMerchant, jsonReq(http.MethodPost, "/api/v1/card-payments/"+intent.CardPaymentID+"/cancel", nil))); rec.Code != http.StatusConflict {
		t.Fatalf("cancel of canceled: want 409, got %d", rec.Code)
	}
}

func TestLegacyCardFlow_DisabledAnswers410(t *testing.T) {
	s, _ := setupCardServer(t)
	s.SetLegacyCardFlowEnabled(false)
	for _, path := range []string{"/api/v1/payments/emv/authorize", "/api/v1/payments/settle", "/api/v1/payments/emv/reverse"} {
		rec := serve(s, asTenant(t, s, cardMerchant, jsonReq(http.MethodPost, path, map[string]any{"payment_id": "p1"})))
		if rec.Code != http.StatusGone || !contains(rec.Body.String(), "LEGACY_CARD_FLOW_DISABLED") {
			t.Errorf("%s: want 410, got %d %s", path, rec.Code, rec.Body.String())
		}
	}
}
