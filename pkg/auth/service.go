package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"time"
)

// Config tunes token lifetimes and OTP policy. Zero values take the defaults.
type Config struct {
	JWTSecret         []byte        // required, >= 32 bytes
	Issuer            string        // default "kerno"
	AccessTTL         time.Duration // default 15m
	RefreshTTL        time.Duration // default 30 days
	OTPTTL            time.Duration // default 5m
	OTPResendInterval time.Duration // default 60s
	OTPMaxAttempts    int           // default 5
	Now               func() time.Time
}

const (
	DefaultAccessTTL         = 15 * time.Minute
	DefaultRefreshTTL        = 30 * 24 * time.Hour
	DefaultOTPTTL            = 5 * time.Minute
	DefaultOTPResendInterval = 60 * time.Second
	DefaultOTPMaxAttempts    = 5
	otpDigits                = 6
)

var e164 = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)

// ValidPhone reports whether phone is in E.164 form.
func ValidPhone(phone string) bool { return e164.MatchString(phone) }

// TokenPair is returned by login and refresh.
type TokenPair struct {
	AccessToken      string       `json:"access_token"`
	TokenType        string       `json:"token_type"`
	ExpiresIn        int64        `json:"expires_in"`
	RefreshToken     string       `json:"refresh_token"`
	RefreshExpiresIn int64        `json:"refresh_expires_in"`
	UserID           string       `json:"user_id"`
	MerchantID       string       `json:"merchant_id,omitempty"`
	Role             Role         `json:"role,omitempty"`
	Memberships      []Membership `json:"memberships"`
}

// Service implements OTP login and token issuance.
type Service struct {
	repo      Repository
	sender    OTPSender
	cfg       Config
	otpPepper []byte
}

// NewService validates cfg and builds the service.
func NewService(repo Repository, sender OTPSender, cfg Config) (*Service, error) {
	if len(cfg.JWTSecret) < 32 {
		return nil, ErrWeakSecret
	}
	if repo == nil || sender == nil {
		return nil, errors.New("auth: repository and OTP sender are required")
	}
	if cfg.Issuer == "" {
		cfg.Issuer = "kerno"
	}
	if cfg.AccessTTL <= 0 {
		cfg.AccessTTL = DefaultAccessTTL
	}
	if cfg.RefreshTTL <= 0 {
		cfg.RefreshTTL = DefaultRefreshTTL
	}
	if cfg.OTPTTL <= 0 {
		cfg.OTPTTL = DefaultOTPTTL
	}
	if cfg.OTPResendInterval <= 0 {
		cfg.OTPResendInterval = DefaultOTPResendInterval
	}
	if cfg.OTPMaxAttempts <= 0 {
		cfg.OTPMaxAttempts = DefaultOTPMaxAttempts
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	secret := append([]byte(nil), cfg.JWTSecret...)
	cfg.JWTSecret = secret
	// The OTP pepper is derived from (not equal to) the JWT secret.
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("kerno-otp-pepper-v1"))
	return &Service{repo: repo, sender: sender, cfg: cfg, otpPepper: m.Sum(nil)}, nil
}

// Repository exposes the underlying repository (membership management).
func (s *Service) Repository() Repository { return s.repo }

// AccessTTL returns the configured access token lifetime.
func (s *Service) AccessTTL() time.Duration { return s.cfg.AccessTTL }

func (s *Service) now() time.Time { return s.cfg.Now().UTC() }

func randomID(prefix string, n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}

func (s *Service) hashOTP(phone, code string) string {
	m := hmac.New(sha256.New, s.otpPepper)
	m.Write([]byte(phone + ":" + code))
	return hex.EncodeToString(m.Sum(nil))
}

// HashToken returns the hex SHA-256 used to store opaque tokens.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// RequestOTP generates a fresh code for phone, stores its hash and sends it.
// It returns ErrOTPRateLimited (with the wait) if the previous code was sent
// less than OTPResendInterval ago.
func (s *Service) RequestOTP(ctx context.Context, phone string) (retryAfter time.Duration, err error) {
	if !ValidPhone(phone) {
		return 0, ErrInvalidPhone
	}
	now := s.now()
	if prev, found, err := s.repo.GetOTPChallenge(ctx, phone); err != nil {
		return 0, err
	} else if found {
		if wait := prev.LastSentAt.Add(s.cfg.OTPResendInterval).Sub(now); wait > 0 {
			return wait, ErrOTPRateLimited
		}
	}
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return 0, err
	}
	code := fmt.Sprintf("%0*d", otpDigits, n.Int64())
	if err := s.repo.SaveOTPChallenge(ctx, OTPChallenge{
		Phone:      phone,
		CodeHash:   s.hashOTP(phone, code),
		ExpiresAt:  now.Add(s.cfg.OTPTTL),
		LastSentAt: now,
	}); err != nil {
		return 0, err
	}
	if err := s.sender.SendOTP(ctx, phone, code); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrOTPDelivery, err)
	}
	return 0, nil
}

