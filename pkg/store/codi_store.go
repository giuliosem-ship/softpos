package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"softpos/pkg/payments"
)

// SQLCoDiRepository persists CoDi QR requests and confirmations on PostgreSQL.
type SQLCoDiRepository struct {
	db *sql.DB
}

var _ payments.CoDiRepository = (*SQLCoDiRepository)(nil)

func NewSQLCoDiRepository(db *sql.DB) *SQLCoDiRepository {
	return &SQLCoDiRepository{db: db}
}

// SaveRequest upserts a CoDi request keyed by its reference (same semantics as
// the in-memory service: a new request with the same reference replaces it).
func (r *SQLCoDiRepository) SaveRequest(ctx context.Context, req payments.CoDiRequest) error {
	now := time.Now().UTC()
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO codi_requests (reference, seller_clabe, amount_cents, concept, institution_code, version, expires_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)
		ON CONFLICT (reference) DO UPDATE SET
			seller_clabe = EXCLUDED.seller_clabe,
			amount_cents = EXCLUDED.amount_cents,
			concept = EXCLUDED.concept,
			institution_code = EXCLUDED.institution_code,
			version = EXCLUDED.version,
			expires_at = EXCLUDED.expires_at,
			updated_at = EXCLUDED.updated_at
	`, req.Reference, req.SellerCLABE, req.AmountCents, req.Concept, req.InstitutionCode, req.Version, req.Expiration.UTC(), now)
	if err != nil {
		return fmt.Errorf("failed to save CoDi request: %w", err)
	}
	return nil
}

func (r *SQLCoDiRepository) GetRequest(ctx context.Context, reference string) (payments.CoDiRequest, bool, error) {
	var req payments.CoDiRequest
	err := r.db.QueryRowContext(ctx, `
		SELECT reference, seller_clabe, amount_cents, concept, institution_code, version, expires_at
		FROM codi_requests
		WHERE reference = $1
	`, reference).Scan(&req.Reference, &req.SellerCLABE, &req.AmountCents, &req.Concept, &req.InstitutionCode, &req.Version, &req.Expiration)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return payments.CoDiRequest{}, false, nil
		}
		return payments.CoDiRequest{}, false, fmt.Errorf("failed to get CoDi request: %w", err)
	}
	return req, true, nil
}

// SaveConfirmation upserts a confirmation keyed by tracking key.
func (r *SQLCoDiRepository) SaveConfirmation(ctx context.Context, conf payments.CoDiConfirmation) error {
	now := time.Now().UTC()
	var confirmedAt any
	if !conf.Timestamp.IsZero() {
		confirmedAt = conf.Timestamp.UTC()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO codi_confirmations (tracking_key, seller_clabe, buyer_clabe, amount_cents, status, banxico_approval_code, confirmed_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)
		ON CONFLICT (tracking_key) DO UPDATE SET
			seller_clabe = EXCLUDED.seller_clabe,
			buyer_clabe = EXCLUDED.buyer_clabe,
			amount_cents = EXCLUDED.amount_cents,
			status = EXCLUDED.status,
			banxico_approval_code = EXCLUDED.banxico_approval_code,
			confirmed_at = EXCLUDED.confirmed_at,
			updated_at = EXCLUDED.updated_at
	`, conf.TrackingKey, conf.SellerCLABE, conf.BuyerCLABE, conf.AmountCents, conf.Status, conf.BanxicoApprovalCode, confirmedAt, now)
	if err != nil {
		return fmt.Errorf("failed to save CoDi confirmation: %w", err)
	}
	return nil
}

func (r *SQLCoDiRepository) GetConfirmation(ctx context.Context, trackingKey string) (payments.CoDiConfirmation, bool, error) {
	var conf payments.CoDiConfirmation
	var confirmedAt sql.NullTime
	err := r.db.QueryRowContext(ctx, `
		SELECT tracking_key, seller_clabe, buyer_clabe, amount_cents, status, banxico_approval_code, confirmed_at
		FROM codi_confirmations
		WHERE tracking_key = $1
	`, trackingKey).Scan(&conf.TrackingKey, &conf.SellerCLABE, &conf.BuyerCLABE, &conf.AmountCents, &conf.Status, &conf.BanxicoApprovalCode, &confirmedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return payments.CoDiConfirmation{}, false, nil
		}
		return payments.CoDiConfirmation{}, false, fmt.Errorf("failed to get CoDi confirmation: %w", err)
	}
	if confirmedAt.Valid {
		conf.Timestamp = confirmedAt.Time
	}
	return conf, true, nil
}
