package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"softpos/pkg/ledger"
	"softpos/pkg/payments"
)

// MemoryOutboxEvent is an event enqueued by a committed MemoryMoneyStore unit of work.
type MemoryOutboxEvent struct {
	EventType   string
	AggregateID string
	Payload     []byte
}

type memOperation struct {
	statusCode int
	response   []byte
}

// MemoryMoneyStore is the no-DB MoneyStore. Units of work are serialized by a
// single mutex; writes are buffered in the transaction and applied only after
// fn succeeds and every journal/account has been validated, so a failed unit of
// work leaves no partial state. It operates on the same in-memory ledger,
// payment engine and merchant repository the API uses.
type MemoryMoneyStore struct {
	mu          sync.Mutex
	ledger      *ledger.LedgerEngine
	processor   func() payments.PaymentProcessor
	merchants   func() MerchantRepository
	settlements map[string]SettlementRecord
	operations  map[string]memOperation
	journalIDs  map[string]bool
	journals    []*ledger.JournalEntry // committed, in order (backoffice ledger view)
	events      []MemoryOutboxEvent
	payouts     map[string]PayoutRecord // by clave_rastreo
	payoutKeys  map[string]string       // merchant_id + "\x00" + idempotency key -> clave
	card        memCardState            // processor card payments (card_payments_memory.go)
	sales       []SaleRecord            // credited SPEI / CoDi sales (sales.go)
	saleIDs     map[string]bool
}

var (
	_ MoneyStore       = (*MemoryMoneyStore)(nil)
	_ PayoutRepository = (*MemoryMoneyStore)(nil)
)

// NewMemoryMoneyStore builds the in-memory unit of work. processor and
// merchants are resolved at the start of every transaction so later Set* calls
// on the server are honored.
func NewMemoryMoneyStore(le *ledger.LedgerEngine, processor func() payments.PaymentProcessor, merchants func() MerchantRepository) *MemoryMoneyStore {
	return &MemoryMoneyStore{
		ledger:      le,
		processor:   processor,
		merchants:   merchants,
		settlements: make(map[string]SettlementRecord),
		operations:  make(map[string]memOperation),
		journalIDs:  make(map[string]bool),
		payouts:     make(map[string]PayoutRecord),
		payoutKeys:  make(map[string]string),
		card:        newMemCardState(),
		saleIDs:     make(map[string]bool),
	}
}

// ListPayoutsForRecovery implements PayoutRepository.
func (s *MemoryMoneyStore) ListPayoutsForRecovery(_ context.Context, olderThan time.Time, limit int) ([]PayoutRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []PayoutRecord
	for _, p := range s.payouts {
		if (p.Status == PayoutReserved || p.Status == PayoutPendingReconciliation) && !p.UpdatedAt.After(olderThan) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.Before(out[j].UpdatedAt)
		}
		return out[i].ClaveRastreo < out[j].ClaveRastreo
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Payout returns a committed payout (tests / dev inspection).
func (s *MemoryMoneyStore) Payout(claveRastreo string) (PayoutRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.payouts[claveRastreo]
	return p, ok
}

// SetPayoutUpdatedAt backdates a committed payout (tests of the reconciler's age filter).
func (s *MemoryMoneyStore) SetPayoutUpdatedAt(claveRastreo string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.payouts[claveRastreo]; ok {
		p.UpdatedAt = at
		s.payouts[claveRastreo] = p
	}
}

// Events returns a copy of the committed outbox events (tests / dev inspection).
func (s *MemoryMoneyStore) Events() []MemoryOutboxEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]MemoryOutboxEvent(nil), s.events...)
}

