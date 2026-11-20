package api

import (
	"net/http"
	"strings"
	"testing"

	"softpos/pkg/acquirer"
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
