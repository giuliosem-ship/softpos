package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"softpos/pkg/auth"
	"softpos/pkg/payments"
	"softpos/pkg/stp"
)

const settlementCLABE = "032180000118359719"

func fundMerchant(t *testing.T, s *Server, merchantID string, cents int64) {
	t.Helper()
	if _, err := s.ledger.RecordSplitPayment(cents, merchantID, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
}

func balanceOf(t *testing.T, s *Server, code string) int64 {
	t.Helper()
	b, err := s.ledger.GetBalance(code)
	if err != nil {
		t.Fatalf("balance %s: %v", code, err)
	}
	return b.AmountCents
}

func TestSettlementAccount_RegistrationVerificationAndPayoutGate(t *testing.T) {
	s, _ := setupTestServer(t)
	const m = "merch_payout_gate"
	_ = s.RegisterMerchantCLABE("646180123400000007", m, "GARM850101ABC")
	fundMerchant(t, s, m, 100000)
	path := "/api/v1/merchants/" + m + "/settlement-account"
	payout := map[string]any{"amount_cents": 1000, "concept": "retiro"}

	if rec := serve(s, asTenant(t, s, m, jsonReq("POST", "/api/v1/disbursements", payout))); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "SETTLEMENT_ACCOUNT_NOT_REGISTERED") {
		t.Fatalf("payout without account: %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(s, asRole(t, s, m, auth.RoleCashier, jsonReq("PUT", path, map[string]any{"clabe": settlementCLABE}))); rec.Code != http.StatusForbidden {
		t.Fatalf("cashier must not set the settlement CLABE: %d", rec.Code)
	}
	if rec := serve(s, asTenant(t, s, m, jsonReq("PUT", path, map[string]any{"clabe": "032180000118359710"}))); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid CLABE: %d", rec.Code)
	}
	if rec := serve(s, asTenant(t, s, m, jsonReq("PUT", path, map[string]any{"clabe": "646180123400000007"}))); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "COLLECTION_CLABE") {
		t.Fatalf("collection CLABE as payout account: %d %s", rec.Code, rec.Body.String())
	}
	rec := serve(s, asTenant(t, s, m, jsonReq("PUT", path, map[string]any{"clabe": settlementCLABE})))
	var acct SettlementAccountResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &acct)
	if rec.Code != http.StatusOK || acct.Status != "PENDING" || acct.CLABE != settlementCLABE {
		t.Fatalf("owner PUT: %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(s, asTenant(t, s, m, jsonReq("POST", "/api/v1/disbursements", payout))); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "NOT_VERIFIED") {
		t.Fatalf("payout to PENDING account: %d %s", rec.Code, rec.Body.String())
	}

	review := path + "/verify"
	if rec := serve(s, asTenant(t, s, m, jsonReq("POST", review, map[string]any{"clabe": settlementCLABE, "status": "VERIFIED"}))); rec.Code != http.StatusForbidden {
		t.Fatalf("owner cannot self-verify outside DEV_MODE: %d", rec.Code)
	}
	if rec := serve(s, asRole(t, s, m, auth.RoleOperator, jsonReq("POST", review, map[string]any{"clabe": "012180000118359713", "status": "VERIFIED"}))); rec.Code != http.StatusConflict {
		t.Fatalf("review of a different CLABE must fail: %d", rec.Code)
	}
	if rec := serve(s, asRole(t, s, m, auth.RoleOperator, jsonReq("POST", review, map[string]any{"clabe": settlementCLABE, "status": "VERIFIED"}))); rec.Code != http.StatusOK {
		t.Fatalf("operator verify: %d %s", rec.Code, rec.Body.String())
	}

	if rec := serve(s, asTenant(t, s, m, jsonReq("POST", "/api/v1/disbursements", map[string]any{"amount_cents": 1000, "destination_clabe": "012180000118359713"}))); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "DESTINATION_CLABE_NOT_ALLOWED") {
		t.Fatalf("payout to arbitrary CLABE: %d %s", rec.Code, rec.Body.String())
	}
	rec = serve(s, asTenant(t, s, m, jsonReq("POST", "/api/v1/disbursements", payout)))
	var disb DisbursementResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &disb)
	if rec.Code != http.StatusOK || disb.DestinationCLABE != settlementCLABE || disb.RemainingBalanceCents != 99000 {
		t.Fatalf("payout to verified account: %d %s", rec.Code, rec.Body.String())
	}

	// Changing the account resets verification and blocks payouts again.
	if rec := serve(s, asTenant(t, s, m, jsonReq("PUT", path, map[string]any{"clabe": "012180000118359713"}))); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "PENDING") {
		t.Fatalf("change account: %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(s, asTenant(t, s, m, jsonReq("POST", "/api/v1/disbursements", payout))); rec.Code != http.StatusConflict {
		t.Fatalf("payout after account change must wait for verification: %d", rec.Code)
	}

	// DEV_MODE: the owner may verify (demo path).
	s.SetDevMode(true)
	if rec := serve(s, asTenant(t, s, m, jsonReq("POST", review, map[string]any{"clabe": "012180000118359713", "status": "VERIFIED"}))); rec.Code != http.StatusOK {
		t.Fatalf("dev-mode owner verify: %d %s", rec.Code, rec.Body.String())
	}
}

