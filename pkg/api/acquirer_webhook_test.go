package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"softpos/pkg/acquirer"
	"softpos/pkg/auth"
	"softpos/pkg/store"
)

const testAcquirerSecret = "acquirer_webhook_test_secret_32_bytes!!"

func signedAcquirerRequest(secret string, ts time.Time, provider string, e any) *http.Request {
	body, _ := json.Marshal(e)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/acquirer/"+provider, strings.NewReader(string(body)))
	req.Header.Set(WebhookTimestampHeader, strconv.FormatInt(ts.Unix(), 10))
	req.Header.Set(WebhookSignatureHeader, SignWebhook([]byte(secret), ts.Unix(), body))
	return req
}

func deliver(t *testing.T, s *Server, e acquirer.Event) (int, string) {
	t.Helper()
	rec := serve(s, signedAcquirerRequest(testAcquirerSecret, time.Now(), "mock", e))
	return rec.Code, rec.Body.String()
}

// capturedCardPayment creates an intent and delivers authorized + captured.
func capturedCardPayment(t *testing.T, s *Server, mock *acquirer.MockProvider, key string, amount int64) CardPaymentIntentResponse {
	t.Helper()
	code, intent, body := createIntent(t, s, cardMerchant, key, map[string]any{"amount_cents": amount})
	if code != http.StatusCreated {
		t.Fatalf("intent: %d %s", code, body)
	}
	for _, e := range mock.SimulateTap(intent.ProviderPaymentID, intent.CardPaymentID, amount) {
		if code, body := deliver(t, s, e); code != http.StatusOK {
			t.Fatalf("deliver %s: %d %s", e.Type, code, body)
		}
	}
	return intent
}

func requestRefund(t *testing.T, s *Server, role auth.Role, id, key string, amount int64) (int, CardRefundResponse, string) {
	t.Helper()
	req := jsonReq(http.MethodPost, "/api/v1/card-payments/"+id+"/refunds", map[string]any{"amount_cents": amount})
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := serve(s, asRole(t, s, cardMerchant, role, req))
	var out CardRefundResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Body.String()
}

func setupWebhookCardServer(t *testing.T) (*Server, *acquirer.MockProvider) {
	t.Helper()
	s, mock := setupCardServer(t)
	s.SetAcquirerWebhookSecret(testAcquirerSecret)
	return s, mock
}

