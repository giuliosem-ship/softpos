package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"softpos/pkg/acquirer"
	"softpos/pkg/auth"
	"softpos/pkg/store"
)

// A refund webhook may not take back more than what is still captured and not
// disputed: refund + chargeback of one payment never exceed the capture.
func TestAcquirerWebhook_RefundAfterChargebackCannotExceedCapture(t *testing.T) {
	s, mock := setupWebhookCardServer(t)
	intent := capturedCardPayment(t, s, mock, "cbrf-1", 10000)
	avail := "LIABILITY:MERCHANT:" + cardMerchant + ":AVAILABLE"
	recv := AccountAcquirerReceivable
	opened := acquirer.Event{ID: "evt_cbrf_open", Type: acquirer.EventChargebackOpened, ProviderPaymentID: intent.ProviderPaymentID, ChargebackID: "cb_1", AmountCents: 10000}
	if code, body := deliver(t, s, opened); code != http.StatusOK {
		t.Fatalf("opened: %d %s", code, body)
	}
	beforeAvail, beforeRecv := balanceOf(t, s, avail), balanceOf(t, s, recv)
	refund := acquirer.Event{ID: "evt_cbrf_rf", Type: acquirer.EventRefunded, ProviderPaymentID: intent.ProviderPaymentID, RefundID: "re_1", AmountCents: 10000}
	if code, body := deliver(t, s, refund); code != http.StatusUnprocessableEntity || !strings.Contains(body, "EVENT_CONFLICT") {
		t.Fatalf("refund of a fully disputed payment: want 422 EVENT_CONFLICT, got %d %s", code, body)
	}
	if balanceOf(t, s, avail) != beforeAvail || balanceOf(t, s, recv) != beforeRecv {
		t.Fatal("a conflicting refund must not move the ledger")
	}
}

// The refund webhook can arrive before the provider refund id is stored (the
// provider call timed out, or its answer is still being written). The event
// echoes Kerno's refund id, so the request is matched and its hold released
// instead of blocking the remainder forever.
func TestCardRefund_WebhookBeforeProviderRefundIDStored(t *testing.T) {
	s, mock := setupWebhookCardServer(t)
	intent := capturedCardPayment(t, s, mock, "rf-race", 30000)
	amount := int64(1000 + acquirer.MockTimeoutSuffix)
	if code, _, body := requestRefund(t, s, auth.RoleOwner, intent.CardPaymentID, "rr", amount); code != http.StatusBadGateway {
		t.Fatalf("timeout: %d %s", code, body)
	}
	evs := memEvents(t, s, "card_payment.refund_requested")
	if len(evs) != 1 {
		t.Fatalf("refund_requested events: %d", len(evs))
	}
	var requested CardRefundResponse
	if err := json.Unmarshal(evs[0].Payload, &requested); err != nil || requested.RefundID == "" {
		t.Fatalf("payload: %s %v", evs[0].Payload, err)
	}
	ev := acquirer.Event{ID: "evt_race", Type: acquirer.EventRefunded, ProviderPaymentID: intent.ProviderPaymentID,
		RefundID: "re_provider_1", KernoRefundID: requested.RefundID, AmountCents: amount}
	if code, body := deliver(t, s, ev); code != http.StatusOK {
		t.Fatalf("refund webhook: %d %s", code, body)
	}
	// The request is SUCCEEDED (replay answers it without a new provider call)...
	calls := mock.Calls("Refund")
	code, again, body := requestRefund(t, s, auth.RoleOwner, intent.CardPaymentID, "rr", amount)
	if code != http.StatusAccepted || again.Status != store.RefundSucceeded || again.ProviderRefundID != "re_provider_1" || mock.Calls("Refund") != calls {
		t.Fatalf("replay after webhook: %d %s calls=%d", code, body, mock.Calls("Refund"))
	}
	// ...and no longer holds its amount: the whole remainder is refundable.
	if code, rest, body := requestRefund(t, s, auth.RoleOwner, intent.CardPaymentID, "rr2", 0); code != http.StatusAccepted || rest.AmountCents != 30000-amount {
		t.Fatalf("remainder: %d %s", code, body)
	}
}
