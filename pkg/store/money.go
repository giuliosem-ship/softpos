package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"softpos/pkg/ledger"
	"softpos/pkg/payments"
)

var (
	// ErrDuplicateSettlement is returned when a payment already has a settlement record.
	ErrDuplicateSettlement = errors.New("payment already settled")
	// ErrSettlementNotFound is returned when a settlement record is missing.
	ErrSettlementNotFound = errors.New("settlement not found")
)

// SettlementRecord is the server-computed split of a settled card payment,
// persisted in the same transaction as the payment state change and the ledger
// journal. Refund columns accumulate proportional reversals.
type SettlementRecord struct {
	PaymentID        string
	MerchantID       string
	GrossCents       int64
	BaseCents        int64
	IVACents         int64
	ISRCents         int64
	ISRRateBps       int64
	TaxEscrowCents   int64
	PlatformFeeBps   int64
	PlatformFeeCents int64
	RBFSweepCents    int64
	NetCents         int64
	PersonaMoral     bool
	YearMonth        string
	JournalID        string

	RefundedGrossCents     int64
	RefundedBaseCents      int64
	RefundedTaxEscrowCents int64
	RefundedFeeCents       int64

	CreatedAt time.Time
	UpdatedAt time.Time
}

// MoneyTx is one unit of work over every piece of money state: card payment
// status, settlement records, exactly-once operation keys, ledger balances and
// journals, RESICO monthly accumulators and outbox events. Everything written
// through a MoneyTx commits or rolls back together.
type MoneyTx interface {
	// GetPaymentForUpdate loads and locks a card payment (payments.ErrPaymentNotFound).
	GetPaymentForUpdate(ctx context.Context, paymentID string) (*payments.Payment, error)
	// SetPaymentStatus moves a locked payment from -> to (payments.ErrInvalidStateTransition).
	SetPaymentStatus(ctx context.Context, paymentID string, from, to payments.PaymentStatus) error

	// GetSettlementForUpdate loads and locks the settlement record of a payment.
	GetSettlementForUpdate(ctx context.Context, paymentID string) (SettlementRecord, bool, error)
	// InsertSettlement stores a new settlement (ErrDuplicateSettlement if one exists).
	InsertSettlement(ctx context.Context, rec SettlementRecord) error
	// UpdateSettlementRefunds persists the refund accumulators of rec.
	UpdateSettlementRefunds(ctx context.Context, rec SettlementRecord) error

	// ClaimOperation registers an exactly-once key. claimed=false means the key
	// was already committed by an earlier transaction; its stored response is
	// returned (a concurrent duplicate blocks until the first one finishes).
	ClaimOperation(ctx context.Context, key string) (claimed bool, statusCode int, response []byte, err error)
	// CompleteOperation stores the response replayed for a claimed key.
	CompleteOperation(ctx context.Context, key string, statusCode int, response []byte) error

	// LockAccountBalances locks the given ledger accounts (sorted order) and
	// returns their balances in normal-balance sign.
	LockAccountBalances(ctx context.Context, accountCodes ...string) (map[string]int64, error)
	// PostJournal posts a balanced journal (ErrDuplicateJournal on a reused id).
	PostJournal(ctx context.Context, journal *ledger.JournalEntry) error

	// MonthlySalesForUpdate locks and returns the RESICO accumulator of a month.
	MonthlySalesForUpdate(ctx context.Context, merchantID, yearMonth string) (int64, error)
	// AddMonthlySales adds delta (negative on refunds; floored at 0).
	AddMonthlySales(ctx context.Context, merchantID, yearMonth string, delta int64) error

	// EnqueueEvent records an outbox event that is published only if the
	// transaction commits.
	EnqueueEvent(ctx context.Context, eventType, aggregateID string, payload any) error

	// CreatePayout inserts p (status RESERVED unless set). If the merchant
	// already used p.IdempotencyKey, nothing is inserted and the existing
	// payout is returned locked with created=false.
	CreatePayout(ctx context.Context, p PayoutRecord) (existing PayoutRecord, created bool, err error)
	// SetPayoutRemainingBalance records the balance left after the reservation.
	SetPayoutRemainingBalance(ctx context.Context, claveRastreo string, cents int64) error
	// GetPayoutForUpdate loads and locks a payout.
	GetPayoutForUpdate(ctx context.Context, claveRastreo string) (PayoutRecord, bool, error)
	// SetPayoutStatus moves a payout from -> to (ErrPayoutStateConflict);
	// stpOrderID 0 keeps the stored one.
	SetPayoutStatus(ctx context.Context, claveRastreo string, from, to PayoutStatus, stpOrderID int64, lastError string) error
}

// MoneyStore runs units of work. fn's error rolls everything back.
type MoneyStore interface {
	InTx(ctx context.Context, fn func(tx MoneyTx) error) error
}

// ----------------------------------------------------------------------------
// SQLMoneyStore: one PostgreSQL transaction per unit of work
// ----------------------------------------------------------------------------