// VerifyOTP checks the code and, on success, logs the user in (creating the
// user on first login). merchantID optionally selects the tenant; when empty
// and the user has exactly one membership, that one is used.
func (s *Service) VerifyOTP(ctx context.Context, phone, code, merchantID string) (TokenPair, error) {
	if !ValidPhone(phone) {
		return TokenPair{}, ErrInvalidPhone
	}
	now := s.now()
	ch, found, err := s.repo.GetOTPChallenge(ctx, phone)
	if err != nil {
		return TokenPair{}, err
	}
	if !found || !now.Before(ch.ExpiresAt) {
		return TokenPair{}, ErrOTPInvalid
	}
	// Count the attempt before comparing so concurrent guesses are bounded.
	attempts, found, err := s.repo.IncrementOTPAttempts(ctx, phone)
	if err != nil {
		return TokenPair{}, err
	}
	if !found {
		return TokenPair{}, ErrOTPInvalid
	}
	if attempts > s.cfg.OTPMaxAttempts {
		_, _ = s.repo.DeleteOTPChallenge(ctx, phone)
		return TokenPair{}, ErrOTPLocked
	}
	if !hmac.Equal([]byte(s.hashOTP(phone, code)), []byte(ch.CodeHash)) {
		return TokenPair{}, ErrOTPInvalid
	}
	deleted, err := s.repo.DeleteOTPChallenge(ctx, phone)
	if err != nil {
		return TokenPair{}, err
	}
	if !deleted { // another request consumed it first
		return TokenPair{}, ErrOTPInvalid
	}

	user, err := s.getOrCreateUser(ctx, phone, now)
	if err != nil {
		return TokenPair{}, err
	}
	memberships, err := s.repo.ListMemberships(ctx, user.ID)
	if err != nil {
		return TokenPair{}, err
	}
	var scope *Membership
	if merchantID != "" {
		for i := range memberships {
			if memberships[i].MerchantID == merchantID {
				scope = &memberships[i]
			}
		}
		if scope == nil {
			return TokenPair{}, ErrNotMember
		}
	} else if len(memberships) == 1 {
		scope = &memberships[0]
	}
	familyID, err := randomID("fam_", 16)
	if err != nil {
		return TokenPair{}, err
	}
	return s.issue(ctx, user.ID, scope, memberships, familyID, "", now)
}

func (s *Service) getOrCreateUser(ctx context.Context, phone string, now time.Time) (User, error) {
	if u, found, err := s.repo.GetUserByPhone(ctx, phone); err != nil || found {
		return u, err
	}
	id, err := randomID("usr_", 12)
	if err != nil {
		return User{}, err
	}
	u := User{ID: id, Phone: phone, CreatedAt: now}
	if err := s.repo.CreateUser(ctx, u); err != nil {
		if errors.Is(err, ErrConflict) { // concurrent first login
			if existing, found, gErr := s.repo.GetUserByPhone(ctx, phone); gErr == nil && found {
				return existing, nil
			}
		}
		return User{}, err
	}
	return u, nil
}

// Refresh rotates a refresh token. Presenting an already-rotated (revoked)
// token revokes the whole family and returns ErrRefreshTokenReused.
// merchantID optionally re-scopes the session to another membership.
func (s *Service) Refresh(ctx context.Context, refreshToken, merchantID string) (TokenPair, error) {
	if refreshToken == "" {
		return TokenPair{}, ErrRefreshTokenInvalid
	}
	now := s.now()
	old, found, err := s.repo.GetRefreshTokenByHash(ctx, HashToken(refreshToken))
	if err != nil {
		return TokenPair{}, err
	}
	if !found {
		return TokenPair{}, ErrRefreshTokenInvalid
	}
	if old.RevokedAt != nil {
		_ = s.repo.RevokeRefreshFamily(ctx, old.FamilyID, now)
		return TokenPair{}, ErrRefreshTokenReused
	}
	if !now.Before(old.ExpiresAt) {
		return TokenPair{}, ErrRefreshTokenInvalid
	}
	memberships, err := s.repo.ListMemberships(ctx, old.UserID)
	if err != nil {
		return TokenPair{}, err
	}
	target := old.MerchantID
	if merchantID != "" {
		target = merchantID
	}
	var scope *Membership
	if target != "" {
		for i := range memberships {
			if memberships[i].MerchantID == target {
				scope = &memberships[i]
			}
		}
		if scope == nil {
			return TokenPair{}, ErrNotMember // membership removed or never existed
		}
	} else if len(memberships) == 1 {
		scope = &memberships[0]
	}
	return s.issue(ctx, old.UserID, scope, memberships, old.FamilyID, old.ID, now)
}

