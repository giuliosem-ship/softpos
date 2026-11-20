package acquirer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// MockProviderName is the ACQUIRER_PROVIDER value of the mock adapter.
const MockProviderName = "mock"

// Deterministic mock scenarios, selected by the amount's last two digits
// (like processor test cards):
//
//	...51 -> the tap is declined (payment.declined, code "do_not_honor")
//	...52 -> every provider call times out (ErrProviderUnavailable)
//	other -> approved: payment.authorized + payment.captured
const (
	MockDeclineSuffix = 51
	MockTimeoutSuffix = 52
)

// MockScenario is the outcome the mock assigns to an amount.
type MockScenario string

const (
	MockApprove MockScenario = "approve"
	MockDecline MockScenario = "decline"
	MockTimeout MockScenario = "timeout"
)

// ScenarioForAmount returns the deterministic mock outcome of an amount.
func ScenarioForAmount(amountCents int64) MockScenario {
	switch amountCents % 100 {
	case MockDeclineSuffix:
		return MockDecline
	case MockTimeoutSuffix:
		return MockTimeout
	}
	return MockApprove
}

// MockConfig tunes the mock adapter.
type MockConfig struct {
	FeeBps         int64 // provider fee, default 250 bps (2.50 %), half-up
	SettlementDays int   // T+n payout, default 1
	Now            func() time.Time
}

// MockProvider is an in-process, deterministic Provider for tests and
// DEV_MODE. Ids derive from the idempotency keys, so a retry returns the same
// intent/refund. It never calls the network.
type MockProvider struct {
	cfg     MockConfig
	mu      sync.Mutex
	intents map[string]PaymentState // provider payment id -> state
	calls   map[string]int          // method -> count (tests)
}

var _ Provider = (*MockProvider)(nil)

