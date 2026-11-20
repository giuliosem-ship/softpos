package auth_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"softpos/pkg/auth"
	"softpos/pkg/auth/authtest"
)

const testSecret = "unit_test_jwt_secret_at_least_32_bytes!!"

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newService(t *testing.T) (*auth.Service, *auth.MemoryRepository, *auth.MemorySender, *clock) {
	t.Helper()
	repo := auth.NewMemoryRepository()
	sender := auth.NewMemorySender()
	clk := &clock{t: time.Now().UTC()}
	svc, err := auth.NewService(repo, sender, auth.Config{JWTSecret: []byte(testSecret), Now: clk.Now})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, repo, sender, clk
}

func login(t *testing.T, svc *auth.Service, sender *auth.MemorySender, phone, merchantID string) auth.TokenPair {
	t.Helper()
	ctx := context.Background()
	if _, err := svc.RequestOTP(ctx, phone); err != nil {
		t.Fatalf("RequestOTP: %v", err)
	}
	code, _ := sender.LastCode(phone)
	tp, err := svc.VerifyOTP(ctx, phone, code, merchantID)
	if err != nil {
		t.Fatalf("VerifyOTP: %v", err)
	}
	return tp
}

func TestMemoryRepositoryContract(t *testing.T) {
	authtest.RunRepositoryContract(t, authtest.Harness{
		NewRepo: func(t *testing.T) (auth.Repository, authtest.MerchantSeeder) { return auth.NewMemoryRepository(), nil },
	})
}

func TestNewService_RejectsWeakSecret(t *testing.T) {
	if _, err := auth.NewService(auth.NewMemoryRepository(), auth.NewMemorySender(), auth.Config{JWTSecret: []byte("short")}); !errors.Is(err, auth.ErrWeakSecret) {
		t.Fatalf("want ErrWeakSecret, got %v", err)
	}
}

func TestOTPLogin_CreatesUserAndIssuesTokens(t *testing.T) {
	svc, repo, sender, _ := newService(t)
	phone := "+525512345678"
	tp := login(t, svc, sender, phone, "")
	if tp.AccessToken == "" || tp.RefreshToken == "" || tp.TokenType != "Bearer" || tp.ExpiresIn != int64(auth.DefaultAccessTTL/time.Second) {
		t.Fatalf("unexpected token pair: %+v", tp)
	}
	if tp.MerchantID != "" || len(tp.Memberships) != 0 {
		t.Fatalf("new user must have no merchant scope: %+v", tp)
	}
	claims, err := svc.ParseAccessToken(tp.AccessToken)
	if err != nil || claims.Sub != tp.UserID || claims.Typ != auth.TokenTypeAccess {
		t.Fatalf("ParseAccessToken = %+v %v", claims, err)
	}
	// OTP stored hashed: the code never appears in the repository.
	code, _ := sender.LastCode(phone)
	if _, err := svc.RequestOTP(context.Background(), "+525599990000"); err != nil {
		t.Fatal(err)
	}
	ch, _, _ := repo.GetOTPChallenge(context.Background(), "+525599990000")
	if strings.Contains(ch.CodeHash, code) || len(ch.CodeHash) != 64 {
		t.Fatalf("OTP must be stored as a 64-hex HMAC, got %q", ch.CodeHash)
	}
	// Refresh token stored hashed.
	if _, found, _ := repo.GetRefreshTokenByHash(context.Background(), tp.RefreshToken); found {
		t.Fatal("refresh token must not be stored in clear")
	}
	// Second login reuses the user and its single membership.
	if err := svc.AddMembership(context.Background(), tp.UserID, "mer_1", auth.RoleOwner); err != nil {
		t.Fatal(err)
	}
	tp2 := login(t, svc, sender, phone, "")
	if tp2.UserID != tp.UserID || tp2.MerchantID != "mer_1" || tp2.Role != auth.RoleOwner {
		t.Fatalf("second login = %+v", tp2)
	}
}

