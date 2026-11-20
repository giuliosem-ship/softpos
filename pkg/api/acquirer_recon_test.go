package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"softpos/pkg/acquirer"
	"softpos/pkg/auth"
	"softpos/pkg/store"
)

func payoutEvent(id, payoutID string, amount int64, items ...acquirer.PayoutItem) acquirer.Event {
	return acquirer.Event{ID: id, Type: acquirer.EventPayoutSettled, Payout: &acquirer.Payout{
		PayoutID: payoutID, AmountCents: amount, SettlementDate: "2026-11-21", Items: items,
	}}
}

type reconReport struct {
	Items   []ReconciliationItemResponse `json:"items"`
	Payouts []AcquirerPayoutResponse     `json:"payouts"`
}

func getRecon(t *testing.T, s *Server, role auth.Role, query string) (int, reconReport) {
	t.Helper()
	rec := serve(s, asRole(t, s, cardMerchant, role, jsonReq(http.MethodGet, "/api/v1/acquirer/reconciliation"+query, nil)))
	var out reconReport
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestAcquirerPayout_ReconcilesAndMovesReceivableToBank(t *testing.T) {
	s, mock := setupWebhookCardServer(t)
	a := capturedCardPayment(t, s, mock, "po-a", 10000)
	b := capturedCardPayment(t, s, mock, "po-b", 20000)
	feeA, feeB := mock.FeeCents(10000), mock.FeeCents(20000)
	receivable := balanceOf(t, s, AccountAcquirerReceivable)
	if receivable != 30000-feeA-feeB {
		t.Fatalf("receivable before payout: %d", receivable)
	}
	ev := payoutEvent("evt_po_1", "po_1", receivable,
		acquirer.PayoutItem{ProviderPaymentID: a.ProviderPaymentID, Kind: acquirer.PayoutItemCapture, AmountCents: 10000, FeeCents: feeA},
		acquirer.PayoutItem{ProviderPaymentID: b.ProviderPaymentID, Kind: acquirer.PayoutItemCapture, AmountCents: 20000, FeeCents: feeB})
	code, body := deliver(t, s, ev)
	if code != http.StatusOK || !strings.Contains(body, `"applied"`) || strings.Contains(body, "discrepancies") {
		t.Fatalf("payout: %d %s", code, body)
	}
	if balanceOf(t, s, AccountAcquirerReceivable) != 0 || balanceOf(t, s, AccountBankOperating) != receivable {
		t.Fatalf("receivable %d bank %d", balanceOf(t, s, AccountAcquirerReceivable), balanceOf(t, s, AccountBankOperating))
	}
	ms := s.moneyStore().(*store.MemoryMoneyStore)
	if cp, _ := ms.CardPayment(a.CardPaymentID); cp.ProviderPayoutID != "po_1" || cp.ProviderSettledAt == nil {
		t.Fatalf("capture not marked settled: %+v", cp)
	}
	// Same payout again under another event id: nothing posted twice.
	again := ev
	again.ID = "evt_po_1_resent"
	if code, body := deliver(t, s, again); code != http.StatusOK || !strings.Contains(body, `"ignored"`) {
		t.Fatalf("duplicate payout: %d %s", code, body)
	}
	if balanceOf(t, s, AccountBankOperating) != receivable {
		t.Fatal("payout posted twice")
	}
	code, rep := getRecon(t, s, auth.RoleOperator, "")
	if code != http.StatusOK || len(rep.Items) != 0 || len(rep.Payouts) != 1 || rep.Payouts[0].Status != "RECONCILED" {
		t.Fatalf("report: %d %+v", code, rep)
	}
	if n := len(memEvents(t, s, "acquirer.payout_settled")); n != 1 {
		t.Fatalf("outbox payout_settled: %d", n)
	}
}

func TestAcquirerPayout_FlagsMismatches(t *testing.T) {
	s, mock := setupWebhookCardServer(t)
	c := capturedCardPayment(t, s, mock, "po-c", 5000)
	d := capturedCardPayment(t, s, mock, "po-d", 8000)
	feeC, feeD := mock.FeeCents(5000), mock.FeeCents(8000)
	// First payout settles D correctly.
	deliver(t, s, payoutEvent("evt_po_d", "po_d", 8000-feeD,
		acquirer.PayoutItem{ProviderPaymentID: d.ProviderPaymentID, Kind: acquirer.PayoutItemCapture, AmountCents: 8000, FeeCents: feeD}))

	ev := payoutEvent("evt_po_2", "po_2", 99999,
		acquirer.PayoutItem{ProviderPaymentID: c.ProviderPaymentID, Kind: acquirer.PayoutItemCapture, AmountCents: 4900, FeeCents: feeC + 75}, // amount + fee mismatch
		acquirer.PayoutItem{ProviderPaymentID: "mock_pi_unknown", Kind: acquirer.PayoutItemCapture, AmountCents: 1000},                        // missing payment
		acquirer.PayoutItem{ProviderPaymentID: d.ProviderPaymentID, Kind: acquirer.PayoutItemCapture, AmountCents: 8000, FeeCents: feeD},      // settled twice
	)
	code, body := deliver(t, s, ev)
	if code != http.StatusOK || !strings.Contains(body, `"discrepancies":5`) {
		t.Fatalf("mismatched payout: %d %s", code, body)
	}
	if balanceOf(t, s, AccountBankOperating) != 8000-feeD+99999 {
		t.Fatalf("the deposited amount is booked as received: %d", balanceOf(t, s, AccountBankOperating))
	}

	if code, _ := getRecon(t, s, auth.RoleOwner, ""); code != http.StatusForbidden {
		t.Fatalf("owner: want 403, got %d", code)
	}
	code, rep := getRecon(t, s, auth.RoleOperator, "?status=OPEN")
	if code != http.StatusOK {
		t.Fatalf("report: %d", code)
	}
	kinds := map[string]ReconciliationItemResponse{}
	for _, it := range rep.Items {
		kinds[it.Kind] = it
	}
	for _, k := range []string{store.ReconAmountMismatch, store.ReconFeeMismatch, store.ReconMissingPayment, store.ReconDuplicateSettlement, store.ReconPayoutTotalMismatch} {
		if _, ok := kinds[k]; !ok {
			t.Errorf("missing discrepancy %s in %+v", k, rep.Items)
		}
	}
	if fm := kinds[store.ReconFeeMismatch]; fm.ExpectedCents != feeC || fm.ActualCents != feeC+75 || fm.CardPaymentID != c.CardPaymentID {
		t.Fatalf("fee mismatch line: %+v", fm)
	}
	wantExpected := (4900 - feeC - 75) + 1000 + (8000 - feeD)
	if tm := kinds[store.ReconPayoutTotalMismatch]; tm.ExpectedCents != wantExpected || tm.ActualCents != 99999 {
		t.Fatalf("total mismatch line: %+v (want expected %d)", tm, wantExpected)
	}
	var disc AcquirerPayoutResponse
	for _, p := range rep.Payouts {
		if p.PayoutID == "po_2" {
			disc = p
		}
	}
	if disc.Status != "DISCREPANCY" || disc.DiscrepancyCount != 5 {
		t.Fatalf("payout summary: %+v", disc)
	}

	// Operator resolves one item.
	id := kinds[store.ReconMissingPayment].ID
	path := "/api/v1/acquirer/reconciliation/" + strconv.FormatInt(id, 10) + "/resolve"
	if rec := serve(s, asRole(t, s, cardMerchant, auth.RoleOperator, jsonReq(http.MethodPost, path, map[string]any{}))); rec.Code != http.StatusBadRequest {
		t.Fatalf("resolve without note: want 400, got %d", rec.Code)
	}
	if rec := serve(s, asRole(t, s, cardMerchant, auth.RoleOperator, jsonReq(http.MethodPost, path, map[string]any{"note": "processor test line"}))); rec.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(s, asRole(t, s, cardMerchant, auth.RoleOperator, jsonReq(http.MethodPost, path, map[string]any{"note": "again"}))); rec.Code != http.StatusNotFound {
		t.Fatalf("resolve twice: want 404, got %d", rec.Code)
	}
	if _, rep := getRecon(t, s, auth.RoleOperator, "?status=RESOLVED"); len(rep.Items) != 1 || rep.Items[0].ResolutionNote != "processor test line" {
		t.Fatalf("resolved list: %+v", rep.Items)
	}
	if code, _ := getRecon(t, s, auth.RoleOperator, "?status=BOGUS"); code != http.StatusBadRequest {
		t.Fatalf("bad status: %d", code)
	}
}
