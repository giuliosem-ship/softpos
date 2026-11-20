package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PayoutStatus is the lifecycle state of a persisted payout (SPEI disbursement).
type PayoutStatus string

const (
	// PayoutReserved: funds debited in the same transaction that created the
	// row; registraOrden is in flight or was interrupted (crash).
	PayoutReserved PayoutStatus = "RESERVED"
	// PayoutDispatched: STP accepted the order (awaiting CEP).
	PayoutDispatched PayoutStatus = "DISPATCHED"
	// PayoutPendingReconciliation: registraOrden outcome unknown (timeout,
	// reset, 5xx); funds stay reserved until the reconciler resolves it.
	PayoutPendingReconciliation PayoutStatus = "PENDING_RECONCILIATION"
	// PayoutSettled: STP confirmed the order was liquidated (recovery path).
	PayoutSettled PayoutStatus = "SETTLED"
	// PayoutFailed: definitively rejected; the reservation was compensated.
	PayoutFailed PayoutStatus = "FAILED"
)

// ErrPayoutStateConflict is returned when a payout is not in the expected
// state (another worker already finalized it).
var ErrPayoutStateConflict = errors.New("payout is not in the expected state")

// PayoutRecord is one persisted payout. (MerchantID, IdempotencyKey) is
// unique; RequestHash fingerprints the request body that created it.
type PayoutRecord struct {
	ClaveRastreo          string
	MerchantID            string
	IdempotencyKey        string
	RequestHash           string
	AmountCents           int64
	DestinationCLABE      string
	BeneficiaryName       string
	BeneficiaryRFC        string
	Concept               string
	FechaOperacion        string
	Status                PayoutStatus
	STPOrderID            int64
	RemainingBalanceCents int64
	LastError             string
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// PayoutRepository lists payouts outside a unit of work (reconciler scans).
type PayoutRepository interface {
	// ListPayoutsForRecovery returns RESERVED and PENDING_RECONCILIATION
	// payouts last updated at or before olderThan, oldest first.
	ListPayoutsForRecovery(ctx context.Context, olderThan time.Time, limit int) ([]PayoutRecord, error)
}

const payoutColumns = `clave_rastreo, merchant_id, idempotency_key, request_hash, amount_cents,
	destination_clabe, beneficiary_name, beneficiary_rfc, concept, fecha_operacion, status,
	stp_order_id, remaining_balance_cents, last_error, created_at, updated_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanPayout(row rowScanner) (PayoutRecord, error) {
	var p PayoutRecord
	var status string
	err := row.Scan(&p.ClaveRastreo, &p.MerchantID, &p.IdempotencyKey, &p.RequestHash, &p.AmountCents,
		&p.DestinationCLABE, &p.BeneficiaryName, &p.BeneficiaryRFC, &p.Concept, &p.FechaOperacion, &status,
		&p.STPOrderID, &p.RemainingBalanceCents, &p.LastError, &p.CreatedAt, &p.UpdatedAt)
	p.Status = PayoutStatus(status)
	return p, err
}

func (t *sqlMoneyTx) CreatePayout(ctx context.Context, p PayoutRecord) (PayoutRecord, bool, error) {
	now := time.Now().UTC()
	if p.Status == "" {
		p.Status = PayoutReserved
	}
	res, err := t.tx.ExecContext(ctx, `
		INSERT INTO payouts (`+payoutColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $15)
		ON CONFLICT (merchant_id, idempotency_key) DO NOTHING
	`, p.ClaveRastreo, p.MerchantID, p.IdempotencyKey, p.RequestHash, p.AmountCents,
		p.DestinationCLABE, p.BeneficiaryName, p.BeneficiaryRFC, p.Concept, p.FechaOperacion, string(p.Status),
		p.STPOrderID, p.RemainingBalanceCents, p.LastError, now)
	if err != nil {
		return PayoutRecord{}, false, fmt.Errorf("failed to insert payout: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 1 {
		p.CreatedAt, p.UpdatedAt = now, now
		return p, true, nil
	}
	// The key exists (a concurrent duplicate waited for the first commit).
	existing, err := scanPayout(t.tx.QueryRowContext(ctx, `SELECT `+payoutColumns+`
		FROM payouts WHERE merchant_id = $1 AND idempotency_key = $2 FOR UPDATE`, p.MerchantID, p.IdempotencyKey))
	if err != nil {
		return PayoutRecord{}, false, fmt.Errorf("failed to load existing payout: %w", err)
	}
	return existing, false, nil
}

func (t *sqlMoneyTx) SetPayoutRemainingBalance(ctx context.Context, claveRastreo string, cents int64) error {
	res, err := t.tx.ExecContext(ctx, `UPDATE payouts SET remaining_balance_cents = $1 WHERE clave_rastreo = $2`, cents, claveRastreo)
	if err != nil {
		return fmt.Errorf("failed to update payout balance: %w", err)
	}
	return requireRowAffected(res, ErrPayoutStateConflict)
}

func (t *sqlMoneyTx) GetPayoutForUpdate(ctx context.Context, claveRastreo string) (PayoutRecord, bool, error) {
	p, err := scanPayout(t.tx.QueryRowContext(ctx, `SELECT `+payoutColumns+` FROM payouts WHERE clave_rastreo = $1 FOR UPDATE`, claveRastreo))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PayoutRecord{}, false, nil
		}
		return PayoutRecord{}, false, fmt.Errorf("failed to load payout: %w", err)
	}
	return p, true, nil
}

func (t *sqlMoneyTx) SetPayoutStatus(ctx context.Context, claveRastreo string, from, to PayoutStatus, stpOrderID int64, lastError string) error {
	res, err := t.tx.ExecContext(ctx, `
		UPDATE payouts
		SET status = $1, stp_order_id = CASE WHEN $2::BIGINT <> 0 THEN $2::BIGINT ELSE stp_order_id END,
		    last_error = $3, updated_at = $4
		WHERE clave_rastreo = $5 AND status = $6
	`, string(to), stpOrderID, lastError, time.Now().UTC(), claveRastreo, string(from))
	if err != nil {
		return fmt.Errorf("failed to update payout status: %w", err)
	}
	return requireRowAffected(res, ErrPayoutStateConflict)
}

// SQLPayoutRepository reads payouts outside a unit of work.
type SQLPayoutRepository struct {
	db *sql.DB
}

var _ PayoutRepository = (*SQLPayoutRepository)(nil)

func NewSQLPayoutRepository(db *sql.DB) *SQLPayoutRepository {
	return &SQLPayoutRepository{db: db}
}

func (r *SQLPayoutRepository) ListPayoutsForRecovery(ctx context.Context, olderThan time.Time, limit int) ([]PayoutRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+payoutColumns+` FROM payouts
		WHERE status IN ('RESERVED', 'PENDING_RECONCILIATION') AND updated_at <= $1
		ORDER BY updated_at ASC, clave_rastreo ASC
		LIMIT $2`, olderThan.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("failed to list payouts for recovery: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PayoutRecord
	for rows.Next() {
		p, err := scanPayout(rows)
		if err != nil {
			return nil, fmt.Errorf("failed to scan payout: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
