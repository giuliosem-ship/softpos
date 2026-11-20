package store

import (
	"context"
	"sort"
	"time"
)

// memCardState is the committed card-payment state of a MemoryMoneyStore.
type memCardState struct {
	payments   map[string]CardPaymentRecord // by id
	intentKeys map[string]string            // merchant + "\x00" + intent key -> id
	byProvider map[string]string            // provider + "\x00" + provider payment id -> id
	refunds    map[string]CardRefundRecord  // by id
	refundKeys map[string]string            // card payment + "\x00" + idempotency key -> id
	payouts    map[string]AcquirerPayoutRecord
	recon      []ReconciliationItem
	reconKeys  map[string]bool
	reconSeq   int64
}

func newMemCardState() memCardState {
	return memCardState{
		payments:   map[string]CardPaymentRecord{},
		intentKeys: map[string]string{},
		byProvider: map[string]string{},
		refunds:    map[string]CardRefundRecord{},
		refundKeys: map[string]string{},
		payouts:    map[string]AcquirerPayoutRecord{},
		reconKeys:  map[string]bool{},
	}
}

// memCardTxState buffers card-payment writes of one unit of work.
type memCardTxState struct {
	payments map[string]CardPaymentRecord
	refunds  map[string]CardRefundRecord
	payouts  map[string]AcquirerPayoutRecord
	recon    []ReconciliationItem
}

func newMemCardTxState() memCardTxState {
	return memCardTxState{
		payments: map[string]CardPaymentRecord{},
		refunds:  map[string]CardRefundRecord{},
		payouts:  map[string]AcquirerPayoutRecord{},
	}
}

func k2(a, b string) string { return a + "\x00" + b }

func (t *memMoneyTx) cardByID(id string) (CardPaymentRecord, bool) {
	if r, ok := t.card.payments[id]; ok {
		return r, true
	}
	r, ok := t.store.card.payments[id]
	return r, ok
}

func (t *memMoneyTx) CreateCardPayment(_ context.Context, r CardPaymentRecord) (CardPaymentRecord, bool, error) {
	key := k2(r.MerchantID, r.IntentIdempotencyKey)
	if id, ok := t.store.card.intentKeys[key]; ok {
		existing, _ := t.cardByID(id)
		return existing, false, nil
	}
	for _, p := range t.card.payments {
		if k2(p.MerchantID, p.IntentIdempotencyKey) == key {
			return p, false, nil
		}
	}
	if r.Status == "" {
		r.Status = CardCreated
	}
	if r.Currency == "" {
		r.Currency = "MXN"
	}
	now := time.Now().UTC()
	r.CreatedAt, r.UpdatedAt = now, now
	t.card.payments[r.ID] = r
	return r, true, nil
}

func (t *memMoneyTx) GetCardPaymentForUpdate(_ context.Context, id string) (CardPaymentRecord, bool, error) {
	r, ok := t.cardByID(id)
	return r, ok, nil
}

func (t *memMoneyTx) GetCardPaymentByProviderForUpdate(_ context.Context, provider, ppid string) (CardPaymentRecord, bool, error) {
	for _, p := range t.card.payments {
		if p.Provider == provider && p.ProviderPaymentID == ppid && ppid != "" {
			return p, true, nil
		}
	}
	if id, ok := t.store.card.byProvider[k2(provider, ppid)]; ok {
		r, found := t.cardByID(id)
		if found && r.ProviderPaymentID == ppid {
			return r, true, nil
		}
	}
	return CardPaymentRecord{}, false, nil
}

func (t *memMoneyTx) UpdateCardPayment(_ context.Context, r CardPaymentRecord) error {
	cur, ok := t.cardByID(r.ID)
	if !ok {
		return ErrCardPaymentConflict
	}
	r.CreatedAt = cur.CreatedAt
	r.UpdatedAt = time.Now().UTC()
	t.card.payments[r.ID] = r
	return nil
}

func (t *memMoneyTx) refundByID(id string) (CardRefundRecord, bool) {
	if r, ok := t.card.refunds[id]; ok {
		return r, true
	}
	r, ok := t.store.card.refunds[id]
	return r, ok
}

func (t *memMoneyTx) CreateCardRefund(_ context.Context, r CardRefundRecord) (CardRefundRecord, bool, error) {
	key := k2(r.CardPaymentID, r.IdempotencyKey)
	if id, ok := t.store.card.refundKeys[key]; ok {
		existing, _ := t.refundByID(id)
		return existing, false, nil
	}
	for _, x := range t.card.refunds {
		if k2(x.CardPaymentID, x.IdempotencyKey) == key {
			return x, false, nil
		}
	}
	if r.Status == "" {
		r.Status = RefundRequested
	}
	now := time.Now().UTC()
	r.CreatedAt, r.UpdatedAt = now, now
	t.card.refunds[r.ID] = r
	return r, true, nil
}

func (t *memMoneyTx) GetCardRefundForUpdate(_ context.Context, id string) (CardRefundRecord, bool, error) {
	r, ok := t.refundByID(id)
	return r, ok, nil
}

func (t *memMoneyTx) allRefunds(cardPaymentID string) []CardRefundRecord {
	seen := map[string]bool{}
	var out []CardRefundRecord
	for id, r := range t.card.refunds {
		if r.CardPaymentID == cardPaymentID {
			out = append(out, r)
		}
		seen[id] = true
	}
	for id, r := range t.store.card.refunds {
		if !seen[id] && r.CardPaymentID == cardPaymentID {
			out = append(out, r)
		}
	}
	return out
}