// Logout revokes the session family of refreshToken. Unknown tokens are a no-op.
func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	if refreshToken == "" {
		return nil
	}
	t, found, err := s.repo.GetRefreshTokenByHash(ctx, HashToken(refreshToken))
	if err != nil || !found {
		return err
	}
	return s.repo.RevokeRefreshFamily(ctx, t.FamilyID, s.now())
}

// MintAccessToken signs an access token without a refresh token (internal use,
// tests and the explicit dev-mode demo session).
func (s *Service) MintAccessToken(userID, merchantID string, role Role) (string, error) {
	now := s.now()
	jti, err := randomID("", 8)
	if err != nil {
		return "", err
	}
	return SignHS256(Claims{
		Sub:        userID,
		MerchantID: merchantID,
		Role:       role,
		Typ:        TokenTypeAccess,
		Iss:        s.cfg.Issuer,
		Jti:        jti,
		Iat:        now.Unix(),
		Exp:        now.Add(s.cfg.AccessTTL).Unix(),
	}, s.cfg.JWTSecret)
}

// ParseAccessToken validates an access token issued by this service.
func (s *Service) ParseAccessToken(token string) (*Claims, error) {
	c, err := ParseHS256(token, s.cfg.JWTSecret, s.now())
	if err != nil {
		return nil, err
	}
	if c.Typ != TokenTypeAccess || c.Sub == "" {
		return nil, ErrTokenInvalid
	}
	return c, nil
}

func (s *Service) issue(ctx context.Context, userID string, scope *Membership, memberships []Membership, familyID, rotateFrom string, now time.Time) (TokenPair, error) {
	var merchantID string
	var role Role
	if scope != nil {
		merchantID, role = scope.MerchantID, scope.Role
	}
	access, err := s.MintAccessToken(userID, merchantID, role)
	if err != nil {
		return TokenPair{}, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return TokenPair{}, err
	}
	refresh := "rt_" + base64.RawURLEncoding.EncodeToString(raw)
	id, err := randomID("rtk_", 12)
	if err != nil {
		return TokenPair{}, err
	}
	rec := RefreshToken{
		ID:         id,
		FamilyID:   familyID,
		UserID:     userID,
		MerchantID: merchantID,
		TokenHash:  HashToken(refresh),
		ExpiresAt:  now.Add(s.cfg.RefreshTTL),
		CreatedAt:  now,
	}
	if rotateFrom == "" {
		err = s.repo.CreateRefreshToken(ctx, rec)
	} else {
		err = s.repo.RotateRefreshToken(ctx, rotateFrom, rec, now)
		if errors.Is(err, ErrRefreshTokenReused) { // lost a concurrent rotation race
			_ = s.repo.RevokeRefreshFamily(ctx, familyID, now)
		}
	}
	if err != nil {
		return TokenPair{}, err
	}
	if memberships == nil {
		memberships = []Membership{}
	}
	return TokenPair{
		AccessToken:      access,
		TokenType:        "Bearer",
		ExpiresIn:        int64(s.cfg.AccessTTL / time.Second),
		RefreshToken:     refresh,
		RefreshExpiresIn: int64(s.cfg.RefreshTTL / time.Second),
		UserID:           userID,
		MerchantID:       merchantID,
		Role:             role,
		Memberships:      memberships,
	}, nil
}

// AddMembership links userID to merchantID with role (user must exist).
func (s *Service) AddMembership(ctx context.Context, userID, merchantID string, role Role) error {
	if !role.Valid() {
		return fmt.Errorf("auth: invalid role %q", role)
	}
	if _, found, err := s.repo.GetUser(ctx, userID); err != nil {
		return err
	} else if !found {
		return ErrUserNotFound
	}
	return s.repo.AddMembership(ctx, Membership{UserID: userID, MerchantID: merchantID, Role: role, CreatedAt: s.now()})
}
