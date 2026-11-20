package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"softpos/pkg/auth"
)

// SQLAuthRepository is the PostgreSQL auth.Repository (migration 005).
type SQLAuthRepository struct {
	db *sql.DB
}

var _ auth.Repository = (*SQLAuthRepository)(nil)

func NewSQLAuthRepository(db *sql.DB) *SQLAuthRepository {
	return &SQLAuthRepository{db: db}
}

func (r *SQLAuthRepository) CreateUser(ctx context.Context, u auth.User) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO users (id, phone, created_at) VALUES ($1, $2, $3)`, u.ID, u.Phone, u.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return auth.ErrConflict
		}
		return fmt.Errorf("create user: %w", err)
	}
	return nil
}

func (r *SQLAuthRepository) getUser(ctx context.Context, where string, arg string) (auth.User, bool, error) {
	var u auth.User
	err := r.db.QueryRowContext(ctx, `SELECT id, phone, created_at FROM users WHERE `+where+` = $1`, arg).Scan(&u.ID, &u.Phone, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.User{}, false, nil
	}
	if err != nil {
		return auth.User{}, false, fmt.Errorf("get user: %w", err)
	}
	return u, true, nil
}

func (r *SQLAuthRepository) GetUser(ctx context.Context, id string) (auth.User, bool, error) {
	return r.getUser(ctx, "id", id)
}

func (r *SQLAuthRepository) GetUserByPhone(ctx context.Context, phone string) (auth.User, bool, error) {
	return r.getUser(ctx, "phone", phone)
}

func (r *SQLAuthRepository) AddMembership(ctx context.Context, m auth.Membership) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO merchant_memberships (user_id, merchant_id, role, created_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, merchant_id) DO UPDATE SET role = EXCLUDED.role`,
		m.UserID, m.MerchantID, string(m.Role), m.CreatedAt)
	if err != nil {
		return fmt.Errorf("add membership: %w", err)
	}
	return nil
}

func (r *SQLAuthRepository) GetMembership(ctx context.Context, userID, merchantID string) (auth.Membership, bool, error) {
	var m auth.Membership
	var role string
	err := r.db.QueryRowContext(ctx, `SELECT user_id, merchant_id, role, created_at FROM merchant_memberships WHERE user_id = $1 AND merchant_id = $2`,
		userID, merchantID).Scan(&m.UserID, &m.MerchantID, &role, &m.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.Membership{}, false, nil
	}
	if err != nil {
		return auth.Membership{}, false, fmt.Errorf("get membership: %w", err)
	}
	m.Role = auth.Role(role)
	return m, true, nil
}