// SQLMoneyStore implements MoneyStore with a single database/sql transaction
// (READ COMMITTED + row locks). Lock order used by the API handlers: payment
// row -> settlement row -> operation key -> monthly sales row -> ledger
// accounts (sorted). A payout row is taken like an operation key: before the
// ledger accounts.
type SQLMoneyStore struct {
	db     *sql.DB
	outbox *SQLOutboxRepository
}

var _ MoneyStore = (*SQLMoneyStore)(nil)

func NewSQLMoneyStore(db *sql.DB) *SQLMoneyStore {
	return &SQLMoneyStore{db: db, outbox: NewSQLOutboxRepository(db)}
}

func (s *SQLMoneyStore) InTx(ctx context.Context, fn func(tx MoneyTx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin money transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(&sqlMoneyTx{tx: tx, outbox: s.outbox}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit money transaction: %w", err)
	}
	return nil
}

type sqlMoneyTx struct {
	tx     *sql.Tx
	outbox *SQLOutboxRepository
}

func (t *sqlMoneyTx) GetPaymentForUpdate(ctx context.Context, paymentID string) (*payments.Payment, error) {
	var p payments.Payment
	var status string
	err := t.tx.QueryRowContext(ctx, `
		SELECT id, merchant_id, amount_cents, status, masked_pan, created_at, updated_at
		FROM emv_payments
		WHERE id = $1
		FOR UPDATE
	`, paymentID).Scan(&p.ID, &p.MerchantID, &p.AmountCents, &status, &p.MaskedPAN, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, payments.ErrPaymentNotFound
		}
		return nil, fmt.Errorf("failed to lock payment: %w", err)
	}
	p.Status = payments.PaymentStatus(status)
	return &p, nil
}

func (t *sqlMoneyTx) SetPaymentStatus(ctx context.Context, paymentID string, from, to payments.PaymentStatus) error {
	res, err := t.tx.ExecContext(ctx, `
		UPDATE emv_payments SET status = $1, updated_at = $2
		WHERE id = $3 AND status = $4
	`, string(to), time.Now().UTC(), paymentID, string(from))
	if err != nil {
		return fmt.Errorf("failed to update payment status: %w", err)
	}
	return requireRowAffected(res, payments.ErrInvalidStateTransition)
}

const settlementColumns = `payment_id, merchant_id, gross_cents, base_cents, iva_cents, isr_cents, isr_rate_bps,
	tax_escrow_cents, platform_fee_bps, platform_fee_cents, rbf_sweep_cents, net_available_cents,
	persona_moral, year_month, journal_id, refunded_gross_cents, refunded_base_cents,
	refunded_tax_escrow_cents, refunded_fee_cents, created_at, updated_at`

func (t *sqlMoneyTx) GetSettlementForUpdate(ctx context.Context, paymentID string) (SettlementRecord, bool, error) {
	var r SettlementRecord
	err := t.tx.QueryRowContext(ctx, `SELECT `+settlementColumns+` FROM payment_settlements WHERE payment_id = $1 FOR UPDATE`, paymentID).Scan(
		&r.PaymentID, &r.MerchantID, &r.GrossCents, &r.BaseCents, &r.IVACents, &r.ISRCents, &r.ISRRateBps,
		&r.TaxEscrowCents, &r.PlatformFeeBps, &r.PlatformFeeCents, &r.RBFSweepCents, &r.NetCents,
		&r.PersonaMoral, &r.YearMonth, &r.JournalID, &r.RefundedGrossCents, &r.RefundedBaseCents,
		&r.RefundedTaxEscrowCents, &r.RefundedFeeCents, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SettlementRecord{}, false, nil
		}
		return SettlementRecord{}, false, fmt.Errorf("failed to load settlement: %w", err)
	}
	return r, true, nil
}

