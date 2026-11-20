package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CardPaymentStatus is the Kerno-side state of a processor card payment. It
// only moves on the processor's signed webhooks (docs/ACQUIRER_INTEGRATION.md).
type CardPaymentStatus string

const (
	CardCreated            CardPaymentStatus = "CREATED"
	CardAuthorized         CardPaymentStatus = "AUTHORIZED"
	CardDeclined           CardPaymentStatus = "DECLINED"
	CardCanceled           CardPaymentStatus = "CANCELED"
	CardCaptured           CardPaymentStatus = "CAPTURED"
	CardPartiallyRefunded  CardPaymentStatus = "PARTIALLY_REFUNDED"
	CardRefunded           CardPaymentStatus = "REFUNDED"
	CardChargeback         CardPaymentStatus = "CHARGEBACK"
	CardChargebackReversed CardPaymentStatus = "CHARGEBACK_REVERSED"
)

// Captured reports whether funds were captured (settled into the ledger).
func (s CardPaymentStatus) Captured() bool {
	switch s {
	case CardCaptured, CardPartiallyRefunded, CardRefunded, CardChargeback, CardChargebackReversed:
		return true
	}
	return false
}

// Refundable reports whether a refund may be requested or applied.
func (s CardPaymentStatus) Refundable() bool {
	return s == CardCaptured || s == CardPartiallyRefunded || s == CardChargebackReversed
}

