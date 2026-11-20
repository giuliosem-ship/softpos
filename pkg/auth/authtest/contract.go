// Package authtest holds a behavioural contract shared by every
// auth.Repository implementation (memory and PostgreSQL).
package authtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"softpos/pkg/auth"
)

// MerchantSeeder creates a merchant row the repository can reference (FK).
type MerchantSeeder func(t *testing.T, merchantID string)

// Harness builds a fresh repository plus an optional merchant seeder (nil when
// the implementation has no merchant foreign key).
type Harness struct {
	NewRepo func(t *testing.T) (auth.Repository, MerchantSeeder)
}

// RunRepositoryContract exercises every Repository method.
func RunRepositoryContract(t *testing.T, h Harness) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	newRepo := func(t *testing.T) (auth.Repository, MerchantSeeder) {
		r, seed := h.NewRepo(t)
		if seed == nil {
			seed = func(*testing.T, string) {}
		}
		return r, seed
	}

	t.Run("users", func(t *testing.T) {
		r, _ := newRepo(t)
		u := auth.User{ID: "usr_1", Phone: "+525511112222", CreatedAt: now}
		if err := r.CreateUser(ctx, u); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		if err := r.CreateUser(ctx, auth.User{ID: "usr_2", Phone: u.Phone, CreatedAt: now}); !errors.Is(err, auth.ErrConflict) {
			t.Fatalf("duplicate phone: want ErrConflict, got %v", err)
		}
		got, found, err := r.GetUserByPhone(ctx, u.Phone)
		if err != nil || !found || got.ID != u.ID {
			t.Fatalf("GetUserByPhone = %+v %v %v", got, found, err)
		}
		if _, found, _ := r.GetUser(ctx, "usr_missing"); found {
			t.Fatal("unexpected user")
		}
	})

	t.Run("memberships", func(t *testing.T) {
		r, mkMerchant := newRepo(t)
		_ = r.CreateUser(ctx, auth.User{ID: "usr_m", Phone: "+525533334444", CreatedAt: now})
		mkMerchant(t, "mer_b")
		mkMerchant(t, "mer_a")
		if err := r.AddMembership(ctx, auth.Membership{UserID: "usr_m", MerchantID: "mer_b", Role: auth.RoleCashier, CreatedAt: now}); err != nil {
			t.Fatalf("AddMembership: %v", err)
		}
		if err := r.AddMembership(ctx, auth.Membership{UserID: "usr_m", MerchantID: "mer_a", Role: auth.RoleOwner, CreatedAt: now}); err != nil {
			t.Fatalf("AddMembership: %v", err)
		}
		// upsert changes role
		if err := r.AddMembership(ctx, auth.Membership{UserID: "usr_m", MerchantID: "mer_b", Role: auth.RoleOwner, CreatedAt: now}); err != nil {
			t.Fatalf("AddMembership upsert: %v", err)
		}
		list, err := r.ListMemberships(ctx, "usr_m")
		if err != nil || len(list) != 2 || list[0].MerchantID != "mer_a" || list[1].Role != auth.RoleOwner {
			t.Fatalf("ListMemberships = %+v %v", list, err)
		}
		m, found, err := r.GetMembership(ctx, "usr_m", "mer_a")
		if err != nil || !found || m.Role != auth.RoleOwner {
			t.Fatalf("GetMembership = %+v %v %v", m, found, err)
		}
		if _, found, _ := r.GetMembership(ctx, "usr_m", "mer_zzz"); found {
			t.Fatal("unexpected membership")
		}
	})

	t.Run("otp", func(t *testing.T) {
		r, _ := newRepo(t)
		phone := "+525555556666"
		c := auth.OTPChallenge{Phone: phone, CodeHash: "h1", ExpiresAt: now.Add(time.Minute), LastSentAt: now}
		if err := r.SaveOTPChallenge(ctx, c); err != nil {
			t.Fatalf("SaveOTPChallenge: %v", err)
		}
		for want := 1; want <= 2; want++ {
			n, found, err := r.IncrementOTPAttempts(ctx, phone)
			if err != nil || !found || n != want {
				t.Fatalf("IncrementOTPAttempts = %d %v %v, want %d", n, found, err, want)
			}
		}
		c.CodeHash = "h2"
		_ = r.SaveOTPChallenge(ctx, c) // resend resets attempts
		got, found, err := r.GetOTPChallenge(ctx, phone)
		if err != nil || !found || got.CodeHash != "h2" || got.Attempts != 0 || !got.ExpiresAt.Equal(c.ExpiresAt) {
			t.Fatalf("GetOTPChallenge = %+v %v %v", got, found, err)
		}
		if del, err := r.DeleteOTPChallenge(ctx, phone); err != nil || !del {
			t.Fatalf("first delete = %v %v", del, err)
		}
		if del, _ := r.DeleteOTPChallenge(ctx, phone); del {
			t.Fatal("second delete must report false")
		}
		if _, found, _ := r.IncrementOTPAttempts(ctx, phone); found {
			t.Fatal("increment on missing challenge must report not found")
		}
	})

	t.Run("refresh rotation and family revocation", func(t *testing.T) {
		r, _ := newRepo(t)
		_ = r.CreateUser(ctx, auth.User{ID: "usr_r", Phone: "+525577778888", CreatedAt: now})
		t1 := auth.RefreshToken{ID: "rtk_1", FamilyID: "fam_1", UserID: "usr_r", TokenHash: "hash1", ExpiresAt: now.Add(time.Hour), CreatedAt: now}
		if err := r.CreateRefreshToken(ctx, t1); err != nil {
			t.Fatalf("CreateRefreshToken: %v", err)
		}
		t2 := t1
		t2.ID, t2.TokenHash = "rtk_2", "hash2"
		if err := r.RotateRefreshToken(ctx, "rtk_1", t2, now); err != nil {
			t.Fatalf("Rotate: %v", err)
		}
		old, found, _ := r.GetRefreshTokenByHash(ctx, "hash1")
		if !found || old.RevokedAt == nil || old.ReplacedBy != "rtk_2" {
			t.Fatalf("old token after rotate = %+v", old)
		}
		t3 := t1
		t3.ID, t3.TokenHash = "rtk_3", "hash3"
		if err := r.RotateRefreshToken(ctx, "rtk_1", t3, now); !errors.Is(err, auth.ErrRefreshTokenReused) {
			t.Fatalf("rotating a revoked token: want ErrRefreshTokenReused, got %v", err)
		}
		if _, found, _ := r.GetRefreshTokenByHash(ctx, "hash3"); found {
			t.Fatal("failed rotation must not insert the new token")
		}
		if err := r.RevokeRefreshFamily(ctx, "fam_1", now); err != nil {
			t.Fatalf("RevokeRefreshFamily: %v", err)
		}
		cur, _, _ := r.GetRefreshTokenByHash(ctx, "hash2")
		if cur.RevokedAt == nil {
			t.Fatal("family revocation must revoke the current token")
		}
	})

	t.Run("platform operators", func(t *testing.T) {
		r, _ := newRepo(t)
		_ = r.CreateUser(ctx, auth.User{ID: "usr_op", Phone: "+525566660000", CreatedAt: now})
		if ok, err := r.IsPlatformOperator(ctx, "usr_op"); err != nil || ok {
			t.Fatalf("not an operator yet: %v %v", ok, err)
		}
		if err := r.AddPlatformOperator(ctx, "usr_op", "test"); err != nil {
			t.Fatal(err)
		}
		if err := r.AddPlatformOperator(ctx, "usr_op", "test"); err != nil {
			t.Fatalf("idempotent add: %v", err)
		}
		if ok, _ := r.IsPlatformOperator(ctx, "usr_op"); !ok {
			t.Fatal("operator not recorded")
		}
		if err := r.AddPlatformOperator(ctx, "usr_ghost", "test"); !errors.Is(err, auth.ErrUserNotFound) {
			t.Fatalf("unknown user: want ErrUserNotFound, got %v", err)
		}
		if err := r.RemovePlatformOperator(ctx, "usr_op"); err != nil {
			t.Fatal(err)
		}
		if ok, _ := r.IsPlatformOperator(ctx, "usr_op"); ok {
			t.Fatal("operator not removed")
		}
	})

	t.Run("refresh grace sealed successor and one-time claim", func(t *testing.T) {
		r, _ := newRepo(t)
		_ = r.CreateUser(ctx, auth.User{ID: "usr_g", Phone: "+525577770000", CreatedAt: now})
		t1 := auth.RefreshToken{ID: "rtk_g1", FamilyID: "fam_g", UserID: "usr_g", TokenHash: "gh1", ExpiresAt: now.Add(time.Hour), CreatedAt: now}
		_ = r.CreateRefreshToken(ctx, t1)
		t2 := t1
		t2.ID, t2.TokenHash, t2.GraceSealed = "rtk_g2", "gh2", []byte{1, 2, 3, 0, 255}
		if err := r.RotateRefreshToken(ctx, "rtk_g1", t2, now); err != nil {
			t.Fatal(err)
		}
		got, found, err := r.GetRefreshTokenByID(ctx, "rtk_g2")
		if err != nil || !found || string(got.GraceSealed) != string(t2.GraceSealed) || got.GraceUsedAt != nil {
			t.Fatalf("GetRefreshTokenByID = %+v %v %v", got, found, err)
		}
		if _, found, _ := r.GetRefreshTokenByID(ctx, "rtk_missing"); found {
			t.Fatal("unexpected token")
		}
		if ok, err := r.ClaimRefreshGrace(ctx, "rtk_g1", now); err != nil || !ok {
			t.Fatalf("first claim: %v %v", ok, err)
		}
		if ok, _ := r.ClaimRefreshGrace(ctx, "rtk_g1", now); ok {
			t.Fatal("grace must be claimable once")
		}
		old, _, _ := r.GetRefreshTokenByHash(ctx, "gh1")
		if old.GraceUsedAt == nil {
			t.Fatal("grace_used_at not recorded")
		}
	})

	t.Run("attestation nonce single use", func(t *testing.T) {
		r, _ := newRepo(t)
		n := auth.AttestationNonce{NonceHash: "nh1", ExpiresAt: now.Add(time.Minute), CreatedAt: now}
		if err := r.SaveAttestationNonce(ctx, n); err != nil {
			t.Fatalf("SaveAttestationNonce: %v", err)
		}
		if ok, err := r.ConsumeAttestationNonce(ctx, "nh1", now); err != nil || !ok {
			t.Fatalf("first consume = %v %v", ok, err)
		}
		if ok, _ := r.ConsumeAttestationNonce(ctx, "nh1", now); ok {
			t.Fatal("nonce consumed twice")
		}
		if ok, _ := r.ConsumeAttestationNonce(ctx, "unknown", now); ok {
			t.Fatal("unknown nonce consumed")
		}
		_ = r.SaveAttestationNonce(ctx, auth.AttestationNonce{NonceHash: "nh2", ExpiresAt: now.Add(time.Minute), CreatedAt: now})
		if ok, _ := r.ConsumeAttestationNonce(ctx, "nh2", now.Add(2*time.Minute)); ok {
			t.Fatal("expired nonce consumed")
		}
	})
}