func (r *SQLAuthRepository) ListMemberships(ctx context.Context, userID string) ([]auth.Membership, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT user_id, merchant_id, role, created_at FROM merchant_memberships WHERE user_id = $1 ORDER BY merchant_id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list memberships: %w", err)
	}
	defer rows.Close()
	out := []auth.Membership{}
	for rows.Next() {
		var m auth.Membership
		var role string
		if err := rows.Scan(&m.UserID, &m.MerchantID, &role, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.Role = auth.Role(role)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (r *SQLAuthRepository) SaveOTPChallenge(ctx context.Context, c auth.OTPChallenge) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO otp_challenges (phone, code_hash, expires_at, attempts, last_sent_at) VALUES ($1, $2, $3, 0, $4)
		ON CONFLICT (phone) DO UPDATE SET code_hash = EXCLUDED.code_hash, expires_at = EXCLUDED.expires_at,
			attempts = 0, last_sent_at = EXCLUDED.last_sent_at`,
		c.Phone, c.CodeHash, c.ExpiresAt, c.LastSentAt)
	if err != nil {
		return fmt.Errorf("save otp challenge: %w", err)
	}
	return nil
}

func (r *SQLAuthRepository) GetOTPChallenge(ctx context.Context, phone string) (auth.OTPChallenge, bool, error) {
	var c auth.OTPChallenge
	err := r.db.QueryRowContext(ctx, `SELECT phone, code_hash, expires_at, attempts, last_sent_at FROM otp_challenges WHERE phone = $1`, phone).
		Scan(&c.Phone, &c.CodeHash, &c.ExpiresAt, &c.Attempts, &c.LastSentAt)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.OTPChallenge{}, false, nil
	}
	if err != nil {
		return auth.OTPChallenge{}, false, fmt.Errorf("get otp challenge: %w", err)
	}
	return c, true, nil
}

func (r *SQLAuthRepository) IncrementOTPAttempts(ctx context.Context, phone string) (int, bool, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `UPDATE otp_challenges SET attempts = attempts + 1 WHERE phone = $1 RETURNING attempts`, phone).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("increment otp attempts: %w", err)
	}
	return n, true, nil
}

func (r *SQLAuthRepository) DeleteOTPChallenge(ctx context.Context, phone string) (bool, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM otp_challenges WHERE phone = $1`, phone)
	if err != nil {
		return false, fmt.Errorf("delete otp challenge: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func insertRefreshToken(ctx context.Context, ex interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, t auth.RefreshToken) error {
	_, err := ex.ExecContext(ctx, `
		INSERT INTO refresh_tokens (id, family_id, user_id, merchant_id, token_hash, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		t.ID, t.FamilyID, t.UserID, nullString(t.MerchantID), t.TokenHash, t.ExpiresAt, t.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return auth.ErrConflict
		}
		return fmt.Errorf("insert refresh token: %w", err)
	}
	return nil
}

func (r *SQLAuthRepository) CreateRefreshToken(ctx context.Context, t auth.RefreshToken) error {
	return insertRefreshToken(ctx, r.db, t)
}

func (r *SQLAuthRepository) GetRefreshTokenByHash(ctx context.Context, hash string) (auth.RefreshToken, bool, error) {
	var t auth.RefreshToken
	var merchantID, replacedBy sql.NullString
	var revokedAt sql.NullTime
	err := r.db.QueryRowContext(ctx, `
		SELECT id, family_id, user_id, merchant_id, token_hash, expires_at, revoked_at, replaced_by, created_at
		FROM refresh_tokens WHERE token_hash = $1`, hash).
		Scan(&t.ID, &t.FamilyID, &t.UserID, &merchantID, &t.TokenHash, &t.ExpiresAt, &revokedAt, &replacedBy, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.RefreshToken{}, false, nil
	}
	if err != nil {
		return auth.RefreshToken{}, false, fmt.Errorf("get refresh token: %w", err)
	}
	t.MerchantID = merchantID.String
	t.ReplacedBy = replacedBy.String
	if revokedAt.Valid {
		ts := revokedAt.Time
		t.RevokedAt = &ts
	}
	return t, true, nil
}

// RotateRefreshToken revokes oldID only if it is still active (conditional
// UPDATE, so exactly one concurrent rotation wins) and inserts next in the same
// transaction.
func (r *SQLAuthRepository) RotateRefreshToken(ctx context.Context, oldID string, next auth.RefreshToken, now time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin rotate: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE refresh_tokens SET revoked_at = $1, replaced_by = $2 WHERE id = $3 AND revoked_at IS NULL`, now, next.ID, oldID)
	if err != nil {
		return fmt.Errorf("revoke rotated token: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM refresh_tokens WHERE id = $1)`, oldID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return auth.ErrRefreshTokenInvalid
		}
		return auth.ErrRefreshTokenReused
	}
	if err := insertRefreshToken(ctx, tx, next); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *SQLAuthRepository) RevokeRefreshFamily(ctx context.Context, familyID string, now time.Time) error {
	if _, err := r.db.ExecContext(ctx, `UPDATE refresh_tokens SET revoked_at = $1 WHERE family_id = $2 AND revoked_at IS NULL`, now, familyID); err != nil {
		return fmt.Errorf("revoke refresh family: %w", err)
	}
	return nil
}

func (r *SQLAuthRepository) SaveAttestationNonce(ctx context.Context, n auth.AttestationNonce) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO attestation_nonces (nonce_hash, expires_at, created_at) VALUES ($1, $2, $3)`,
		n.NonceHash, n.ExpiresAt, n.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return auth.ErrConflict
		}
		return fmt.Errorf("save attestation nonce: %w", err)
	}
	return nil
}

// ConsumeAttestationNonce is a single conditional UPDATE, so a nonce can be
// consumed exactly once even under concurrency.
func (r *SQLAuthRepository) ConsumeAttestationNonce(ctx context.Context, hash string, now time.Time) (bool, error) {
	res, err := r.db.ExecContext(ctx, `UPDATE attestation_nonces SET used_at = $1 WHERE nonce_hash = $2 AND used_at IS NULL AND expires_at > $1`, now, hash)
	if err != nil {
		return false, fmt.Errorf("consume attestation nonce: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