func TestDisbursement_ConcurrentPayoutsCannotOverdraw(t *testing.T) {
	s, _ := setupTestServer(t)
	const m = "merch_payout_race"
	_ = s.RegisterMerchant(m, "GARM850101ABC", "M", true)
	_ = s.SeedVerifiedSettlementAccount(context.Background(), m, settlementCLABE)
	fundMerchant(t, s, m, 100000)
	codes := make([]int, 10)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = serve(s, asTenant(t, s, m, jsonReq("POST", "/api/v1/disbursements", map[string]any{"amount_cents": 30000}))).Code
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, c := range codes {
		if c == http.StatusOK {
			ok++
		} else if c != http.StatusBadRequest {
			t.Fatalf("unexpected status %d", c)
		}
	}
	if ok != 3 || availableCents(t, s, m) != 10000 {
		t.Fatalf("want 3 payouts and 10000 left, got %d payouts and %d", ok, availableCents(t, s, m))
	}
}

type failingSTP struct{}

func (failingSTP) RegisterOrder(context.Context, stp.STPOrderRequest) (*stp.STPOrderResponse, error) {
	return nil, errors.New("stp down")
}

func TestDisbursement_DispatchFailureReturnsFunds(t *testing.T) {
	s, _ := setupTestServer(t)
	const m = "merch_payout_fail"
	_ = s.RegisterMerchant(m, "GARM850101ABC", "M", true)
	_ = s.SeedVerifiedSettlementAccount(context.Background(), m, settlementCLABE)
	fundMerchant(t, s, m, 50000)
	s.SetSTPClient(failingSTP{})
	if rec := serve(s, asTenant(t, s, m, jsonReq("POST", "/api/v1/disbursements", map[string]any{"amount_cents": 20000}))); rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", rec.Code)
	}
	if got := availableCents(t, s, m); got != 50000 {
		t.Fatalf("funds must be returned after a failed dispatch, available=%d", got)
	}
	if len(memEvents(t, s, "disbursement.requested")) != 1 || len(memEvents(t, s, "disbursement.failed")) != 1 {
		t.Fatalf("expected requested + failed events")
	}
}

func reverse(t *testing.T, s *Server, m string, role auth.Role, body map[string]any, idemKey string) *httpResult {
	t.Helper()
	req := asRole(t, s, m, role, jsonReq("POST", "/api/v1/payments/emv/reverse", body))
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	rec := serve(s, req)
	var resp EMVReverseResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return &httpResult{code: rec.Code, body: rec.Body.String(), resp: resp}
}

type httpResult struct {
	code int
	body string
	resp EMVReverseResponse
}

func TestRefund_PartialAndFullReverseTaxFeeAndAccumulator(t *testing.T) {
	s, _ := setupTestServer(t)
	const m = "merch_refund_prop"
	_ = s.RegisterMerchant(m, "GARM850101ABC", "M", true)
	authorizeTestPayment(t, s, m, "pay_prop", 11600)
	if rec := serve(s, asTenant(t, s, m, jsonReq("POST", "/api/v1/payments/settle", map[string]any{"payment_id": "pay_prop"}))); rec.Code != http.StatusOK {
		t.Fatalf("settle: %d %s", rec.Code, rec.Body.String())
	}
	escrow := "LIABILITY:MERCHANT:" + m + ":TAX_ESCROW"
	// Settled: avail 9668, escrow 1700 (IVA 1600 + ISR 100), fee 232, base 10000.
	if availableCents(t, s, m) != 9668 || balanceOf(t, s, escrow) != 1700 || balanceOf(t, s, "REVENUE:FEES") != 232 {
		t.Fatalf("unexpected settled balances")
	}

	// Half refund: escrow -850, fee -116, accumulator -5000, available -4834.
	r := reverse(t, s, m, auth.RoleOwner, map[string]any{"payment_id": "pay_prop", "refund_cents": 5800}, "")
	if r.code != http.StatusOK || r.resp.Status != payments.PaymentSettled || r.resp.RemainingRefundableCents != 5800 ||
		r.resp.ReversedTaxEscrowCents != 850 || r.resp.ReversedPlatformFeeCents != 116 {
		t.Fatalf("partial refund: %d %s", r.code, r.body)
	}
	if availableCents(t, s, m) != 4834 || balanceOf(t, s, escrow) != 850 || balanceOf(t, s, "REVENUE:FEES") != 116 || s.GetMerchantMonthlySalesCents(m) != 5000 {
		t.Fatalf("after partial refund: avail=%d escrow=%d fees=%d sales=%d", availableCents(t, s, m), balanceOf(t, s, escrow), balanceOf(t, s, "REVENUE:FEES"), s.GetMerchantMonthlySalesCents(m))
	}
	if r := reverse(t, s, m, auth.RoleOwner, map[string]any{"payment_id": "pay_prop", "refund_cents": 5801}, ""); r.code != http.StatusBadRequest || !strings.Contains(r.body, "REFUND_EXCEEDS_REMAINING") {
		t.Fatalf("over-refund: %d %s", r.code, r.body)
	}

	// Remaining refund (refund_cents omitted) reverses everything exactly.
	r = reverse(t, s, m, auth.RoleOwner, map[string]any{"payment_id": "pay_prop"}, "")
	if r.code != http.StatusOK || r.resp.Status != payments.PaymentRefunded || r.resp.RefundedTotalCents != 11600 || r.resp.RemainingRefundableCents != 0 {
		t.Fatalf("final refund: %d %s", r.code, r.body)
	}
	if availableCents(t, s, m) != 0 || balanceOf(t, s, escrow) != 0 || balanceOf(t, s, "REVENUE:FEES") != 0 ||
		balanceOf(t, s, "ASSET:ACQUIRER_RECEIVABLE") != 0 || s.GetMerchantMonthlySalesCents(m) != 0 {
		t.Fatalf("full reversal must zero every account and the accumulator")
	}
	if r := reverse(t, s, m, auth.RoleOwner, map[string]any{"payment_id": "pay_prop"}, ""); r.code != http.StatusConflict {
		t.Fatalf("refund of a refunded payment: %d", r.code)
	}
	if got := len(memEvents(t, s, "payment.refunded")); got != 2 {
		t.Fatalf("payment.refunded events = %d", got)
	}
}