func (t *sqlMoneyTx) InsertSettlement(ctx context.Context, r SettlementRecord) error {
	now := time.Now().UTC()
	res, err := t.tx.ExecContext(ctx, `INSERT INTO payment_settlements (`+settlementColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $20)
		ON CONFLICT (payment_id) DO NOTHING`,
		r.PaymentID, r.MerchantID, r.GrossCents, r.BaseCents, r.IVACents, r.ISRCents, r.ISRRateBps,
		r.TaxEscrowCents, r.PlatformFeeBps, r.PlatformFeeCents, r.RBFSweepCents, r.NetCents,
		r.PersonaMoral, r.YearMonth, r.JournalID, r.RefundedGrossCents, r.RefundedBaseCents,
		r.RefundedTaxEscrowCents, r.RefundedFeeCents, now)
	if err != nil {
		return fmt.Errorf("failed to insert settlement: %w", err)
	}
	return requireRowAffected(res, ErrDuplicateSettlement)
}

func (t *sqlMoneyTx) UpdateSettlementRefunds(ctx context.Context, r SettlementRecord) error {
	res, err := t.tx.ExecContext(ctx, `
		UPDATE payment_settlements
		SET refunded_gross_cents = $1, refunded_base_cents = $2, refunded_tax_escrow_cents = $3,
		    refunded_fee_cents = $4, updated_at = $5
		WHERE payment_id = $6
	`, r.RefundedGrossCents, r.RefundedBaseCents, r.RefundedTaxEscrowCents, r.RefundedFeeCents, time.Now().UTC(), r.PaymentID)
	if err != nil {
		return fmt.Errorf("failed to update settlement refunds: %w", err)
	}
	return requireRowAffected(res, ErrSettlementNotFound)
}

func (t *sqlMoneyTx) ClaimOperation(ctx context.Context, key string) (bool, int, []byte, error) {
	res, err := t.tx.ExecContext(ctx, `
		INSERT INTO money_operations (op_key, status_code, created_at, updated_at)
		VALUES ($1, 0, $2, $2)
		ON CONFLICT (op_key) DO NOTHING
	`, key, time.Now().UTC())
	if err != nil {
		return false, 0, nil, fmt.Errorf("failed to claim operation key: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 1 {
		return true, 0, nil, nil
	}
	var status int
	var body []byte
	if err := t.tx.QueryRowContext(ctx, `SELECT status_code, response_body FROM money_operations WHERE op_key = $1`, key).Scan(&status, &body); err != nil {
		return false, 0, nil, fmt.Errorf("failed to read claimed operation: %w", err)
	}
	return false, status, body, nil
}

func (t *sqlMoneyTx) CompleteOperation(ctx context.Context, key string, statusCode int, response []byte) error {
	_, err := t.tx.ExecContext(ctx, `
		UPDATE money_operations SET status_code = $1, response_body = $2, updated_at = $3 WHERE op_key = $4
	`, statusCode, response, time.Now().UTC(), key)
	if err != nil {
		return fmt.Errorf("failed to complete operation: %w", err)
	}
	return nil
}

func (t *sqlMoneyTx) LockAccountBalances(ctx context.Context, accountCodes ...string) (map[string]int64, error) {
	codes := append([]string(nil), accountCodes...)
	sort.Strings(codes)
	out := make(map[string]int64, len(codes))
	for _, code := range codes {
		if _, done := out[code]; done {
			continue
		}
		var bal int64
		err := t.tx.QueryRowContext(ctx, `
			SELECT balance_cents FROM ledger_accounts WHERE id = $1 OR code = $1 FOR UPDATE
		`, code).Scan(&bal)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, ErrAccountNotFound
			}
			return nil, fmt.Errorf("failed to lock account %s: %w", code, err)
		}
		out[code] = bal
	}
	return out, nil
}

func (t *sqlMoneyTx) PostJournal(ctx context.Context, journal *ledger.JournalEntry) error {
	return postJournalTx(ctx, t.tx, journal)
}

func (t *sqlMoneyTx) MonthlySalesForUpdate(ctx context.Context, merchantID, yearMonth string) (int64, error) {
	// Materialize the row first so FOR UPDATE always has something to lock
	// (two first sales of the month would otherwise both read 0).
	if _, err := t.tx.ExecContext(ctx, `
		INSERT INTO merchant_monthly_sales (merchant_id, year_month, accumulated_cents, updated_at)
		VALUES ($1, $2, 0, $3)
		ON CONFLICT (merchant_id, year_month) DO NOTHING
	`, merchantID, yearMonth, time.Now().UTC()); err != nil {
		if isForeignKeyViolation(err) {
			return 0, ErrMerchantNotFound
		}
		return 0, fmt.Errorf("failed to init monthly sales: %w", err)
	}
	var cents int64
	if err := t.tx.QueryRowContext(ctx, `
		SELECT accumulated_cents FROM merchant_monthly_sales
		WHERE merchant_id = $1 AND year_month = $2
		FOR UPDATE
	`, merchantID, yearMonth).Scan(&cents); err != nil {
		return 0, fmt.Errorf("failed to lock monthly sales: %w", err)
	}
	return cents, nil
}

func (t *sqlMoneyTx) AddMonthlySales(ctx context.Context, merchantID, yearMonth string, delta int64) error {
	if delta == 0 {
		return nil
	}
	_, err := t.tx.ExecContext(ctx, `
		INSERT INTO merchant_monthly_sales (merchant_id, year_month, accumulated_cents, updated_at)
		VALUES ($1, $2, GREATEST($3::BIGINT, 0), $4)
		ON CONFLICT (merchant_id, year_month) DO UPDATE
		SET accumulated_cents = GREATEST(merchant_monthly_sales.accumulated_cents + $3::BIGINT, 0),
		    updated_at = EXCLUDED.updated_at
	`, merchantID, yearMonth, delta, time.Now().UTC())
	if err != nil {
		if isForeignKeyViolation(err) {
			return ErrMerchantNotFound
		}
		return fmt.Errorf("failed to add monthly sales: %w", err)
	}
	return nil
}

func (t *sqlMoneyTx) EnqueueEvent(ctx context.Context, eventType, aggregateID string, payload any) error {
	return t.outbox.SaveEventTx(ctx, t.tx, eventType, aggregateID, payload)
}