func (s *MemoryMoneyStore) InTx(ctx context.Context, fn func(tx MoneyTx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx := &memMoneyTx{
		store:       s,
		proc:        s.processor(),
		merchants:   s.merchants(),
		payments:    make(map[string]*payments.Payment),
		settlements: make(map[string]SettlementRecord),
		operations:  make(map[string]memOperation),
		claimed:     make(map[string]bool),
		pendingBal:  make(map[string]int64),
		salesDelta:  make(map[[2]string]int64),
		payouts:     make(map[string]PayoutRecord),
		card:        newMemCardTxState(),
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.commit(ctx)
}

type statusChange struct {
	id       string
	from, to payments.PaymentStatus
	amount   int64
}

type memMoneyTx struct {
	store       *MemoryMoneyStore
	proc        payments.PaymentProcessor
	merchants   MerchantRepository
	payments    map[string]*payments.Payment
	changes     []statusChange
	settlements map[string]SettlementRecord
	operations  map[string]memOperation
	claimed     map[string]bool
	journals    []*ledger.JournalEntry
	pendingBal  map[string]int64
	salesDelta  map[[2]string]int64
	events      []MemoryOutboxEvent
	payouts     map[string]PayoutRecord
	card        memCardTxState
	sales       []SaleRecord
}

func payoutKey(merchantID, idemKey string) string { return merchantID + "\x00" + idemKey }

func (t *memMoneyTx) lookupPayout(clave string) (PayoutRecord, bool) {
	if p, ok := t.payouts[clave]; ok {
		return p, true
	}
	p, ok := t.store.payouts[clave]
	return p, ok
}

func (t *memMoneyTx) CreatePayout(_ context.Context, p PayoutRecord) (PayoutRecord, bool, error) {
	k := payoutKey(p.MerchantID, p.IdempotencyKey)
	if clave, ok := t.store.payoutKeys[k]; ok {
		existing, _ := t.lookupPayout(clave)
		return existing, false, nil
	}
	for _, q := range t.payouts {
		if payoutKey(q.MerchantID, q.IdempotencyKey) == k {
			return q, false, nil
		}
	}
	if _, dup := t.lookupPayout(p.ClaveRastreo); dup {
		return PayoutRecord{}, false, fmt.Errorf("failed to insert payout: duplicate clave_rastreo %s", p.ClaveRastreo)
	}
	if p.Status == "" {
		p.Status = PayoutReserved
	}
	now := time.Now().UTC()
	p.CreatedAt, p.UpdatedAt = now, now
	t.payouts[p.ClaveRastreo] = p
	return p, true, nil
}

func (t *memMoneyTx) SetPayoutRemainingBalance(_ context.Context, clave string, cents int64) error {
	p, ok := t.lookupPayout(clave)
	if !ok {
		return ErrPayoutStateConflict
	}
	p.RemainingBalanceCents = cents
	t.payouts[clave] = p
	return nil
}

func (t *memMoneyTx) GetPayoutForUpdate(_ context.Context, clave string) (PayoutRecord, bool, error) {
	p, ok := t.lookupPayout(clave)
	return p, ok, nil
}

func (t *memMoneyTx) SetPayoutStatus(_ context.Context, clave string, from, to PayoutStatus, stpOrderID int64, lastError string) error {
	p, ok := t.lookupPayout(clave)
	if !ok || p.Status != from {
		return ErrPayoutStateConflict
	}
	p.Status = to
	if stpOrderID != 0 {
		p.STPOrderID = stpOrderID
	}
	p.LastError = lastError
	p.UpdatedAt = time.Now().UTC()
	t.payouts[clave] = p
	return nil
}

func (t *memMoneyTx) GetPaymentForUpdate(_ context.Context, paymentID string) (*payments.Payment, error) {
	if p, ok := t.payments[paymentID]; ok {
		cp := *p
		return &cp, nil
	}
	p, err := t.proc.GetPayment(paymentID)
	if err != nil {
		return nil, err
	}
	cp := *p
	t.payments[paymentID] = &cp
	out := cp
	return &out, nil
}

func (t *memMoneyTx) SetPaymentStatus(ctx context.Context, paymentID string, from, to payments.PaymentStatus) error {
	p, err := t.GetPaymentForUpdate(ctx, paymentID)
	if err != nil {
		return err
	}
	if p.Status != from {
		return payments.ErrInvalidStateTransition
	}
	t.payments[paymentID].Status = to
	t.payments[paymentID].UpdatedAt = time.Now().UTC()
	t.changes = append(t.changes, statusChange{id: paymentID, from: from, to: to, amount: p.AmountCents})
	return nil
}

func (t *memMoneyTx) GetSettlementForUpdate(_ context.Context, paymentID string) (SettlementRecord, bool, error) {
	if r, ok := t.settlements[paymentID]; ok {
		return r, true, nil
	}
	r, ok := t.store.settlements[paymentID]
	return r, ok, nil
}

func (t *memMoneyTx) InsertSettlement(ctx context.Context, rec SettlementRecord) error {
	if _, exists, _ := t.GetSettlementForUpdate(ctx, rec.PaymentID); exists {
		return ErrDuplicateSettlement
	}
	now := time.Now().UTC()
	rec.CreatedAt, rec.UpdatedAt = now, now
	t.settlements[rec.PaymentID] = rec
	return nil
}

func (t *memMoneyTx) UpdateSettlementRefunds(ctx context.Context, rec SettlementRecord) error {
	cur, exists, _ := t.GetSettlementForUpdate(ctx, rec.PaymentID)
	if !exists {
		return ErrSettlementNotFound
	}
	cur.RefundedGrossCents = rec.RefundedGrossCents
	cur.RefundedBaseCents = rec.RefundedBaseCents
	cur.RefundedTaxEscrowCents = rec.RefundedTaxEscrowCents
	cur.RefundedFeeCents = rec.RefundedFeeCents
	cur.UpdatedAt = time.Now().UTC()
	t.settlements[rec.PaymentID] = cur
	return nil
}

func (t *memMoneyTx) ClaimOperation(_ context.Context, key string) (bool, int, []byte, error) {
	if op, ok := t.store.operations[key]; ok {
		return false, op.statusCode, append([]byte(nil), op.response...), nil
	}
	if t.claimed[key] {
		op := t.operations[key]
		return false, op.statusCode, append([]byte(nil), op.response...), nil
	}
	t.claimed[key] = true
	t.operations[key] = memOperation{}
	return true, 0, nil, nil
}

func (t *memMoneyTx) CompleteOperation(_ context.Context, key string, statusCode int, response []byte) error {
	if !t.claimed[key] {
		return fmt.Errorf("operation %q was not claimed in this transaction", key)
	}
	t.operations[key] = memOperation{statusCode: statusCode, response: append([]byte(nil), response...)}
	return nil
}

func (t *memMoneyTx) LockAccountBalances(_ context.Context, accountCodes ...string) (map[string]int64, error) {
	out := make(map[string]int64, len(accountCodes))
	for _, code := range accountCodes {
		bal, err := t.store.ledger.GetBalance(code)
		if err != nil {
			return nil, err
		}
		out[code] = bal.AmountCents + t.pendingBal[code]
	}
	return out, nil
}

func (t *memMoneyTx) PostJournal(_ context.Context, journal *ledger.JournalEntry) error {
	if journal == nil || !journal.IsBalanced() {
		return ErrUnbalancedJournal
	}
	if t.store.journalIDs[journal.ID] {
		return ErrDuplicateJournal
	}
	for _, j := range t.journals {
		if j.ID == journal.ID {
			return ErrDuplicateJournal
		}
	}
	deltas := make(map[string]int64)
	for _, e := range journal.Entries {
		acc, err := t.store.ledger.GetAccount(e.AccountID)
		if err != nil {
			return err
		}
		debitNormal := acc.Type == ledger.AccountAsset || acc.Type == ledger.AccountExpense
		if (e.Direction == ledger.Debit) == debitNormal {
			deltas[e.AccountID] += e.Money.AmountCents
		} else {
			deltas[e.AccountID] -= e.Money.AmountCents
		}
	}
	for k, v := range deltas {
		t.pendingBal[k] += v
	}
	t.journals = append(t.journals, journal)
	return nil
}

func (t *memMoneyTx) MonthlySalesForUpdate(ctx context.Context, merchantID, yearMonth string) (int64, error) {
	if _, found, err := t.merchants.GetMerchant(ctx, merchantID); err != nil {
		return 0, err
	} else if !found {
		return 0, ErrMerchantNotFound
	}
	cur, err := t.merchants.GetMonthlySalesCents(ctx, merchantID, yearMonth)
	if err != nil {
		return 0, err
	}
	return cur + t.salesDelta[[2]string{merchantID, yearMonth}], nil
}

func (t *memMoneyTx) AddMonthlySales(ctx context.Context, merchantID, yearMonth string, delta int64) error {
	cur, err := t.MonthlySalesForUpdate(ctx, merchantID, yearMonth)
	if err != nil {
		return err
	}
	if cur+delta < 0 {
		delta = -cur
	}
	t.salesDelta[[2]string{merchantID, yearMonth}] += delta
	return nil
}

func (t *memMoneyTx) EnqueueEvent(_ context.Context, eventType, aggregateID string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal outbox event payload: %w", err)
	}
	t.events = append(t.events, MemoryOutboxEvent{EventType: eventType, AggregateID: aggregateID, Payload: b})
	return nil
}

// monthlySalesAdjuster is implemented by MemoryMerchantRepository to apply
// negative (refund) deltas.
type monthlySalesAdjuster interface {
	AdjustMonthlySalesCents(ctx context.Context, merchantID, yearMonth string, delta int64) (int64, error)
}

func (t *memMoneyTx) commit(ctx context.Context) error {
	adjuster, canAdjust := t.merchants.(monthlySalesAdjuster)
	for _, d := range t.salesDelta {
		if d < 0 && !canAdjust {
			return fmt.Errorf("merchant repository %T cannot apply negative monthly sales deltas", t.merchants)
		}
	}

	// 1. Payment state (the only step that can still fail on a concurrent change).
	for _, c := range t.changes {
		var err error
		switch {
		case c.from == payments.PaymentAuthorized && c.to == payments.PaymentSettled:
			_, err = t.proc.Settle(c.id)
		case c.from == payments.PaymentAuthorized && c.to == payments.PaymentVoided:
			_, err = t.proc.Void(c.id)
		case c.from == payments.PaymentSettled && c.to == payments.PaymentRefunded:
			_, err = t.proc.Refund(c.id, c.amount)
		default:
			err = payments.ErrInvalidStateTransition
		}
		if err != nil {
			return fmt.Errorf("payment %s %s->%s: %w", c.id, c.from, c.to, err)
		}
	}
	// 2. Ledger (accounts validated in PostJournal).
	for _, j := range t.journals {
		if err := t.store.ledger.PostTransaction(j); err != nil {
			return fmt.Errorf("ledger post %s: %w", j.ID, err)
		}
		t.store.journalIDs[j.ID] = true
		t.store.journals = append(t.store.journals, j)
	}
	// 3. RESICO accumulators.
	for k, d := range t.salesDelta {
		var err error
		switch {
		case d > 0:
			_, err = t.merchants.AddMonthlySalesCents(ctx, k[0], k[1], d)
		case d < 0:
			_, err = adjuster.AdjustMonthlySalesCents(ctx, k[0], k[1], d)
		}
		if err != nil {
			return fmt.Errorf("monthly sales %s/%s: %w", k[0], k[1], err)
		}
	}
	// 4. Records, keys and events.
	for id, r := range t.settlements {
		t.store.settlements[id] = r
	}
	for k, op := range t.operations {
		t.store.operations[k] = op
	}
	for clave, p := range t.payouts {
		t.store.payouts[clave] = p
		t.store.payoutKeys[payoutKey(p.MerchantID, p.IdempotencyKey)] = clave
	}
	t.commitCard()
	for _, sr := range t.sales {
		if !t.store.saleIDs[sr.ID] {
			t.store.saleIDs[sr.ID] = true
			t.store.sales = append(t.store.sales, sr)
		}
	}
	t.store.events = append(t.store.events, t.events...)
	return nil
}