func TestOTP_InvalidPhoneExpiryAttemptsAndResend(t *testing.T) {
	svc, _, sender, clk := newService(t)
	ctx := context.Background()
	if _, err := svc.RequestOTP(ctx, "5512345678"); !errors.Is(err, auth.ErrInvalidPhone) {
		t.Fatalf("want ErrInvalidPhone, got %v", err)
	}
	phone := "+525511110000"
	if _, err := svc.RequestOTP(ctx, phone); err != nil {
		t.Fatal(err)
	}
	// Resend inside the interval is rate limited.
	if wait, err := svc.RequestOTP(ctx, phone); !errors.Is(err, auth.ErrOTPRateLimited) || wait <= 0 {
		t.Fatalf("want ErrOTPRateLimited with wait, got %v %v", wait, err)
	}
	// Expiry.
	code, _ := sender.LastCode(phone)
	clk.Advance(auth.DefaultOTPTTL + time.Second)
	if _, err := svc.VerifyOTP(ctx, phone, code, ""); !errors.Is(err, auth.ErrOTPInvalid) {
		t.Fatalf("expired OTP: want ErrOTPInvalid, got %v", err)
	}
	// Max attempts: 5 wrong guesses, then even the right code is locked out.
	if _, err := svc.RequestOTP(ctx, phone); err != nil {
		t.Fatal(err)
	}
	code, _ = sender.LastCode(phone)
	wrong := "000000"
	if wrong == code {
		wrong = "111111"
	}
	for i := 0; i < auth.DefaultOTPMaxAttempts; i++ {
		if _, err := svc.VerifyOTP(ctx, phone, wrong, ""); !errors.Is(err, auth.ErrOTPInvalid) {
			t.Fatalf("attempt %d: want ErrOTPInvalid, got %v", i, err)
		}
	}
	if _, err := svc.VerifyOTP(ctx, phone, code, ""); !errors.Is(err, auth.ErrOTPLocked) {
		t.Fatalf("want ErrOTPLocked, got %v", err)
	}
	// The challenge stays locked until it expires or a new code is requested.
	if _, err := svc.VerifyOTP(ctx, phone, code, ""); !errors.Is(err, auth.ErrOTPLocked) {
		t.Fatalf("after lockout: want ErrOTPLocked, got %v", err)
	}
}

func TestOTP_SingleUse(t *testing.T) {
	svc, _, sender, _ := newService(t)
	phone := "+525522223333"
	_ = login(t, svc, sender, phone, "")
	code, _ := sender.LastCode(phone)
	if _, err := svc.VerifyOTP(context.Background(), phone, code, ""); !errors.Is(err, auth.ErrOTPInvalid) {
		t.Fatalf("reused OTP: want ErrOTPInvalid, got %v", err)
	}
}

func TestOTP_DeliveryFailure(t *testing.T) {
	svc, _, sender, _ := newService(t)
	sender.Err = errors.New("provider down")
	if _, err := svc.RequestOTP(context.Background(), "+525500001111"); !errors.Is(err, auth.ErrOTPDelivery) {
		t.Fatalf("want ErrOTPDelivery, got %v", err)
	}
}

func TestVerifyOTP_MerchantSelection(t *testing.T) {
	svc, _, sender, _ := newService(t)
	ctx := context.Background()
	phone := "+525544445555"
	first := login(t, svc, sender, phone, "")
	_ = svc.AddMembership(ctx, first.UserID, "mer_a", auth.RoleOwner)
	_ = svc.AddMembership(ctx, first.UserID, "mer_b", auth.RoleCashier)

	// Several memberships and none selected: no tenant scope.
	tp := login(t, svc, sender, phone, "")
	if tp.MerchantID != "" || len(tp.Memberships) != 2 {
		t.Fatalf("multi-membership login = %+v", tp)
	}
	tp = login(t, svc, sender, phone, "mer_b")
	c, _ := svc.ParseAccessToken(tp.AccessToken)
	if c.MerchantID != "mer_b" || c.Role != auth.RoleCashier {
		t.Fatalf("claims = %+v", c)
	}
}

func TestVerifyOTP_NonMemberMerchantRejected(t *testing.T) {
	svc, _, sender, _ := newService(t)
	ctx := context.Background()
	phone := "+525566667777"
	if _, err := svc.RequestOTP(ctx, phone); err != nil {
		t.Fatal(err)
	}
	code, _ := sender.LastCode(phone)
	if _, err := svc.VerifyOTP(ctx, phone, code, "mer_foreign"); !errors.Is(err, auth.ErrNotMember) {
		t.Fatalf("want ErrNotMember, got %v", err)
	}
}

func TestRefresh_RotationAndReuseDetection(t *testing.T) {
	svc, _, sender, clk := newService(t)
	ctx := context.Background()
	tp := login(t, svc, sender, "+525588889999", "")

	rotated, err := svc.Refresh(ctx, tp.RefreshToken, "")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if rotated.RefreshToken == tp.RefreshToken {
		t.Fatal("refresh must rotate the token")
	}
	// Replaying the old token after the grace window is reuse: the whole
	// family is revoked.
	clk.Advance(auth.DefaultRefreshGrace + time.Second)
	if _, err := svc.Refresh(ctx, tp.RefreshToken, ""); !errors.Is(err, auth.ErrRefreshTokenReused) {
		t.Fatalf("want ErrRefreshTokenReused, got %v", err)
	}
	if _, err := svc.Refresh(ctx, rotated.RefreshToken, ""); !errors.Is(err, auth.ErrRefreshTokenReused) {
		t.Fatalf("current token must be revoked after reuse, got %v", err)
	}
	if _, err := svc.Refresh(ctx, "rt_unknown", ""); !errors.Is(err, auth.ErrRefreshTokenInvalid) {
		t.Fatalf("unknown token: want ErrRefreshTokenInvalid, got %v", err)
	}
}

