package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
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
	Issuer            string        // "iss" minted and required on access tokens; default "kerno"
	Audience          string        // "aud" minted and required on access tokens; default "kerno-api"
	AccessTTL         time.Duration // default 15m
	RefreshTTL        time.Duration // default 30 days
	OTPTTL            time.Duration // default 5m
	OTPResendInterval time.Duration // default 60s
	OTPMaxAttempts    int           // default 5
	// OTPDailyLimit caps the codes sent to one phone per calendar day
	// (America/Mexico_City) across instances, when the repository is an
	// OTPSendCounter (default 10; negative disables).
	OTPDailyLimit int
	// RefreshGrace is how long after a rotation the immediately previous
	// refresh token may be presented again (once) and get the same new
	// refresh token back instead of revoking the family (default 30s,
	// negative disables). See docs/AUTH.md for the trade-off.
	RefreshGrace time.Duration
	Now          func() time.Time
}

const (
	DefaultAccessTTL         = 15 * time.Minute
	DefaultRefreshTTL        = 30 * 24 * time.Hour
	DefaultOTPTTL            = 5 * time.Minute
	DefaultOTPResendInterval = 60 * time.Second
	DefaultOTPMaxAttempts    = 5
	DefaultOTPDailyLimit     = 10
	DefaultIssuer            = "kerno"
	DefaultAudience          = "kerno-api"
	DefaultRefreshGrace      = 30 * time.Second
	maxRefreshGrace          = 2 * time.Minute
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
	graceKey  []byte // derives the per-rotation grace sealing keys
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
		cfg.Issuer = DefaultIssuer
	}
	if cfg.Audience == "" {
		cfg.Audience = DefaultAudience
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
	if cfg.OTPDailyLimit == 0 {
		cfg.OTPDailyLimit = DefaultOTPDailyLimit
	}
	if cfg.RefreshGrace == 0 {
		cfg.RefreshGrace = DefaultRefreshGrace
	}
	if cfg.RefreshGrace > maxRefreshGrace {
		cfg.RefreshGrace = maxRefreshGrace
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	secret := append([]byte(nil), cfg.JWTSecret...)
	cfg.JWTSecret = secret
	// The OTP pepper is derived from (not equal to) the JWT secret.
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("kerno-otp-pepper-v1"))
	g := hmac.New(sha256.New, secret)
	g.Write([]byte("kerno-refresh-grace-v1"))
	return &Service{repo: repo, sender: sender, cfg: cfg, otpPepper: m.Sum(nil), graceKey: g.Sum(nil)}, nil
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

// otpDayZone is the calendar of the daily OTP cap (Mexican users).
var otpDayZone = func() *time.Location {
	if loc, err := time.LoadLocation("America/Mexico_City"); err == nil {
		return loc
	}
	return time.FixedZone("CST", -6*3600)
}()

func otpDay(t time.Time) string { return t.In(otpDayZone).Format("2006-01-02") }

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
	if counter, ok := s.repo.(OTPSendCounter); ok && s.cfg.OTPDailyLimit > 0 {
		sends, err := counter.IncrementOTPSends(ctx, phone, otpDay(now))
		if err != nil {
			return 0, err
		}
		if sends > s.cfg.OTPDailyLimit {
			return 0, ErrOTPDailyLimit
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
		// Keep the locked challenge: deleting it would also drop LastSentAt and
		// let RequestOTP issue a fresh code (5 more guesses) immediately.
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
	scope, err := s.resolveScope(ctx, user.ID, memberships, merchantID)
	if err != nil {
		return TokenPair{}, err
	}
	familyID, err := randomID("fam_", 16)
	if err != nil {
		return TokenPair{}, err
	}
	return s.issue(ctx, user.ID, scope, memberships, familyID, "", "", now)
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
// token revokes the whole family and returns ErrRefreshTokenReused, except
// within the grace window (Config.RefreshGrace): the immediately previous
// token presented again, once, while its successor is still unused, returns
// the same successor refresh token (plus a fresh access token). This covers
// an app killed mid-refresh or a retried request that lost the response.
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
		return s.graceOrRevoke(ctx, refreshToken, old, merchantID, now)
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
	scope, err := s.resolveScope(ctx, old.UserID, memberships, target)
	if err != nil {
		return TokenPair{}, err
	}
	tp, err := s.issue(ctx, old.UserID, scope, memberships, old.FamilyID, old.ID, refreshToken, now)
	if errors.Is(err, ErrRefreshTokenReused) { // lost a concurrent rotation of the same token
		if cur, found, gErr := s.repo.GetRefreshTokenByHash(ctx, old.TokenHash); gErr == nil && found {
			return s.graceOrRevoke(ctx, refreshToken, cur, merchantID, now)
		}
		_ = s.repo.RevokeRefreshFamily(ctx, old.FamilyID, now)
	}
	return tp, err
}

// pickScope selects the membership for target ("" = the only membership).
func pickScope(memberships []Membership, target string) (*Membership, error) {
	if target != "" {
		for i := range memberships {
			if memberships[i].MerchantID == target {
				return &memberships[i], nil
			}
		}
		return nil, ErrNotMember // membership removed or never existed
	}
	if len(memberships) == 1 {
		return &memberships[0], nil
	}
	return nil, nil
}

// resolveScope is pickScope plus the backoffice pseudo-tenant: target
// BackofficeScope requires a platform operator and yields RoleOperator.
func (s *Service) resolveScope(ctx context.Context, userID string, memberships []Membership, target string) (*Membership, error) {
	if target != BackofficeScope {
		return pickScope(memberships, target)
	}
	ok, err := s.repo.IsPlatformOperator(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotMember
	}
	return &Membership{UserID: userID, MerchantID: BackofficeScope, Role: RoleOperator}, nil
}

// GrantPlatformOperator makes the user of phone (created if needed) a
// platform operator. Trusted tooling only (bootstrap, operatorctl).
func (s *Service) GrantPlatformOperator(ctx context.Context, phone, createdBy string) (User, error) {
	u, err := s.EnsureUser(ctx, phone)
	if err != nil {
		return User{}, err
	}
	return u, s.repo.AddPlatformOperator(ctx, u.ID, createdBy)
}

// IsPlatformOperator reports whether userID is a platform operator.
func (s *Service) IsPlatformOperator(ctx context.Context, userID string) (bool, error) {
	return s.repo.IsPlatformOperator(ctx, userID)
}

// graceOrRevoke handles a revoked token: the grace re-presentation when it
// applies, reuse detection (family revoked) otherwise.
func (s *Service) graceOrRevoke(ctx context.Context, raw string, old RefreshToken, merchantID string, now time.Time) (TokenPair, error) {
	if tp, ok := s.refreshGrace(ctx, raw, old, merchantID, now); ok {
		return tp, nil
	}
	_ = s.repo.RevokeRefreshFamily(ctx, old.FamilyID, now)
	return TokenPair{}, ErrRefreshTokenReused
}

func (s *Service) refreshGrace(ctx context.Context, raw string, old RefreshToken, merchantID string, now time.Time) (TokenPair, bool) {
	if s.cfg.RefreshGrace <= 0 || old.RevokedAt == nil || old.ReplacedBy == "" || old.GraceUsedAt != nil {
		return TokenPair{}, false
	}
	// A concurrent loser may have read its clock before the winner revoked
	// the token: only the upper bound matters.
	if now.Sub(*old.RevokedAt) > s.cfg.RefreshGrace {
		return TokenPair{}, false
	}
	next, found, err := s.repo.GetRefreshTokenByID(ctx, old.ReplacedBy)
	if err != nil || !found || next.RevokedAt != nil || !now.Before(next.ExpiresAt) {
		return TokenPair{}, false // the successor was already used, revoked or expired
	}
	if merchantID != "" && merchantID != next.MerchantID {
		return TokenPair{}, false
	}
	successor, err := s.openGrace(raw, next)
	if err != nil || HashToken(successor) != next.TokenHash {
		return TokenPair{}, false
	}
	if claimed, err := s.repo.ClaimRefreshGrace(ctx, old.ID, now); err != nil || !claimed {
		return TokenPair{}, false
	}
	memberships, err := s.repo.ListMemberships(ctx, next.UserID)
	if err != nil {
		return TokenPair{}, false
	}
	scope, err := s.resolveScope(ctx, next.UserID, memberships, next.MerchantID)
	if err != nil {
		return TokenPair{}, false
	}
	var mID string
	var role Role
	if scope != nil {
		mID, role = scope.MerchantID, scope.Role
	}
	access, err := s.MintAccessToken(next.UserID, mID, role)
	if err != nil {
		return TokenPair{}, false
	}
	if memberships == nil {
		memberships = []Membership{}
	}
	return TokenPair{
		AccessToken: access, TokenType: "Bearer", ExpiresIn: int64(s.cfg.AccessTTL / time.Second),
		RefreshToken: successor, RefreshExpiresIn: int64(next.ExpiresAt.Sub(now) / time.Second),
		UserID: next.UserID, MerchantID: mID, Role: role, Memberships: memberships,
	}, true
}

// graceAEAD derives the AES-256-GCM key that seals a successor token under
// the raw value of its predecessor (never stored: only its hash is).
func (s *Service) graceAEAD(predecessorRaw string) (cipher.AEAD, error) {
	m := hmac.New(sha256.New, s.graceKey)
	m.Write([]byte(predecessorRaw))
	block, err := aes.NewCipher(m.Sum(nil))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (s *Service) sealGrace(predecessorRaw, successorRaw, successorID string) ([]byte, error) {
	g, err := s.graceAEAD(predecessorRaw)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return g.Seal(nonce, nonce, []byte(successorRaw), []byte(successorID)), nil
}

func (s *Service) openGrace(predecessorRaw string, next RefreshToken) (string, error) {
	g, err := s.graceAEAD(predecessorRaw)
	if err != nil {
		return "", err
	}
	if len(next.GraceSealed) <= g.NonceSize() {
		return "", ErrRefreshTokenInvalid
	}
	ns := g.NonceSize()
	out, err := g.Open(nil, next.GraceSealed[:ns], next.GraceSealed[ns:], []byte(next.ID))
	if err != nil {
		return "", ErrRefreshTokenInvalid
	}
	return string(out), nil
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
		Aud:        s.cfg.Audience,
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
	if c.Typ != TokenTypeAccess || c.Sub == "" || c.Iss != s.cfg.Issuer || c.Aud != s.cfg.Audience {
		return nil, ErrTokenInvalid
	}
	return c, nil
}

// issue mints a token pair. With rotateFrom (the id of the presented token)
// the new refresh token replaces it atomically and is sealed for the grace
// window under presentedRaw.
func (s *Service) issue(ctx context.Context, userID string, scope *Membership, memberships []Membership, familyID, rotateFrom, presentedRaw string, now time.Time) (TokenPair, error) {
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
		if rec.GraceSealed, err = s.sealGrace(presentedRaw, refresh, rec.ID); err != nil {
			return TokenPair{}, err
		}
		err = s.repo.RotateRefreshToken(ctx, rotateFrom, rec, now) // ErrRefreshTokenReused: lost a race
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

// DefaultAttestationNonceTTL matches the Android PlayIntegrityClient default.
const DefaultAttestationNonceTTL = 300 * time.Second

// IssueAttestationNonce creates a random single-use nonce for Play Integrity
// (stored as SHA-256) and returns it with its issue time and TTL.
func (s *Service) IssueAttestationNonce(ctx context.Context) (nonce string, issuedAt time.Time, ttl time.Duration, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, 0, err
	}
	nonce = base64.RawURLEncoding.EncodeToString(raw)
	now := s.now()
	ttl = DefaultAttestationNonceTTL
	if err := s.repo.SaveAttestationNonce(ctx, AttestationNonce{NonceHash: HashToken(nonce), ExpiresAt: now.Add(ttl), CreatedAt: now}); err != nil {
		return "", time.Time{}, 0, err
	}
	return nonce, now, ttl, nil
}

// ConsumeAttestationNonce burns nonce; ErrAttestationNonceInvalid if it is
// unknown, expired or was already used.
func (s *Service) ConsumeAttestationNonce(ctx context.Context, nonce string) error {
	if nonce == "" {
		return ErrAttestationNonceInvalid
	}
	ok, err := s.repo.ConsumeAttestationNonce(ctx, HashToken(nonce), s.now())
	if err != nil {
		return err
	}
	if !ok {
		return ErrAttestationNonceInvalid
	}
	return nil
}

// EnsureUser returns the user for phone, creating it if needed.
func (s *Service) EnsureUser(ctx context.Context, phone string) (User, error) {
	if !ValidPhone(phone) {
		return User{}, ErrInvalidPhone
	}
	return s.getOrCreateUser(ctx, phone, s.now())
}