func (t *memMoneyTx) GetCardRefundByProviderForUpdate(_ context.Context, cardPaymentID, providerRefundID string) (CardRefundRecord, bool, error) {
	if providerRefundID == "" {
		return CardRefundRecord{}, false, nil
	}
	for _, r := range t.allRefunds(cardPaymentID) {
		if r.ProviderRefundID == providerRefundID {
			return r, true, nil
		}
	}
	return CardRefundRecord{}, false, nil
}

func (t *memMoneyTx) UpdateCardRefund(_ context.Context, r CardRefundRecord) error {
	cur, ok := t.refundByID(r.ID)
	if !ok {
		return ErrCardPaymentConflict
	}
	cur.Status, cur.ProviderRefundID, cur.LastError = r.Status, r.ProviderRefundID, r.LastError
	cur.UpdatedAt = time.Now().UTC()
	t.card.refunds[r.ID] = cur
	return nil
}

func (t *memMoneyTx) OpenRefundCents(_ context.Context, cardPaymentID string) (int64, error) {
	var sum int64
	for _, r := range t.allRefunds(cardPaymentID) {
		if r.Status == RefundRequested || r.Status == RefundPending {
			sum += r.AmountCents
		}
	}
	return sum, nil
}

func (t *memMoneyTx) InsertAcquirerPayout(_ context.Context, r AcquirerPayoutRecord) (bool, error) {
	key := k2(r.Provider, r.PayoutID)
	if _, ok := t.store.card.payouts[key]; ok {
		return false, nil
	}
	if _, ok := t.card.payouts[key]; ok {
		return false, nil
	}
	r.CreatedAt = time.Now().UTC()
	t.card.payouts[key] = r
	return true, nil
}

func (t *memMoneyTx) InsertReconciliationItem(_ context.Context, it ReconciliationItem) (bool, error) {
	if t.store.card.reconKeys[it.DedupeKey] {
		return false, nil
	}
	for _, x := range t.card.recon {
		if x.DedupeKey == it.DedupeKey {
			return false, nil
		}
	}
	t.card.recon = append(t.card.recon, it)
	return true, nil
}

// commitCard applies the buffered card writes (cannot fail).
func (t *memMoneyTx) commitCard() {
	c := &t.store.card
	for id, r := range t.card.payments {
		if old, ok := c.payments[id]; ok && old.ProviderPaymentID != "" && old.ProviderPaymentID != r.ProviderPaymentID {
			delete(c.byProvider, k2(old.Provider, old.ProviderPaymentID))
		}
		c.payments[id] = r
		c.intentKeys[k2(r.MerchantID, r.IntentIdempotencyKey)] = id
		if r.ProviderPaymentID != "" {
			c.byProvider[k2(r.Provider, r.ProviderPaymentID)] = id
		}
	}
	for id, r := range t.card.refunds {
		c.refunds[id] = r
		c.refundKeys[k2(r.CardPaymentID, r.IdempotencyKey)] = id
	}
	for k, p := range t.card.payouts {
		c.payouts[k] = p
	}
	for _, it := range t.card.recon {
		c.addRecon(it)
	}
}

func (c *memCardState) addRecon(it ReconciliationItem) bool {
	if c.reconKeys[it.DedupeKey] {
		return false
	}
	c.reconSeq++
	it.ID = c.reconSeq
	if it.Status == "" {
		it.Status = "OPEN"
	}
	it.CreatedAt = time.Now().UTC()
	c.recon = append(c.recon, it)
	c.reconKeys[it.DedupeKey] = true
	return true
}

// --- AcquirerRepository on MemoryMoneyStore ----------------------------------

var _ AcquirerRepository = (*MemoryMoneyStore)(nil)

func (s *MemoryMoneyStore) ListUnsettledCaptures(_ context.Context, before string, limit int) ([]CardPaymentRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []CardPaymentRecord
	for _, p := range s.card.payments {
		if p.ProviderPayoutID == "" && p.CapturedCents > 0 && p.ProviderSettlementDate != "" && p.ProviderSettlementDate < before {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProviderSettlementDate != out[j].ProviderSettlementDate {
			return out[i].ProviderSettlementDate < out[j].ProviderSettlementDate
		}
		return out[i].ID < out[j].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *MemoryMoneyStore) InsertReconciliationItem(_ context.Context, it ReconciliationItem) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.card.addRecon(it), nil
}

func (s *MemoryMoneyStore) ListReconciliationItems(_ context.Context, status string, limit int) ([]ReconciliationItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ReconciliationItem
	for i := len(s.card.recon) - 1; i >= 0; i-- {
		it := s.card.recon[i]
		if status == "" || it.Status == status {
			out = append(out, it)
		}
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *MemoryMoneyStore) ResolveReconciliationItem(_ context.Context, id int64, note string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, it := range s.card.recon {
		if it.ID == id && it.Status == "OPEN" {
			now := time.Now().UTC()
			s.card.recon[i].Status, s.card.recon[i].ResolutionNote, s.card.recon[i].ResolvedAt = "RESOLVED", note, &now
			return true, nil
		}
	}
	return false, nil
}

func (s *MemoryMoneyStore) ListAcquirerPayouts(_ context.Context, limit int) ([]AcquirerPayoutRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AcquirerPayoutRecord, 0, len(s.card.payouts))
	for _, p := range s.card.payouts {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].PayoutID < out[j].PayoutID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// CardPayment returns a committed card payment (tests / dev inspection).
func (s *MemoryMoneyStore) CardPayment(id string) (CardPaymentRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.card.payments[id]
	return p, ok
}