func NewMockProvider(cfg MockConfig) *MockProvider {
	if cfg.FeeBps == 0 {
		cfg.FeeBps = 250
	}
	if cfg.SettlementDays == 0 {
		cfg.SettlementDays = 1
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	return &MockProvider{cfg: cfg, intents: map[string]PaymentState{}, calls: map[string]int{}}
}

func (m *MockProvider) Name() string { return MockProviderName }

// Calls reports how many times a method was invoked (tests).
func (m *MockProvider) Calls(method string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[method]
}

func mockID(prefix, key string) string {
	sum := sha256.Sum256([]byte(key))
	return prefix + hex.EncodeToString(sum[:12])
}

// FeeCents is the mock provider fee of a captured amount.
func (m *MockProvider) FeeCents(amountCents int64) int64 {
	return (amountCents*m.cfg.FeeBps + 5000) / 10000
}

func (m *MockProvider) CreatePaymentIntent(_ context.Context, req IntentRequest) (Intent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls["CreatePaymentIntent"]++
	if req.IdempotencyKey == "" || req.AmountCents <= 0 {
		return Intent{}, fmt.Errorf("%w: idempotency key and positive amount required", ErrProviderRejected)
	}
	if ScenarioForAmount(req.AmountCents) == MockTimeout {
		return Intent{}, fmt.Errorf("%w: mock timeout", ErrProviderUnavailable)
	}
	id := mockID("mock_pi_", req.IdempotencyKey)
	if _, ok := m.intents[id]; !ok {
		m.intents[id] = PaymentState{ProviderPaymentID: id, Status: StatusRequiresPayment, AmountCents: req.AmountCents}
	}
	return Intent{
		ProviderPaymentID: id,
		ClientSecret:      mockID("mock_cs_", "secret:"+req.IdempotencyKey),
		Status:            m.intents[id].Status,
		ExpiresAt:         m.cfg.Now().Add(15 * time.Minute),
	}, nil
}

func (m *MockProvider) GetPayment(_ context.Context, providerPaymentID string) (PaymentState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls["GetPayment"]++
	st, ok := m.intents[providerPaymentID]
	if !ok {
		return PaymentState{}, ErrPaymentNotFound
	}
	if ScenarioForAmount(st.AmountCents) == MockTimeout {
		return PaymentState{}, fmt.Errorf("%w: mock timeout", ErrProviderUnavailable)
	}
	return st, nil
}

func (m *MockProvider) Capture(_ context.Context, req CaptureRequest) (PaymentState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls["Capture"]++
	st, ok := m.intents[req.ProviderPaymentID]
	if !ok {
		return PaymentState{}, ErrPaymentNotFound
	}
	if ScenarioForAmount(st.AmountCents) == MockTimeout {
		return PaymentState{}, fmt.Errorf("%w: mock timeout", ErrProviderUnavailable)
	}
	amt := req.AmountCents
	if amt == 0 {
		amt = st.AmountCents
	}
	st.Status, st.CapturedCents, st.FeeCents = StatusCaptured, amt, m.FeeCents(amt)
	m.intents[req.ProviderPaymentID] = st
	return st, nil
}

// Refund acknowledges any refund of a positive amount as pending; amounts
// ending in 52 time out. The mock is stateless here so refunds keep working
// after a server restart (DEV_MODE).
func (m *MockProvider) Refund(_ context.Context, req RefundRequest) (RefundResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls["Refund"]++
	if req.IdempotencyKey == "" || req.AmountCents <= 0 || req.ProviderPaymentID == "" {
		return RefundResult{}, fmt.Errorf("%w: invalid refund", ErrProviderRejected)
	}
	if ScenarioForAmount(req.AmountCents) == MockTimeout {
		return RefundResult{}, fmt.Errorf("%w: mock timeout", ErrProviderUnavailable)
	}
	return RefundResult{ProviderRefundID: mockID("mock_re_", req.IdempotencyKey), Status: "pending"}, nil
}

func (m *MockProvider) Cancel(_ context.Context, req CancelRequest) (PaymentState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls["Cancel"]++
	st, ok := m.intents[req.ProviderPaymentID]
	if !ok {
		// Unknown after a restart: the mock accepts the cancellation.
		return PaymentState{ProviderPaymentID: req.ProviderPaymentID, Status: StatusCanceled}, nil
	}
	if ScenarioForAmount(st.AmountCents) == MockTimeout {
		return PaymentState{}, fmt.Errorf("%w: mock timeout", ErrProviderUnavailable)
	}
	if st.Status == StatusCaptured {
		return PaymentState{}, fmt.Errorf("%w: already captured", ErrProviderRejected)
	}
	st.Status = StatusCanceled
	m.intents[req.ProviderPaymentID] = st
	return st, nil
}

// ParseWebhookEvent decodes the normalized Event wire format.
func (m *MockProvider) ParseWebhookEvent(body []byte) (Event, error) {
	return ParseNormalizedEvent(body)
}

// ParseNormalizedEvent decodes and validates the normalized Event JSON.
func ParseNormalizedEvent(body []byte) (Event, error) {
	var e Event
	if err := json.Unmarshal(body, &e); err != nil {
		return Event{}, fmt.Errorf("%w: %v", ErrInvalidEvent, err)
	}
	if err := e.Validate(); err != nil {
		return Event{}, err
	}
	return e, nil
}

// SimulateTap returns the webhook events the processor would send after the
// buyer taps for providerPaymentID, following the amount's scenario
// (DEV_MODE simulation and tests). Event ids are deterministic.
func (m *MockProvider) SimulateTap(providerPaymentID, kernoPaymentID string, amountCents int64) []Event {
	now := m.cfg.Now()
	base := Event{ProviderPaymentID: providerPaymentID, KernoPaymentID: kernoPaymentID, OccurredAt: now}
	if ScenarioForAmount(amountCents) == MockDecline {
		e := base
		e.ID, e.Type, e.DeclineCode = "evt_decl_"+providerPaymentID, EventDeclined, "do_not_honor"
		return []Event{e}
	}
	auth := base
	auth.ID, auth.Type = "evt_auth_"+providerPaymentID, EventAuthorized
	capt := base
	capt.ID, capt.Type = "evt_capt_"+providerPaymentID, EventCaptured
	capt.AmountCents, capt.FeeCents = amountCents, m.FeeCents(amountCents)
	capt.SettlementDate = now.AddDate(0, 0, m.cfg.SettlementDays).Format("2006-01-02")
	return []Event{auth, capt}
}