// TestRefresh_GraceWindowReturnsSamePairOnce: an app killed mid-refresh
// re-presents the previous token; within the grace window it gets the same
// new refresh token back (once) instead of losing the session.
func TestRefresh_GraceWindowReturnsSamePairOnce(t *testing.T) {
	svc, _, sender, clk := newService(t)
	ctx := context.Background()
	tp := login(t, svc, sender, "+525588880001", "")
	rotated, err := svc.Refresh(ctx, tp.RefreshToken, "")
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(10 * time.Second)
	again, err := svc.Refresh(ctx, tp.RefreshToken, "")
	if err != nil {
		t.Fatalf("grace re-presentation: %v", err)
	}
	if again.RefreshToken != rotated.RefreshToken || again.AccessToken == "" || again.UserID != rotated.UserID {
		t.Fatalf("grace must return the same new refresh token, got %+v", again)
	}
	if _, err := svc.ParseAccessToken(again.AccessToken); err != nil {
		t.Fatalf("grace access token: %v", err)
	}
	// Only once: a second re-presentation is reuse and revokes the family.
	if _, err := svc.Refresh(ctx, tp.RefreshToken, ""); !errors.Is(err, auth.ErrRefreshTokenReused) {
		t.Fatalf("second re-presentation: want ErrRefreshTokenReused, got %v", err)
	}
	if _, err := svc.Refresh(ctx, rotated.RefreshToken, ""); !errors.Is(err, auth.ErrRefreshTokenReused) {
		t.Fatalf("family must be revoked after the second replay, got %v", err)
	}
}

