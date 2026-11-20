package acquirer

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMock_IntentIsDeterministicAndIdempotent(t *testing.T) {
	m := NewMockProvider(MockConfig{})
	ctx := context.Background()
	a, err := m.CreatePaymentIntent(ctx, IntentRequest{IdempotencyKey: "kerno_intent_cp_1", AmountCents: 1000, Currency: "MXN"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := m.CreatePaymentIntent(ctx, IntentRequest{IdempotencyKey: "kerno_intent_cp_1", AmountCents: 1000, Currency: "MXN"})
	c, _ := m.CreatePaymentIntent(ctx, IntentRequest{IdempotencyKey: "kerno_intent_cp_2", AmountCents: 1000, Currency: "MXN"})
	if a.ProviderPaymentID != b.ProviderPaymentID || a.ClientSecret != b.ClientSecret || a.ProviderPaymentID == c.ProviderPaymentID {
		t.Fatalf("ids must derive from the idempotency key: %+v %+v %+v", a, b, c)
	}
	if a.ClientSecret == "" || a.Status != StatusRequiresPayment || a.ExpiresAt.IsZero() {
		t.Fatalf("intent: %+v", a)
	}
	if st, err := m.GetPayment(ctx, a.ProviderPaymentID); err != nil || st.AmountCents != 1000 {
		t.Fatalf("get: %+v %v", st, err)
	}
	if _, err := m.GetPayment(ctx, "mock_pi_missing"); !errors.Is(err, ErrPaymentNotFound) {
		t.Fatalf("missing: %v", err)
	}
	st, err := m.Capture(ctx, CaptureRequest{IdempotencyKey: "cap", ProviderPaymentID: a.ProviderPaymentID})
	if err != nil || st.Status != StatusCaptured || st.CapturedCents != 1000 || st.FeeCents != 25 {
		t.Fatalf("capture: %+v %v", st, err)
	}
	if _, err := m.Cancel(ctx, CancelRequest{IdempotencyKey: "c", ProviderPaymentID: a.ProviderPaymentID}); !errors.Is(err, ErrProviderRejected) {
		t.Fatalf("cancel of captured: %v", err)
	}
	r1, _ := m.Refund(ctx, RefundRequest{IdempotencyKey: "kerno_refund_r1", ProviderPaymentID: a.ProviderPaymentID, AmountCents: 500})
	r2, _ := m.Refund(ctx, RefundRequest{IdempotencyKey: "kerno_refund_r1", ProviderPaymentID: a.ProviderPaymentID, AmountCents: 500})
	if r1.ProviderRefundID == "" || r1 != r2 || r1.Status != "pending" {
		t.Fatalf("refund idempotency: %+v %+v", r1, r2)
	}
}

func TestMock_Scenarios(t *testing.T) {
	m := NewMockProvider(MockConfig{Now: func() time.Time { return time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC) }})
	ctx := context.Background()
	if ScenarioForAmount(1051) != MockDecline || ScenarioForAmount(1052) != MockTimeout || ScenarioForAmount(1000) != MockApprove {
		t.Fatal("scenario mapping")
	}
	if _, err := m.CreatePaymentIntent(ctx, IntentRequest{IdempotencyKey: "k", AmountCents: 1052}); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("timeout scenario: %v", err)
	}
	if _, err := m.CreatePaymentIntent(ctx, IntentRequest{AmountCents: 1000}); !errors.Is(err, ErrProviderRejected) {
		t.Fatalf("missing key: %v", err)
	}
	ev := m.SimulateTap("pi_1", "cp_1", 1051)
	if len(ev) != 1 || ev[0].Type != EventDeclined || ev[0].Validate() != nil {
		t.Fatalf("decline: %+v", ev)
	}
	ev = m.SimulateTap("pi_2", "cp_2", 10000)
	if len(ev) != 2 || ev[1].Type != EventCaptured || ev[1].FeeCents != 250 || ev[1].SettlementDate != "2026-11-21" {
		t.Fatalf("approve: %+v", ev)
	}
	for _, e := range ev {
		if err := e.Validate(); err != nil {
			t.Fatalf("validate %s: %v", e.Type, err)
		}
	}
}