// CardPaymentRecord is one row of card_payments.
type CardPaymentRecord struct {
	ID                     string
	MerchantID             string
	Provider               string
	ProviderPaymentID      string // "" until the provider returned the intent
	IntentIdempotencyKey   string
	RequestHash            string
	AmountCents            int64
	Currency               string
	Reference              string
	PersonaMoral           bool
	Status                 CardPaymentStatus
	CapturedCents          int64
	RefundedCents          int64
	ChargebackCents        int64
	ProviderFeeCents       int64
	ProviderSettlementDate string // YYYY-MM-DD expected payout to Kerno
	ProviderPayoutID       string
	ProviderSettledAt      *time.Time
	DeclineCode            string
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// CardRefundStatus is the state of a refund request sent to the processor.
type CardRefundStatus string

const (
	RefundRequested CardRefundStatus = "REQUESTED" // row stored, provider call in flight / outcome unknown
	RefundPending   CardRefundStatus = "PENDING"   // provider acknowledged; waiting for the webhook
	RefundSucceeded CardRefundStatus = "SUCCEEDED" // webhook applied, ledger reversed
	RefundFailed    CardRefundStatus = "FAILED"    // rejected; the hold is released
)

// CardRefundRecord is one row of card_payment_refunds.
type CardRefundRecord struct {
	ID               string
	CardPaymentID    string
	MerchantID       string
	IdempotencyKey   string
	AmountCents      int64
	Status           CardRefundStatus
	ProviderRefundID string
	LastError        string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// AcquirerPayoutRecord is one processor payout to Kerno (acquirer_payouts).
type AcquirerPayoutRecord struct {
	Provider         string
	PayoutID         string
	AmountCents      int64
	ExpectedCents    int64
	SettlementDate   string
	ItemCount        int
	DiscrepancyCount int
	Status           string // RECONCILED | DISCREPANCY
	JournalID        string
	CreatedAt        time.Time
}

// Reconciliation item kinds.
const (
	ReconMissingPayment      = "MISSING_PAYMENT"
	ReconAmountMismatch      = "AMOUNT_MISMATCH"
	ReconFeeMismatch         = "FEE_MISMATCH"
	ReconDuplicateSettlement = "DUPLICATE_SETTLEMENT"
	ReconPayoutTotalMismatch = "PAYOUT_TOTAL_MISMATCH"
	ReconNotSettled          = "NOT_SETTLED"
)

// ReconciliationItem is one difference in the processor reconciliation report.
type ReconciliationItem struct {
	ID                int64
	DedupeKey         string
	Provider          string
	PayoutID          string
	CardPaymentID     string
	ProviderPaymentID string
	Kind              string
	ExpectedCents     int64
	ActualCents       int64
	Detail            string
	Status            string // OPEN | RESOLVED
	ResolutionNote    string
	ResolvedBy        string // user id of the operator who resolved it
	CreatedAt         time.Time
	ResolvedAt        *time.Time
}

// ErrCardPaymentConflict is returned when a card payment / refund row changed
// under the caller (it was not locked or not found).
var ErrCardPaymentConflict = errors.New("card payment record not found")

// CardPaymentTx is the card-payment part of MoneyTx.
type CardPaymentTx interface {
	// CreateCardPayment inserts rec; if (merchant, intent key) exists the
	// existing row is returned locked with created=false.
	CreateCardPayment(ctx context.Context, rec CardPaymentRecord) (CardPaymentRecord, bool, error)
	GetCardPaymentForUpdate(ctx context.Context, id string) (CardPaymentRecord, bool, error)
	GetCardPaymentByProviderForUpdate(ctx context.Context, provider, providerPaymentID string) (CardPaymentRecord, bool, error)
	// UpdateCardPayment writes every mutable column of a locked row.
	UpdateCardPayment(ctx context.Context, rec CardPaymentRecord) error

	CreateCardRefund(ctx context.Context, rec CardRefundRecord) (CardRefundRecord, bool, error)
	GetCardRefundForUpdate(ctx context.Context, id string) (CardRefundRecord, bool, error)
	GetCardRefundByKeyForUpdate(ctx context.Context, cardPaymentID, idempotencyKey string) (CardRefundRecord, bool, error)
	GetCardRefundByProviderForUpdate(ctx context.Context, cardPaymentID, providerRefundID string) (CardRefundRecord, bool, error)
	UpdateCardRefund(ctx context.Context, rec CardRefundRecord) error
	// OpenRefundCents sums REQUESTED + PENDING refunds of a payment.
	OpenRefundCents(ctx context.Context, cardPaymentID string) (int64, error)

	// InsertAcquirerPayout stores a payout (created=false if it already exists).
	InsertAcquirerPayout(ctx context.Context, rec AcquirerPayoutRecord) (bool, error)
	// InsertReconciliationItem stores a difference (created=false on a reused dedupe key).
	InsertReconciliationItem(ctx context.Context, item ReconciliationItem) (bool, error)
}

// AcquirerRepository reads and maintains processor reconciliation state
// outside a unit of work (operator report, overdue-settlement job).
// SQLMoneyStore and MemoryMoneyStore implement it.
type AcquirerRepository interface {
	// ListUnsettledCaptures returns captured payments with no provider payout
	// whose expected settlement date is before settlementDateBefore (YYYY-MM-DD).
	ListUnsettledCaptures(ctx context.Context, settlementDateBefore string, limit int) ([]CardPaymentRecord, error)
	InsertReconciliationItem(ctx context.Context, item ReconciliationItem) (bool, error)
	ListReconciliationItems(ctx context.Context, status string, limit int) ([]ReconciliationItem, error)
	// ResolveReconciliationItem marks an OPEN item RESOLVED by the operator
	// resolvedBy (false if not open).
	ResolveReconciliationItem(ctx context.Context, id int64, note, resolvedBy string) (bool, error)
	ListAcquirerPayouts(ctx context.Context, limit int) ([]AcquirerPayoutRecord, error)
}

var _ AcquirerRepository = (*SQLMoneyStore)(nil)

// ----------------------------------------------------------------------------
// SQL implementation
// ----------------------------------------------------------------------------

const cardPaymentColumns = `id, merchant_id, provider, COALESCE(provider_payment_id, ''), intent_idempotency_key,
	request_hash, amount_cents, currency, reference, persona_moral, status, captured_cents, refunded_cents,
	chargeback_cents, provider_fee_cents, provider_settlement_date, provider_payout_id, provider_settled_at,
	decline_code, created_at, updated_at`

func scanCardPayment(row rowScanner) (CardPaymentRecord, error) {
	var r CardPaymentRecord
	var status string
	var settledAt sql.NullTime
	err := row.Scan(&r.ID, &r.MerchantID, &r.Provider, &r.ProviderPaymentID, &r.IntentIdempotencyKey,
		&r.RequestHash, &r.AmountCents, &r.Currency, &r.Reference, &r.PersonaMoral, &status, &r.CapturedCents,
		&r.RefundedCents, &r.ChargebackCents, &r.ProviderFeeCents, &r.ProviderSettlementDate, &r.ProviderPayoutID,
		&settledAt, &r.DeclineCode, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return CardPaymentRecord{}, err
	}
	r.Status = CardPaymentStatus(status)
	if settledAt.Valid {
		t := settledAt.Time.UTC()
		r.ProviderSettledAt = &t
	}
	return r, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (t *sqlMoneyTx) queryCardPayment(ctx context.Context, where string, args ...any) (CardPaymentRecord, bool, error) {
	r, err := scanCardPayment(t.tx.QueryRowContext(ctx, `SELECT `+cardPaymentColumns+` FROM card_payments WHERE `+where+` FOR UPDATE`, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return CardPaymentRecord{}, false, nil
	}
	if err != nil {
		return CardPaymentRecord{}, false, fmt.Errorf("failed to lock card payment: %w", err)
	}
	return r, true, nil
}

func (t *sqlMoneyTx) CreateCardPayment(ctx context.Context, r CardPaymentRecord) (CardPaymentRecord, bool, error) {
	if r.Status == "" {
		r.Status = CardCreated
	}
	if r.Currency == "" {
		r.Currency = "MXN"
	}
	now := time.Now().UTC()
	res, err := t.tx.ExecContext(ctx, `
		INSERT INTO card_payments (id, merchant_id, provider, provider_payment_id, intent_idempotency_key, request_hash,
			amount_cents, currency, reference, persona_moral, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12)
		ON CONFLICT (merchant_id, intent_idempotency_key) DO NOTHING
	`, r.ID, r.MerchantID, r.Provider, nullIfEmpty(r.ProviderPaymentID), r.IntentIdempotencyKey, r.RequestHash,
		r.AmountCents, r.Currency, r.Reference, r.PersonaMoral, string(r.Status), now)
	if err != nil {
		return CardPaymentRecord{}, false, fmt.Errorf("failed to insert card payment: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 1 {
		r.CreatedAt, r.UpdatedAt = now, now
		return r, true, nil
	}
	existing, found, err := t.queryCardPayment(ctx, `merchant_id = $1 AND intent_idempotency_key = $2`, r.MerchantID, r.IntentIdempotencyKey)
	if err != nil {
		return CardPaymentRecord{}, false, err
	}
	if !found {
		return CardPaymentRecord{}, false, ErrCardPaymentConflict
	}
	return existing, false, nil
}

func (t *sqlMoneyTx) GetCardPaymentForUpdate(ctx context.Context, id string) (CardPaymentRecord, bool, error) {
	return t.queryCardPayment(ctx, `id = $1`, id)
}

func (t *sqlMoneyTx) GetCardPaymentByProviderForUpdate(ctx context.Context, provider, providerPaymentID string) (CardPaymentRecord, bool, error) {
	return t.queryCardPayment(ctx, `provider = $1 AND provider_payment_id = $2`, provider, providerPaymentID)
}

func (t *sqlMoneyTx) UpdateCardPayment(ctx context.Context, r CardPaymentRecord) error {
	var settledAt any
	if r.ProviderSettledAt != nil {
		settledAt = r.ProviderSettledAt.UTC()
	}
	res, err := t.tx.ExecContext(ctx, `
		UPDATE card_payments SET provider_payment_id = $1, status = $2, captured_cents = $3, refunded_cents = $4,
			chargeback_cents = $5, provider_fee_cents = $6, provider_settlement_date = $7, provider_payout_id = $8,
			provider_settled_at = $9, decline_code = $10, updated_at = $11
		WHERE id = $12
	`, nullIfEmpty(r.ProviderPaymentID), string(r.Status), r.CapturedCents, r.RefundedCents, r.ChargebackCents,
		r.ProviderFeeCents, r.ProviderSettlementDate, r.ProviderPayoutID, settledAt, r.DeclineCode, time.Now().UTC(), r.ID)
	if err != nil {
		return fmt.Errorf("failed to update card payment: %w", err)
	}
	return requireRowAffected(res, ErrCardPaymentConflict)
}

const cardRefundColumns = `id, card_payment_id, merchant_id, idempotency_key, amount_cents, status,
	COALESCE(provider_refund_id, ''), last_error, created_at, updated_at`

func (t *sqlMoneyTx) queryCardRefund(ctx context.Context, where string, args ...any) (CardRefundRecord, bool, error) {
	var r CardRefundRecord
	var status string
	err := t.tx.QueryRowContext(ctx, `SELECT `+cardRefundColumns+` FROM card_payment_refunds WHERE `+where+` FOR UPDATE`, args...).Scan(
		&r.ID, &r.CardPaymentID, &r.MerchantID, &r.IdempotencyKey, &r.AmountCents, &status, &r.ProviderRefundID,
		&r.LastError, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return CardRefundRecord{}, false, nil
	}
	if err != nil {
		return CardRefundRecord{}, false, fmt.Errorf("failed to lock card refund: %w", err)
	}
	r.Status = CardRefundStatus(status)
	return r, true, nil
}

func (t *sqlMoneyTx) CreateCardRefund(ctx context.Context, r CardRefundRecord) (CardRefundRecord, bool, error) {
	if r.Status == "" {
		r.Status = RefundRequested
	}
	now := time.Now().UTC()
	res, err := t.tx.ExecContext(ctx, `
		INSERT INTO card_payment_refunds (id, card_payment_id, merchant_id, idempotency_key, amount_cents, status,
			provider_refund_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)
		ON CONFLICT (card_payment_id, idempotency_key) DO NOTHING
	`, r.ID, r.CardPaymentID, r.MerchantID, r.IdempotencyKey, r.AmountCents, string(r.Status), nullIfEmpty(r.ProviderRefundID), now)
	if err != nil {
		return CardRefundRecord{}, false, fmt.Errorf("failed to insert card refund: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 1 {
		r.CreatedAt, r.UpdatedAt = now, now
		return r, true, nil
	}
	existing, found, err := t.queryCardRefund(ctx, `card_payment_id = $1 AND idempotency_key = $2`, r.CardPaymentID, r.IdempotencyKey)
	if err != nil {
		return CardRefundRecord{}, false, err
	}
	if !found {
		return CardRefundRecord{}, false, ErrCardPaymentConflict
	}
	return existing, false, nil
}

func (t *sqlMoneyTx) GetCardRefundForUpdate(ctx context.Context, id string) (CardRefundRecord, bool, error) {
	return t.queryCardRefund(ctx, `id = $1`, id)
}

func (t *sqlMoneyTx) GetCardRefundByKeyForUpdate(ctx context.Context, cardPaymentID, idempotencyKey string) (CardRefundRecord, bool, error) {
	return t.queryCardRefund(ctx, `card_payment_id = $1 AND idempotency_key = $2`, cardPaymentID, idempotencyKey)
}

func (t *sqlMoneyTx) GetCardRefundByProviderForUpdate(ctx context.Context, cardPaymentID, providerRefundID string) (CardRefundRecord, bool, error) {
	return t.queryCardRefund(ctx, `card_payment_id = $1 AND provider_refund_id = $2`, cardPaymentID, providerRefundID)
}

func (t *sqlMoneyTx) UpdateCardRefund(ctx context.Context, r CardRefundRecord) error {
	res, err := t.tx.ExecContext(ctx, `
		UPDATE card_payment_refunds SET status = $1, provider_refund_id = $2, last_error = $3, updated_at = $4 WHERE id = $5
	`, string(r.Status), nullIfEmpty(r.ProviderRefundID), r.LastError, time.Now().UTC(), r.ID)
	if err != nil {
		return fmt.Errorf("failed to update card refund: %w", err)
	}
	return requireRowAffected(res, ErrCardPaymentConflict)
}

func (t *sqlMoneyTx) OpenRefundCents(ctx context.Context, cardPaymentID string) (int64, error) {
	var sum int64
	err := t.tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount_cents), 0) FROM card_payment_refunds
		WHERE card_payment_id = $1 AND status IN ('REQUESTED', 'PENDING')
	`, cardPaymentID).Scan(&sum)
	if err != nil {
		return 0, fmt.Errorf("failed to sum open refunds: %w", err)
	}
	return sum, nil
}

func (t *sqlMoneyTx) InsertAcquirerPayout(ctx context.Context, r AcquirerPayoutRecord) (bool, error) {
	res, err := t.tx.ExecContext(ctx, `
		INSERT INTO acquirer_payouts (provider, payout_id, amount_cents, expected_cents, settlement_date, item_count,
			discrepancy_count, status, journal_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (provider, payout_id) DO NOTHING
	`, r.Provider, r.PayoutID, r.AmountCents, r.ExpectedCents, r.SettlementDate, r.ItemCount, r.DiscrepancyCount,
		r.Status, r.JournalID, time.Now().UTC())
	if err != nil {
		return false, fmt.Errorf("failed to insert acquirer payout: %w", err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func insertReconItem(ctx context.Context, exec interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, it ReconciliationItem) (bool, error) {
	res, err := exec.ExecContext(ctx, `
		INSERT INTO acquirer_reconciliation_items (dedupe_key, provider, payout_id, card_payment_id, provider_payment_id,
			kind, expected_cents, actual_cents, detail, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'OPEN', $10)
		ON CONFLICT (dedupe_key) DO NOTHING
	`, it.DedupeKey, it.Provider, it.PayoutID, it.CardPaymentID, it.ProviderPaymentID, it.Kind, it.ExpectedCents,
		it.ActualCents, it.Detail, time.Now().UTC())
	if err != nil {
		return false, fmt.Errorf("failed to insert reconciliation item: %w", err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (t *sqlMoneyTx) InsertReconciliationItem(ctx context.Context, it ReconciliationItem) (bool, error) {
	return insertReconItem(ctx, t.tx, it)
}

// --- AcquirerRepository on SQLMoneyStore -------------------------------------

func (s *SQLMoneyStore) ListUnsettledCaptures(ctx context.Context, before string, limit int) ([]CardPaymentRecord, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+cardPaymentColumns+` FROM card_payments
		WHERE provider_payout_id = '' AND captured_cents > 0 AND provider_settlement_date <> ''
		  AND provider_settlement_date < $1
		ORDER BY provider_settlement_date, id LIMIT $2`, before, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to list unsettled captures: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []CardPaymentRecord
	for rows.Next() {
		r, err := scanCardPayment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *SQLMoneyStore) InsertReconciliationItem(ctx context.Context, it ReconciliationItem) (bool, error) {
	return insertReconItem(ctx, s.db, it)
}

func (s *SQLMoneyStore) ListReconciliationItems(ctx context.Context, status string, limit int) ([]ReconciliationItem, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, dedupe_key, provider, payout_id, card_payment_id, provider_payment_id, kind, expected_cents,
			actual_cents, detail, status, resolution_note, resolved_by, created_at, resolved_at
		FROM acquirer_reconciliation_items
		WHERE ($1 = '' OR status = $1)
		ORDER BY id DESC LIMIT $2`, status, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to list reconciliation items: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ReconciliationItem
	for rows.Next() {
		var it ReconciliationItem
		var resolved sql.NullTime
		if err := rows.Scan(&it.ID, &it.DedupeKey, &it.Provider, &it.PayoutID, &it.CardPaymentID, &it.ProviderPaymentID,
			&it.Kind, &it.ExpectedCents, &it.ActualCents, &it.Detail, &it.Status, &it.ResolutionNote, &it.ResolvedBy, &it.CreatedAt, &resolved); err != nil {
			return nil, err
		}
		if resolved.Valid {
			t := resolved.Time.UTC()
			it.ResolvedAt = &t
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func (s *SQLMoneyStore) ResolveReconciliationItem(ctx context.Context, id int64, note, resolvedBy string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE acquirer_reconciliation_items SET status = 'RESOLVED', resolution_note = $1, resolved_by = $2, resolved_at = $3
		WHERE id = $4 AND status = 'OPEN'`, note, resolvedBy, time.Now().UTC(), id)
	if err != nil {
		return false, fmt.Errorf("failed to resolve reconciliation item: %w", err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *SQLMoneyStore) ListAcquirerPayouts(ctx context.Context, limit int) ([]AcquirerPayoutRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT provider, payout_id, amount_cents, expected_cents, settlement_date, item_count, discrepancy_count,
			status, journal_id, created_at
		FROM acquirer_payouts ORDER BY created_at DESC, payout_id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to list acquirer payouts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []AcquirerPayoutRecord
	for rows.Next() {
		var p AcquirerPayoutRecord
		if err := rows.Scan(&p.Provider, &p.PayoutID, &p.AmountCents, &p.ExpectedCents, &p.SettlementDate, &p.ItemCount,
			&p.DiscrepancyCount, &p.Status, &p.JournalID, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
