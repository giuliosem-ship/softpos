package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"softpos/pkg/auth"
)

// TestAccessToken_IssuerAndAudience (MVP-31): access tokens carry iss/aud
// and a token minted with another issuer or audience (same secret) is rejected.
func TestAccessToken_IssuerAndAudience(t *testing.T) {
	repo := auth.NewMemoryRepository()
	mk := func(iss, aud string) *auth.Service {
		svc, err := auth.NewService(repo, auth.NewMemorySender(), auth.Config{JWTSecret: []byte(testSecret), Issuer: iss, Audience: aud})
		if err != nil {
			t.Fatal(err)
		}
		return svc
	}
	prod := mk("", "")
	tok, err := prod.MintAccessToken("usr_1", "m1", auth.RoleOwner)
	if err != nil {
		t.Fatal(err)
	}
	c, err := prod.ParseAccessToken(tok)
	if err != nil || c.Iss != auth.DefaultIssuer || c.Aud != auth.DefaultAudience {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	for _, other := range []*auth.Service{mk("kerno-staging", ""), mk("", "other-api")} {
		if _, err := other.ParseAccessToken(tok); !errors.Is(err, auth.ErrTokenInvalid) {
			t.Fatalf("foreign iss/aud must be rejected: %v", err)
		}
	}
	// A token without iss/aud (signed with the same secret) is rejected.
	bare, _ := auth.SignHS256(auth.Claims{Sub: "usr_1", MerchantID: "m1", Role: auth.RoleOwner, Typ: auth.TokenTypeAccess,
		Exp: time.Now().Add(time.Minute).Unix()}, []byte(testSecret))
	if _, err := prod.ParseAccessToken(bare); !errors.Is(err, auth.ErrTokenInvalid) {
		t.Fatalf("token without iss/aud: %v", err)
	}
}

// TestOTP_DailyLimit (MVP-31): at most OTPDailyLimit codes per phone per
// calendar day (Mexico City); the next day the counter starts again.
func TestOTP_DailyLimit(t *testing.T) {
	repo := auth.NewMemoryRepository()
	sender := auth.NewMemorySender()
	loc, _ := time.LoadLocation("America/Mexico_City")
	clk := &clock{t: time.Date(2026, 11, 20, 8, 0, 0, 0, loc)}
	svc, err := auth.NewService(repo, sender, auth.Config{JWTSecret: []byte(testSecret), Now: clk.Now, OTPDailyLimit: 3})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	phone := "+525511112222"
	for i := 0; i < 3; i++ {
		if _, err := svc.RequestOTP(ctx, phone); err != nil {
			t.Fatalf("send %d: %v", i+1, err)
		}
		clk.Advance(2 * time.Minute) // past the resend interval
	}
	if _, err := svc.RequestOTP(ctx, phone); !errors.Is(err, auth.ErrOTPDailyLimit) {
		t.Fatalf("4th send: want ErrOTPDailyLimit, got %v", err)
	}
	if _, err := svc.RequestOTP(ctx, "+525533334444"); err != nil {
		t.Fatalf("the cap is per phone: %v", err)
	}
	clk.Advance(24 * time.Hour)
	if _, err := svc.RequestOTP(ctx, phone); err != nil {
		t.Fatalf("next day: %v", err)
	}
	// A rate-limited (too soon) request does not consume the daily budget.
	if _, err := svc.RequestOTP(ctx, phone); !errors.Is(err, auth.ErrOTPRateLimited) {
		t.Fatalf("resend interval: %v", err)
	}
	if n, _ := repo.IncrementOTPSends(ctx, phone, clk.Now().In(loc).Format("2006-01-02")); n != 2 {
		t.Fatalf("rate-limited request must not count: %d", n)
	}
}