func TestEvent_Validate(t *testing.T) {
	ok := []Event{
		{ID: "e1", Type: EventAuthorized, ProviderPaymentID: "p"},
		{ID: "e2", Type: EventCaptured, ProviderPaymentID: "p", AmountCents: 100, FeeCents: 3, SettlementDate: "2026-11-21"},
		{ID: "e3", Type: EventRefunded, ProviderPaymentID: "p", RefundID: "r", AmountCents: 10},
		{ID: "e4", Type: EventChargebackOpened, ProviderPaymentID: "p", ChargebackID: "cb", AmountCents: 10},
		{ID: "e5", Type: EventPayoutSettled, Payout: &Payout{PayoutID: "po", AmountCents: 97, SettlementDate: "2026-11-21",
			Items: []PayoutItem{{ProviderPaymentID: "p", Kind: PayoutItemCapture, AmountCents: 100, FeeCents: 3}}}},
	}
	for _, e := range ok {
		if err := e.Validate(); err != nil {
			t.Errorf("%s: %v", e.ID, err)
		}
	}
	bad := []Event{
		{ID: "", Type: EventAuthorized, ProviderPaymentID: "p"},
		{ID: "e", Type: "payment.exploded", ProviderPaymentID: "p"},
		{ID: "e", Type: EventCaptured, ProviderPaymentID: "p", AmountCents: 0},
		{ID: "e", Type: EventCaptured, ProviderPaymentID: "p", AmountCents: 10, FeeCents: 11},
		{ID: "e", Type: EventCaptured, ProviderPaymentID: "p", AmountCents: 10, SettlementDate: "2026-13-01"},
		{ID: "e", Type: EventRefunded, ProviderPaymentID: "p", AmountCents: 10},
		{ID: "e", Type: EventChargebackOpened, ProviderPaymentID: "p", ChargebackID: "cb"},
		{ID: "e", Type: EventPayoutSettled},
		{ID: "e", Type: EventPayoutSettled, Payout: &Payout{PayoutID: "po", SettlementDate: "2026-11-21", Items: []PayoutItem{{ProviderPaymentID: "p", Kind: "bonus", AmountCents: 1}}}},
		{ID: "e x", Type: EventAuthorized, ProviderPaymentID: "p"},
	}
	for i, e := range bad {
		if err := e.Validate(); !errors.Is(err, ErrInvalidEvent) {
			t.Errorf("bad %d: want ErrInvalidEvent, got %v", i, err)
		}
	}
	it := []PayoutItem{
		{Kind: PayoutItemCapture, AmountCents: 100, FeeCents: 3},
		{Kind: PayoutItemRefund, AmountCents: 10},
		{Kind: PayoutItemChargeback, AmountCents: 5},
		{Kind: PayoutItemChargebackReversal, AmountCents: 5},
	}
	var sum int64
	for _, x := range it {
		sum += x.NetCents()
	}
	if sum != 87 {
		t.Fatalf("net: %d", sum)
	}
}

func TestNew(t *testing.T) {
	if p, err := New("mock"); err != nil || p.Name() != "mock" {
		t.Fatalf("mock: %v", err)
	}
	if _, err := New("adyen"); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("unknown: %v", err)
	}
}

func TestMockOnboardMerchant_IdempotentAndScenarios(t *testing.T) {
	m := NewMockProvider(MockConfig{})
	ctx := context.Background()
	req := OnboardRequest{IdempotencyKey: "kerno_onboard_m1", MerchantID: "m1", RFC: "CACX7605101P8"}
	a, err := m.OnboardMerchant(ctx, req)
	if err != nil || a.ProviderMerchantID == "" || a.Status != "active" {
		t.Fatalf("onboard: %+v %v", a, err)
	}
	b, _ := m.OnboardMerchant(ctx, req)
	if b.ProviderMerchantID != a.ProviderMerchantID {
		t.Fatal("retry with the same key must return the same sub-merchant")
	}
	if _, err := m.OnboardMerchant(ctx, OnboardRequest{IdempotencyKey: "k", MerchantID: "m", RFC: "TIMEOUT"}); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("want unavailable, got %v", err)
	}
	if _, err := m.OnboardMerchant(ctx, OnboardRequest{IdempotencyKey: "k", MerchantID: "m", RFC: "REJECT"}); !errors.Is(err, ErrProviderRejected) {
		t.Fatalf("want rejected, got %v", err)
	}
	if m.Calls("OnboardMerchant") != 4 {
		t.Fatalf("calls = %d", m.Calls("OnboardMerchant"))
	}
}