func TestAcquirerWebhook_SignatureAndRouting(t *testing.T) {
	s, mock := setupWebhookCardServer(t)
	_, intent, _ := createIntent(t, s, cardMerchant, "sig-1", map[string]any{"amount_cents": 5000})
	ev := mock.SimulateTap(intent.ProviderPaymentID, intent.CardPaymentID, 5000)[0]

	bad := map[string]*http.Request{
		"wrong secret": signedAcquirerRequest("another_secret_that_is_32_bytes_long!!", time.Now(), "mock", ev),
		"stale":        signedAcquirerRequest(testAcquirerSecret, time.Now().Add(-WebhookTolerance-time.Minute), "mock", ev),
		"no headers":   httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/acquirer/mock", strings.NewReader(`{}`)),
	}
	tampered := signedAcquirerRequest(testAcquirerSecret, time.Now(), "mock", ev)
	tampered.Body = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"id":"x","type":"payment.authorized","provider_payment_id":"other"}`)).Body
	bad["tampered"] = tampered
	for name, req := range bad {
		if rec := serve(s, req); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: want 401, got %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if rec := serve(s, signedAcquirerRequest(testAcquirerSecret, time.Now(), "adyen", ev)); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown provider path: want 404, got %d", rec.Code)
	}
	if rec := serve(s, signedAcquirerRequest(testAcquirerSecret, time.Now(), "mock", map[string]any{"id": "e", "type": "payment.exploded", "provider_payment_id": "p"})); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid event: want 400, got %d", rec.Code)
	}
	if _, got := getCardPayment(t, s, cardMerchant, intent.CardPaymentID); got.Status != store.CardCreated {
		t.Fatalf("rejected webhooks must not change state: %s", got.Status)
	}
	// Public route (no JWT), valid signature.
	if code, body := deliver(t, s, ev); code != http.StatusOK || !strings.Contains(body, `"applied"`) {
		t.Fatalf("valid: %d %s", code, body)
	}

	s2, _ := setupCardServer(t) // no secret configured
	if rec := serve(s2, signedAcquirerRequest(testAcquirerSecret, time.Now(), "mock", ev)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no secret: want 503, got %d", rec.Code)
	}
}

func TestAcquirerWebhook_ExactlyOnceAndOutOfOrderRefund(t *testing.T) {
	s, mock := setupWebhookCardServer(t)
	_, intent, _ := createIntent(t, s, cardMerchant, "ooo-1", map[string]any{"amount_cents": 11600})
	tap := mock.SimulateTap(intent.ProviderPaymentID, intent.CardPaymentID, 11600)
	refund := acquirer.Event{ID: "evt_rf_1", Type: acquirer.EventRefunded, ProviderPaymentID: intent.ProviderPaymentID, RefundID: "re_1", AmountCents: 5800}

	// Refund before the capture: retryable, nothing consumed.
	if code, body := deliver(t, s, refund); code != http.StatusConflict || !strings.Contains(body, "EVENT_OUT_OF_ORDER") {
		t.Fatalf("refund before capture: want 409, got %d %s", code, body)
	}
	for _, e := range []acquirer.Event{tap[1], tap[1], tap[0]} { // capture, duplicate, late authorized
		if code, body := deliver(t, s, e); code != http.StatusOK {
			t.Fatalf("%s: %d %s", e.Type, code, body)
		}
	}
	avail := "LIABILITY:MERCHANT:" + cardMerchant + ":AVAILABLE"
	_, got := getCardPayment(t, s, cardMerchant, intent.CardPaymentID)
	net := got.Settlement.NetAvailableCents
	if balanceOf(t, s, avail) != net {
		t.Fatalf("capture posted once: %d vs %d", balanceOf(t, s, avail), net)
	}
	// The provider retries the refund: now applied, proportionally reversed.
	code, body := deliver(t, s, refund)
	if code != http.StatusOK || !strings.Contains(body, `"PARTIALLY_REFUNDED"`) {
		t.Fatalf("refund retry: %d %s", code, body)
	}
	// Same refund notified under another event id: deduplicated by refund id.
	dup := refund
	dup.ID = "evt_rf_1_bis"
	if code, body := deliver(t, s, dup); code != http.StatusOK || !strings.Contains(body, `"ignored"`) {
		t.Fatalf("duplicate refund id: %d %s", code, body)
	}
	_, got = getCardPayment(t, s, cardMerchant, intent.CardPaymentID)
	if got.RefundedCents != 5800 || got.Status != store.CardPartiallyRefunded {
		t.Fatalf("after refund: %+v", got)
	}
	half := net - balanceOf(t, s, avail)
	if half <= 0 || half >= net {
		t.Fatalf("half refund must reverse part of the net: %d of %d", half, net)
	}
	if n := len(memEvents(t, s, "card_payment.refunded")); n != 1 {
		t.Fatalf("outbox refunded: %d", n)
	}
	// Over-refund conflicts.
	over := acquirer.Event{ID: "evt_rf_over", Type: acquirer.EventRefunded, ProviderPaymentID: intent.ProviderPaymentID, RefundID: "re_2", AmountCents: 5801}
	if code, _ := deliver(t, s, over); code != http.StatusUnprocessableEntity {
		t.Fatalf("over-refund: want 422, got %d", code)
	}
	rest := acquirer.Event{ID: "evt_rf_rest", Type: acquirer.EventRefunded, ProviderPaymentID: intent.ProviderPaymentID, RefundID: "re_3", AmountCents: 5800}
	if code, body := deliver(t, s, rest); code != http.StatusOK || !strings.Contains(body, `"REFUNDED"`) {
		t.Fatalf("final refund: %d %s", code, body)
	}
	if balanceOf(t, s, avail) != 0 || s.GetMerchantMonthlySalesCents(cardMerchant) != 0 {
		t.Fatalf("full refund must reverse everything: avail %d sales %d", balanceOf(t, s, avail), s.GetMerchantMonthlySalesCents(cardMerchant))
	}
}

func TestCardRefundRequest_GoesToProviderLedgerMovesOnWebhook(t *testing.T) {
	s, mock := setupWebhookCardServer(t)
	intent := capturedCardPayment(t, s, mock, "rf-1", 20000)
	avail := "LIABILITY:MERCHANT:" + cardMerchant + ":AVAILABLE"
	before := balanceOf(t, s, avail)

	if code, _, _ := requestRefund(t, s, auth.RoleCashier, intent.CardPaymentID, "r1", 5000); code != http.StatusForbidden {
		t.Fatalf("cashier: want 403, got %d", code)
	}
	if code, _, _ := requestRefund(t, s, auth.RoleOwner, intent.CardPaymentID, "", 5000); code != http.StatusBadRequest {
		t.Fatalf("missing key: want 400, got %d", code)
	}
	if code, _, body := requestRefund(t, s, auth.RoleOwner, intent.CardPaymentID, "r0", 20001); code != http.StatusBadRequest || !strings.Contains(body, "REFUND_EXCEEDS_REMAINING") {
		t.Fatalf("over amount: %d %s", code, body)
	}
	code, rf, body := requestRefund(t, s, auth.RoleOwner, intent.CardPaymentID, "r1", 15000)
	if code != http.StatusAccepted || rf.Status != store.RefundPending || rf.ProviderRefundID == "" || mock.Calls("Refund") != 1 {
		t.Fatalf("request: %d %s", code, body)
	}
	if balanceOf(t, s, avail) != before {
		t.Fatal("a refund request must not move the ledger")
	}
	// The pending amount is held: only 5000 remain requestable.
	if code, _, _ := requestRefund(t, s, auth.RoleOwner, intent.CardPaymentID, "r2", 5001); code != http.StatusBadRequest {
		t.Fatalf("hold: want 400, got %d", code)
	}
	// Replay with the same key: same refund, no new provider call.
	if code, again, _ := requestRefund(t, s, auth.RoleOwner, intent.CardPaymentID, "r1", 15000); code != http.StatusAccepted || again.RefundID != rf.RefundID || mock.Calls("Refund") != 1 {
		t.Fatalf("replay: %d %+v calls=%d", code, again, mock.Calls("Refund"))
	}
	if code, _, _ := requestRefund(t, s, auth.RoleOwner, intent.CardPaymentID, "r1", 100); code != http.StatusConflict {
		t.Fatalf("key reuse with another amount: want 409, got %d", code)
	}
	// Processor confirms.
	if code, body := deliver(t, s, acquirer.Event{ID: "evt_r1", Type: acquirer.EventRefunded, ProviderPaymentID: intent.ProviderPaymentID, RefundID: rf.ProviderRefundID, AmountCents: 15000}); code != http.StatusOK {
		t.Fatalf("refund webhook: %d %s", code, body)
	}
	if balanceOf(t, s, avail) >= before {
		t.Fatal("refund webhook must debit the merchant")
	}
	if code, again, _ := requestRefund(t, s, auth.RoleOwner, intent.CardPaymentID, "r1", 0); code != http.StatusAccepted || again.Status != store.RefundSucceeded {
		t.Fatalf("replay after success: %d %+v", code, again)
	}

	// refund.failed releases the hold.
	code, rf2, _ := requestRefund(t, s, auth.RoleOwner, intent.CardPaymentID, "r3", 0)
	if code != http.StatusAccepted || rf2.AmountCents != 5000 {
		t.Fatalf("remainder request: %d %+v", code, rf2)
	}
	if code, body := deliver(t, s, acquirer.Event{ID: "evt_r3_fail", Type: acquirer.EventRefundFailed, ProviderPaymentID: intent.ProviderPaymentID, RefundID: rf2.ProviderRefundID}); code != http.StatusOK {
		t.Fatalf("refund.failed: %d %s", code, body)
	}
	if code, rf4, _ := requestRefund(t, s, auth.RoleOwner, intent.CardPaymentID, "r4", 0); code != http.StatusAccepted || rf4.AmountCents != 5000 {
		t.Fatalf("hold released: %d %+v", code, rf4)
	}
}

func TestCardRefundRequest_ProviderTimeoutKeepsRequested(t *testing.T) {
	s, mock := setupWebhookCardServer(t)
	intent := capturedCardPayment(t, s, mock, "rf-t", 30000)
	amount := int64(1000 + acquirer.MockTimeoutSuffix)
	if code, _, body := requestRefund(t, s, auth.RoleOwner, intent.CardPaymentID, "rt", amount); code != http.StatusBadGateway || !strings.Contains(body, "ACQUIRER_UNAVAILABLE") {
		t.Fatalf("timeout: %d %s", code, body)
	}
	if code, _, _ := requestRefund(t, s, auth.RoleOwner, intent.CardPaymentID, "rt", amount); code != http.StatusBadGateway || mock.Calls("Refund") != 2 {
		t.Fatalf("retry must re-send: %d calls=%d", code, mock.Calls("Refund"))
	}
	// The unknown-outcome request still holds its amount.
	if code, _, _ := requestRefund(t, s, auth.RoleOwner, intent.CardPaymentID, "rt2", 30000-amount+1); code != http.StatusBadRequest {
		t.Fatalf("hold of REQUESTED refund: want 400, got %d", code)
	}
}

func TestAcquirerWebhook_Chargebacks(t *testing.T) {
	s, mock := setupWebhookCardServer(t)
	intent := capturedCardPayment(t, s, mock, "cb-1", 10000)
	avail := "LIABILITY:MERCHANT:" + cardMerchant + ":AVAILABLE"
	before := balanceOf(t, s, avail)
	rev := acquirer.Event{ID: "evt_cb_rev", Type: acquirer.EventChargebackReversed, ProviderPaymentID: intent.ProviderPaymentID, ChargebackID: "cb_1", AmountCents: 10000}
	if code, _ := deliver(t, s, rev); code != http.StatusConflict {
		t.Fatalf("reversed before opened: want 409, got %d", code)
	}
	opened := acquirer.Event{ID: "evt_cb_open", Type: acquirer.EventChargebackOpened, ProviderPaymentID: intent.ProviderPaymentID, ChargebackID: "cb_1", AmountCents: 10000}
	if code, body := deliver(t, s, opened); code != http.StatusOK || !strings.Contains(body, `"CHARGEBACK"`) {
		t.Fatalf("opened: %d %s", code, body)
	}
	// The whole disputed amount leaves the merchant's available balance
	// (negative: more than the net that was credited).
	if got := balanceOf(t, s, avail); got != before-10000 || got >= 0 {
		t.Fatalf("available after chargeback: %d (before %d)", got, before)
	}
	evs := memEvents(t, s, "card_payment.chargeback_opened")
	if len(evs) != 1 || !strings.Contains(string(evs[0].Payload), `"available_after_cents"`) {
		t.Fatalf("merchant notification: %+v", evs)
	}
	if code, _, _ := requestRefund(t, s, auth.RoleOwner, intent.CardPaymentID, "rcb", 100); code != http.StatusConflict {
		t.Fatalf("refund during chargeback: want 409, got %d", code)
	}
	dupOpen := opened
	dupOpen.ID = "evt_cb_open_2"
	if code, body := deliver(t, s, dupOpen); code != http.StatusOK || !strings.Contains(body, `"ignored"`) {
		t.Fatalf("duplicate chargeback id: %d %s", code, body)
	}
	if code, body := deliver(t, s, rev); code != http.StatusOK || !strings.Contains(body, `"CHARGEBACK_REVERSED"`) {
		t.Fatalf("reversed: %d %s", code, body)
	}
	if balanceOf(t, s, avail) != before {
		t.Fatalf("won chargeback must restore the balance: %d vs %d", balanceOf(t, s, avail), before)
	}
}

func TestDevCardSimulate(t *testing.T) {
	s, _ := setupWebhookCardServer(t)
	_, intent, _ := createIntent(t, s, cardMerchant, "dev-1", map[string]any{"amount_cents": 4000})
	path := "/api/v1/dev/card-payments/" + intent.CardPaymentID + "/simulate"
	if rec := serve(s, asTenant(t, s, cardMerchant, jsonReq(http.MethodPost, path, nil))); rec.Code != http.StatusNotFound {
		t.Fatalf("outside DEV_MODE: want 404, got %d", rec.Code)
	}
	s.SetDevMode(true)
	if rec := serve(s, asTenant(t, s, "merch_other", jsonReq(http.MethodPost, path, nil))); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign payment: want 404, got %d", rec.Code)
	}
	rec := serve(s, asTenant(t, s, cardMerchant, jsonReq(http.MethodPost, path, map[string]any{"outcome": "tap"})))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"CAPTURED"`) {
		t.Fatalf("simulate tap: %d %s", rec.Code, rec.Body.String())
	}
	code, rf, _ := requestRefund(t, s, auth.RoleOwner, intent.CardPaymentID, "dev-r", 1000)
	if code != http.StatusAccepted {
		t.Fatalf("refund request: %d", code)
	}
	rec = serve(s, asTenant(t, s, cardMerchant, jsonReq(http.MethodPost, path, map[string]any{"outcome": "refund", "refund_id": rf.RefundID})))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"PARTIALLY_REFUNDED"`) {
		t.Fatalf("simulate refund: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPublicRoutes_AcquirerWebhookPrefixOnly(t *testing.T) {
	for path, want := range map[string]bool{
		"/api/v1/webhooks/acquirer/mock":   true,
		"/api/v1/webhooks/acquirer/":       false,
		"/api/v1/webhooks/acquirer/mock/x": false,
		"/api/v1/webhooks/acquirerx/mock":  false,
		"/api/v1/card-payments/intents":    false,
	} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		if got := isPublicRoute(req); got != want {
			t.Errorf("%s: public=%v want %v", path, got, want)
		}
	}
	if isPublicRoute(httptest.NewRequest(http.MethodGet, "/api/v1/webhooks/acquirer/mock", nil)) {
		t.Error("GET must not be public")
	}
}
