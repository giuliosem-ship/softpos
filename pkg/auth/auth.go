// Package auth implements merchant-user identity for Kerno: users identified by
// phone, user<->merchant memberships with a role, OTP login, short-lived HS256
// access JWTs and opaque rotating refresh tokens with reuse detection.
//
// Persistence goes through Repository (SQL implementation in pkg/store,
// in-memory implementation in this package). Secrets are never stored in
// clear: OTP codes are HMAC-hashed with a server pepper and refresh tokens are
// stored as SHA-256 digests.
package auth

import (
	"context"
	"errors"
	"time"
)

// Role is a user's role inside one merchant.
type Role string

const (
	RoleOwner    Role = "owner"    // merchant owner: full access incl. payouts
	RoleCashier  Role = "cashier"  // can charge, cannot move money out
	RoleOperator Role = "operator" // internal Kerno operator (backoffice)
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool {
	switch r {
	case RoleOwner, RoleCashier, RoleOperator:
		return true
	}
	return false
}

var (
	ErrInvalidPhone        = errors.New("auth: phone must be E.164 (e.g. +525512345678)")
	ErrOTPRateLimited      = errors.New("auth: OTP requested too recently")
	ErrOTPInvalid          = errors.New("auth: invalid or expired OTP")
	ErrOTPLocked           = errors.New("auth: too many OTP attempts")
	ErrOTPDelivery         = errors.New("auth: OTP delivery failed")
	ErrRefreshTokenInvalid = errors.New("auth: invalid or expired refresh token")
	ErrRefreshTokenReused  = errors.New("auth: refresh token reuse detected; session family revoked")
	ErrNotMember           = errors.New("auth: user is not a member of the merchant")
	ErrUserNotFound        = errors.New("auth: user not found")
	ErrConflict            = errors.New("auth: record already exists")
	ErrWeakSecret          = errors.New("auth: JWT secret must be at least 32 bytes")
)

// User is a person that can log in (identified by phone).
type User struct {
	ID        string    `json:"id"`
	Phone     string    `json:"phone"`
	CreatedAt time.Time `json:"created_at"`
}

// Membership links a user to a merchant with a role.
type Membership struct {
	UserID     string    `json:"user_id"`
	MerchantID string    `json:"merchant_id"`
	Role       Role      `json:"role"`
	CreatedAt  time.Time `json:"created_at"`
}

// OTPChallenge is the pending one-time code for a phone (one per phone).
type OTPChallenge struct {
	Phone      string
	CodeHash   string // hex HMAC-SHA256(pepper, phone ":" code)
	ExpiresAt  time.Time
	Attempts   int
	LastSentAt time.Time
}

// RefreshToken is the stored (hashed) form of an opaque refresh token.
type RefreshToken struct {
	ID         string
	FamilyID   string // all rotations of one login share a family
	UserID     string
	MerchantID string // may be empty (user without merchant yet)
	TokenHash  string // hex SHA-256 of the opaque token
	ExpiresAt  time.Time
	RevokedAt  *time.Time
	ReplacedBy string
	CreatedAt  time.Time
}

// Repository is the persistence contract for auth state.
type Repository interface {
	CreateUser(ctx context.Context, u User) error // ErrConflict if phone/id taken
	GetUser(ctx context.Context, id string) (User, bool, error)
	GetUserByPhone(ctx context.Context, phone string) (User, bool, error)

	// AddMembership inserts or updates (role) the user<->merchant link.
	AddMembership(ctx context.Context, m Membership) error
	GetMembership(ctx context.Context, userID, merchantID string) (Membership, bool, error)
	ListMemberships(ctx context.Context, userID string) ([]Membership, error)

	// SaveOTPChallenge upserts the challenge for c.Phone (attempts reset).
	SaveOTPChallenge(ctx context.Context, c OTPChallenge) error
	GetOTPChallenge(ctx context.Context, phone string) (OTPChallenge, bool, error)
	// IncrementOTPAttempts atomically adds one attempt and returns the new count
	// (found=false when no challenge exists).
	IncrementOTPAttempts(ctx context.Context, phone string) (attempts int, found bool, err error)
	// DeleteOTPChallenge removes the challenge; deleted=false if it was already gone
	// (lets exactly one concurrent verifier win).
	DeleteOTPChallenge(ctx context.Context, phone string) (deleted bool, err error)

	CreateRefreshToken(ctx context.Context, t RefreshToken) error
	GetRefreshTokenByHash(ctx context.Context, tokenHash string) (RefreshToken, bool, error)
	// RotateRefreshToken atomically revokes oldID (replaced_by=next.ID) and inserts
	// next. Returns ErrRefreshTokenReused if oldID was already revoked.
	RotateRefreshToken(ctx context.Context, oldID string, next RefreshToken, now time.Time) error
	RevokeRefreshFamily(ctx context.Context, familyID string, now time.Time) error
}
