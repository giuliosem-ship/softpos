package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"softpos/pkg/auth"
	"softpos/pkg/auth/authtest"
)

func pgMerchantCreator(db *sql.DB) func(t *testing.T, id string) {
	return func(t *testing.T, id string) {
		t.Helper()
		if err := NewSQLMerchantRepository(db).SaveMerchant(context.Background(), MerchantRecord{ID: id, RFC: "XAXX010101000", Name: "M " + id, IsRESICO: true}); err != nil {
			t.Fatalf("seed merchant %s: %v", id, err)
		}
	}
}

func TestPGIntegration_AuthRepositoryContract(t *testing.T) {
	authtest.RunRepositoryContract(t, authtest.Harness{
		NewRepo: func(t *testing.T) (auth.Repository, authtest.MerchantSeeder) {
			db := openMigratedPG(t)
			return NewSQLAuthRepository(db), pgMerchantCreator(db)
		},
	})
}

func TestPGIntegration_AuthOTPLoginRefreshReuseAndConcurrency(t *testing.T) {
	db := openMigratedPG(t)
	ctx := context.Background()
	repo := NewSQLAuthRepository(db)
	sender := auth.NewMemorySender()
	svc, err := auth.NewService(repo, sender, auth.Config{JWTSecret: []byte("pg_integration_jwt_secret_32_bytes_min")})
	if err != nil {
		t.Fatal(err)
	}
	phone := "+525512340000"
	if _, err := svc.RequestOTP(ctx, phone); err != nil {
		t.Fatalf("RequestOTP: %v", err)
	}
	if _, err := svc.RequestOTP(ctx, phone); !errors.Is(err, auth.ErrOTPRateLimited) {
		t.Fatalf("resend: want ErrOTPRateLimited, got %v", err)
	}
	code, _ := sender.LastCode(phone)
	tp, err := svc.VerifyOTP(ctx, phone, code, "")
	if err != nil {
		t.Fatalf("VerifyOTP: %v", err)
	}
	// Membership persisted with FK to merchants.
	pgMerchantCreator(db)(t, "mer_pg_1")
	if err := svc.AddMembership(ctx, tp.UserID, "mer_pg_1", auth.RoleOwner); err != nil {
		t.Fatalf("AddMembership: %v", err)
	}
	if err := svc.AddMembership(ctx, tp.UserID, "mer_missing", auth.RoleOwner); err == nil {
		t.Fatal("membership to a non-existent merchant must fail (FK)")
	}
	scoped, err := svc.Refresh(ctx, tp.RefreshToken, "mer_pg_1")
	if err != nil || scoped.MerchantID != "mer_pg_1" {
		t.Fatalf("rescope = %+v %v", scoped, err)
	}

	// Concurrent rotation of the same token on a real DB: exactly one rotates;
	// at most one more gets the same pair through the one-time grace window
	// (MVP-19); the rest are reuse.
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	tokens := map[string]bool{}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r, err := svc.Refresh(ctx, scoped.RefreshToken, ""); err == nil {
				mu.Lock()
				wins++
				tokens[r.RefreshToken] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins < 1 || wins > 2 || len(tokens) != 1 {
		t.Fatalf("want 1 rotation (+ at most 1 grace replay of the same pair), got %d successes / %d tokens", wins, len(tokens))
	}
	// The losers triggered reuse detection: whole family revoked.
	var active int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM refresh_tokens WHERE revoked_at IS NULL`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("family must be fully revoked after reuse, %d active tokens left", active)
	}

	// Nothing secret stored in clear.
	var storedHash string
	if err := db.QueryRowContext(ctx, `SELECT token_hash FROM refresh_tokens LIMIT 1`).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if len(storedHash) != 64 {
		t.Fatalf("refresh token must be stored as SHA-256 hex, got %q", storedHash)
	}

	// Concurrent OTP guesses are bounded by max attempts.
	phone2 := "+525512349999"
	if _, err := svc.RequestOTP(ctx, phone2); err != nil {
		t.Fatal(err)
	}
	var attemptsWG sync.WaitGroup
	for i := 0; i < 20; i++ {
		attemptsWG.Add(1)
		go func() { defer attemptsWG.Done(); _, _ = svc.VerifyOTP(ctx, phone2, "999999x", "") }()
	}
	attemptsWG.Wait()
	code2, _ := sender.LastCode(phone2)
	if _, err := svc.VerifyOTP(ctx, phone2, code2, ""); err == nil {
		t.Fatal("correct code must be locked out after concurrent brute force")
	}
}