func TestRefund_OddPartialsSumExactly(t *testing.T) {
	s, _ := setupTestServer(t)
	const m = "merch_refund_odd"
	_ = s.RegisterMerchant(m, "GARM850101ABC", "M", true)
	authorizeTestPayment(t, s, m, "pay_odd", 10001)
	if rec := serve(s, asTenant(t, s, m, jsonReq("POST", "/api/v1/payments/settle", map[string]any{"payment_id": "pay_odd"}))); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	for _, c := range []int64{3333, 3333, 3335} {
		if r := reverse(t, s, m, auth.RoleOwner, map[string]any{"payment_id": "pay_odd", "refund_cents": c}, ""); r.code != http.StatusOK {
			t.Fatalf("refund %d: %d %s", c, r.code, r.body)
		}
	}
	if availableCents(t, s, m) != 0 || balanceOf(t, s, "LIABILITY:MERCHANT:"+m+":TAX_ESCROW") != 0 || balanceOf(t, s, "REVENUE:FEES") != 0 || s.GetMerchantMonthlySalesCents(m) != 0 {
		t.Fatalf("partials must add up to the full reversal")
	}
}

func TestRefund_OwnerOnlyByDefaultAndIdempotent(t *testing.T) {
	s, _ := setupTestServer(t)
	const m = "merch_refund_roles"
	_ = s.RegisterMerchant(m, "GARM850101ABC", "M", true)
	authorizeTestPayment(t, s, m, "pay_void_c", 11600)
	authorizeTestPayment(t, s, m, "pay_ref_c", 11600)
	if rec := serve(s, asTenant(t, s, m, jsonReq("POST", "/api/v1/payments/settle", map[string]any{"payment_id": "pay_ref_c"}))); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if r := reverse(t, s, m, auth.RoleCashier, map[string]any{"payment_id": "pay_void_c"}, ""); r.code != http.StatusOK || r.resp.Status != payments.PaymentVoided {
		t.Fatalf("cashier void of an authorized payment: %d %s", r.code, r.body)
	}
	if r := reverse(t, s, m, auth.RoleCashier, map[string]any{"payment_id": "pay_ref_c"}, ""); r.code != http.StatusForbidden {
		t.Fatalf("cashier refund must be forbidden by default: %d", r.code)
	}

	s.SetCashierRefundsAllowed(true)
	first := reverse(t, s, m, auth.RoleCashier, map[string]any{"payment_id": "pay_ref_c", "refund_cents": 1000}, "rf-1")
	retry := reverse(t, s, m, auth.RoleCashier, map[string]any{"payment_id": "pay_ref_c", "refund_cents": 1000}, "rf-1")
	if first.code != http.StatusOK || retry.code != http.StatusOK || first.body != retry.body || retry.resp.RefundedTotalCents != 1000 {
		t.Fatalf("idempotent refund retry: %d %s / %d %s", first.code, first.body, retry.code, retry.body)
	}
	if got := len(memEvents(t, s, "payment.refunded")); got != 1 {
		t.Fatalf("retry must not refund twice, events=%d", got)
	}
}