// TestRefresh_GraceDoesNotApplyOnceSuccessorUsed: if the successor was already
// rotated (the client did receive it), re-presenting the old token is theft.
func TestRefresh_GraceDoesNotApplyOnceSuccessorUsed(t *testing.T) {
	svc, _, sender, _ := newService(t)
	ctx := context.Background()
	tp := login(t, svc, sender, "+525588880002", "")
	r1, _ := svc.Refresh(ctx, tp.RefreshToken, "")
	r2, err := svc.Refresh(ctx, r1.RefreshToken, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Refresh(ctx, tp.RefreshToken, ""); !errors.Is(err, auth.ErrRefreshTokenReused) {
		t.Fatalf("replay of a token whose successor was used: want reuse, got %v", err)
	}
	if _, err := svc.Refresh(ctx, r2.RefreshToken, ""); !errors.Is(err, auth.ErrRefreshTokenReused) {
		t.Fatalf("family must be revoked, got %v", err)
	}
}

func TestRefresh_GraceDisabledAndLogout(t *testing.T) {
	repo := auth.NewMemoryRepository()
	sender := auth.NewMemorySender()
	svc, err := auth.NewService(repo, sender, auth.Config{JWTSecret: []byte(testSecret), RefreshGrace: -1})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tp := login(t, svc, sender, "+525588880003", "")
	if _, err := svc.Refresh(ctx, tp.RefreshToken, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Refresh(ctx, tp.RefreshToken, ""); !errors.Is(err, auth.ErrRefreshTokenReused) {
		t.Fatalf("grace disabled: want reuse, got %v", err)
	}
	// With grace on, logout (family revoked) also ends the grace.
	svc2, _, sender2, _ := newService(t)
	tp2 := login(t, svc2, sender2, "+525588880004", "")
	r, _ := svc2.Refresh(ctx, tp2.RefreshToken, "")
	_ = svc2.Logout(ctx, r.RefreshToken)
	if _, err := svc2.Refresh(ctx, tp2.RefreshToken, ""); !errors.Is(err, auth.ErrRefreshTokenReused) {
		t.Fatalf("grace after logout: want reuse, got %v", err)
	}
}

// TestRefresh_ConcurrentRotationAtMostGracePair: of concurrent rotations of
// one token, one rotates and at most one more gets the same pair through the
// grace window; the others are reuse.
func TestRefresh_ConcurrentRotationOnlyOneWins(t *testing.T) {
	svc, _, sender, _ := newService(t)
	tp := login(t, svc, sender, "+525510101010", "")
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := map[string]int{}
	ok := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r, err := svc.Refresh(context.Background(), tp.RefreshToken, ""); err == nil {
				mu.Lock()
				ok++
				got[r.RefreshToken]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok < 1 || ok > 2 || len(got) != 1 {
		t.Fatalf("want one rotation (+ at most one grace replay of the same pair), got %d successes, %d distinct tokens", ok, len(got))
	}
}

func TestRefresh_ExpiryRescopeAndLogout(t *testing.T) {
	svc, _, sender, clk := newService(t)
	ctx := context.Background()
	tp := login(t, svc, sender, "+525520202020", "")
	_ = svc.AddMembership(ctx, tp.UserID, "mer_new", auth.RoleOwner)

	// Re-scope to the newly created merchant.
	scoped, err := svc.Refresh(ctx, tp.RefreshToken, "mer_new")
	if err != nil || scoped.MerchantID != "mer_new" || scoped.Role != auth.RoleOwner {
		t.Fatalf("rescope = %+v %v", scoped, err)
	}
	if _, err := svc.Refresh(ctx, scoped.RefreshToken, "mer_other"); !errors.Is(err, auth.ErrNotMember) {
		t.Fatalf("rescope to foreign merchant: want ErrNotMember, got %v", err)
	}
	// Failed rescope did not rotate: token still usable.
	next, err := svc.Refresh(ctx, scoped.RefreshToken, "")
	if err != nil || next.MerchantID != "mer_new" {
		t.Fatalf("refresh keeps scope = %+v %v", next, err)
	}
	if err := svc.Logout(ctx, next.RefreshToken); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Refresh(ctx, next.RefreshToken, ""); err == nil {
		t.Fatal("refresh after logout must fail")
	}
	// Expiry.
	tp2 := login(t, svc, sender, "+525530303030", "")
	clk.Advance(auth.DefaultRefreshTTL + time.Second)
	if _, err := svc.Refresh(ctx, tp2.RefreshToken, ""); !errors.Is(err, auth.ErrRefreshTokenInvalid) {
		t.Fatalf("expired refresh: want ErrRefreshTokenInvalid, got %v", err)
	}
}

func TestAccessToken_ExpiryAndTampering(t *testing.T) {
	svc, _, _, clk := newService(t)
	tok, err := svc.MintAccessToken("usr_x", "mer_x", auth.RoleOwner)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	forged := parts[0] + "." + parts[1] + "x." + parts[2]
	if _, err := svc.ParseAccessToken(forged); err == nil {
		t.Fatal("tampered token accepted")
	}
	noneAlg := "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0." + parts[1] + "."
	if _, err := svc.ParseAccessToken(noneAlg); err == nil {
		t.Fatal("alg=none token accepted")
	}
	other, _ := auth.NewService(auth.NewMemoryRepository(), auth.NewMemorySender(), auth.Config{JWTSecret: []byte(strings.Repeat("z", 32))})
	if _, err := other.ParseAccessToken(tok); err == nil {
		t.Fatal("token signed with another secret accepted")
	}
	clk.Advance(auth.DefaultAccessTTL + time.Second)
	if _, err := svc.ParseAccessToken(tok); !errors.Is(err, auth.ErrTokenExpired) {
		t.Fatalf("want ErrTokenExpired, got %v", err)
	}
}

// A lockout must not reset the resend throttle: otherwise an attacker gets a
// fresh code (and 5 more guesses) immediately after every lockout.
func TestOTP_LockoutKeepsResendThrottle(t *testing.T) {
	svc, _, sender, clk := newService(t)
	ctx := context.Background()
	phone := "+525544445555"
	if _, err := svc.RequestOTP(ctx, phone); err != nil {
		t.Fatal(err)
	}
	code, _ := sender.LastCode(phone)
	wrong := "000000"
	if wrong == code {
		wrong = "111111"
	}
	for i := 0; i < auth.DefaultOTPMaxAttempts; i++ {
		_, _ = svc.VerifyOTP(ctx, phone, wrong, "")
	}
	if _, err := svc.VerifyOTP(ctx, phone, code, ""); !errors.Is(err, auth.ErrOTPLocked) {
		t.Fatalf("want ErrOTPLocked, got %v", err)
	}
	if _, err := svc.RequestOTP(ctx, phone); !errors.Is(err, auth.ErrOTPRateLimited) {
		t.Fatalf("resend right after lockout: want ErrOTPRateLimited, got %v", err)
	}
	if _, err := svc.VerifyOTP(ctx, phone, code, ""); !errors.Is(err, auth.ErrOTPLocked) {
		t.Fatalf("locked challenge must stay locked, got %v", err)
	}
	clk.Advance(auth.DefaultOTPResendInterval + time.Second)
	if _, err := svc.RequestOTP(ctx, phone); err != nil {
		t.Fatalf("resend after interval: %v", err)
	}
	code, _ = sender.LastCode(phone)
	if _, err := svc.VerifyOTP(ctx, phone, code, ""); err != nil {
		t.Fatalf("fresh code after lockout: %v", err)
	}
}
