package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"softpos/migrations"
	"softpos/pkg/identity"
	"softpos/pkg/ledger"
	"softpos/pkg/payments"
	"softpos/pkg/rbf"
	"softpos/pkg/store/pgtest"
)

// openMigratedPG returns a real PostgreSQL connection on an isolated schema with
// all embedded migrations applied. Skips unless TEST_DATABASE_URL is set.
func openMigratedPG(t *testing.T) *sql.DB {
	t.Helper()
	db := pgtest.Open(t)
	if err := RunMigrationsFS(context.Background(), db, migrations.FS); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	return db
}

func TestPGIntegration_MigrationsIdempotentAndVersioned(t *testing.T) {
	db := openMigratedPG(t)
	ctx := context.Background()

	// Second and third runs must be no-ops.
	for i := 0; i < 2; i++ {
		if err := RunMigrationsFS(ctx, db, migrations.FS); err != nil {
			t.Fatalf("re-run %d: %v", i, err)
		}
	}

	all, err := LoadMigrations(migrations.FS)
	if err != nil {
		t.Fatalf("LoadMigrations: %v", err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if count != len(all) {
		t.Fatalf("expected %d recorded versions, got %d", len(all), count)
	}

	for _, table := range []string{"ledger_accounts", "ledger_journals", "ledger_entries", "merchants",
		"merchant_monthly_sales", "idempotency_keys", "sat_blacklist", "rbf_loans", "emv_payments", "outbox_events"} {
		var reg sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT to_regclass($1)::text`, table).Scan(&reg); err != nil || !reg.Valid {
			t.Errorf("expected table %s to exist (err=%v)", table, err)
		}
	}
}

func TestPGIntegration_LedgerDoubleEntryWithLocking(t *testing.T) {
	db := openMigratedPG(t)
	repo := NewSQLLedgerRepository(db)
	engine := ledger.NewLedgerEngineWithRepository(repo)

	for _, a := range []struct {
		code string
		typ  ledger.AccountType
	}{
		{"ASSET:STP_CONCENTRATOR", ledger.AccountAsset},
		{"ASSET:ACQUIRER_RECEIVABLE", ledger.AccountAsset},
		{"REVENUE:FEES", ledger.AccountRevenue},
		{"ASSET:RBF_LOANS_RECEIVABLE", ledger.AccountAsset},
		{"LIABILITY:MERCHANT:m1:AVAILABLE", ledger.AccountLiability},
		{"LIABILITY:MERCHANT:m1:TAX_ESCROW", ledger.AccountLiability},
		{"ASSET:RBF_LOANS_RECEIVABLE:m1", ledger.AccountAsset},
	} {
		if _, err := engine.CreateAccount(a.code, a.typ, a.code); err != nil {
			t.Fatalf("CreateAccount %s: %v", a.code, err)
		}
	}

	// Duplicate account (e.g. after a restart) maps to ErrDuplicateAccount.
	fresh := ledger.NewLedgerEngineWithRepository(repo)
	if _, err := fresh.CreateAccount("REVENUE:FEES", ledger.AccountRevenue, "x"); !errors.Is(err, ledger.ErrDuplicateAccount) {
		t.Fatalf("expected ErrDuplicateAccount, got %v", err)
	}

	if _, err := engine.RecordSplitPayment(10_000, "m1", 250, 1_000, 0); err != nil {
		t.Fatalf("RecordSplitPayment: %v", err)
	}

	bal, err := repo.GetBalance(context.Background(), "LIABILITY:MERCHANT:m1:AVAILABLE")
	if err != nil {
		t.Fatalf("GetBalance: %v", err)
	}
	if bal.AmountCents != 8_750 {
		t.Fatalf("expected available 8750, got %d", bal.AmountCents)
	}
	esc, _ := repo.GetBalance(context.Background(), "LIABILITY:MERCHANT:m1:TAX_ESCROW")
	if esc.AmountCents != 1_000 {
		t.Fatalf("expected escrow 1000, got %d", esc.AmountCents)
	}

	var debits, credits int64
	if err := db.QueryRow(`SELECT
		COALESCE(SUM(amount_cents) FILTER (WHERE direction='DEBIT'),0),
		COALESCE(SUM(amount_cents) FILTER (WHERE direction='CREDIT'),0)
		FROM ledger_entries`).Scan(&debits, &credits); err != nil {
		t.Fatalf("sum entries: %v", err)
	}
	if debits != credits || debits != 10_000 {
		t.Fatalf("unbalanced persisted entries: debits=%d credits=%d", debits, credits)
	}

	if _, err := repo.GetAccount(context.Background(), "NOPE"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("expected ErrAccountNotFound, got %v", err)
	}
}

func TestPGIntegration_MerchantRepository(t *testing.T) {
	db := openMigratedPG(t)
	ctx := context.Background()
	repo := NewSQLMerchantRepository(db)

	if err := repo.SaveMerchant(ctx, MerchantRecord{ID: "m1", RFC: "AAA010101AAA", Name: "Taqueria", IsRESICO: true}); err != nil {
		t.Fatalf("SaveMerchant: %v", err)
	}
	if err := repo.SetMerchantCLABE(ctx, "m1", "646180123400000007"); err != nil {
		t.Fatalf("SetMerchantCLABE: %v", err)
	}
	// Upsert without CLABE must keep the existing CLABE.
	if err := repo.SaveMerchant(ctx, MerchantRecord{ID: "m1", RFC: "AAA010101AAA", Name: "Taqueria 2", IsRESICO: true}); err != nil {
		t.Fatalf("SaveMerchant upsert: %v", err)
	}
	m, found, err := repo.GetMerchant(ctx, "m1")
	if err != nil || !found || m.Name != "Taqueria 2" || m.VirtualCLABE != "646180123400000007" {
		t.Fatalf("GetMerchant: %+v found=%v err=%v", m, found, err)
	}
	byClabe, found, err := repo.GetMerchantByCLABE(ctx, "646180123400000007")
	if err != nil || !found || byClabe.ID != "m1" {
		t.Fatalf("GetMerchantByCLABE: %+v found=%v err=%v", byClabe, found, err)
	}
	if _, found, _ := repo.GetMerchant(ctx, "missing"); found {
		t.Fatalf("expected missing merchant not found")
	}

	// A second merchant cannot take the same CLABE.
	_ = repo.SaveMerchant(ctx, MerchantRecord{ID: "m2", RFC: "BBB010101BBB", Name: "Other"})
	if err := repo.SetMerchantCLABE(ctx, "m2", "646180123400000007"); err == nil {
		t.Fatalf("expected unique violation for duplicate CLABE")
	}

	csd, _ := json.Marshal(map[string]string{"cert": "abc"})
	if err := repo.SaveMerchantCSD(ctx, "m1", csd); err != nil {
		t.Fatalf("SaveMerchantCSD: %v", err)
	}
	got, found, err := repo.GetMerchantCSD(ctx, "m1")
	if err != nil || !found {
		t.Fatalf("GetMerchantCSD: found=%v err=%v", found, err)
	}
	var decoded map[string]string
	if err := json.Unmarshal(got, &decoded); err != nil || decoded["cert"] != "abc" {
		t.Fatalf("CSD round trip mismatch: %s", got)
	}

	if total, err := repo.AddMonthlySalesCents(ctx, "m1", "2026-11", 5_000); err != nil || total != 5_000 {
		t.Fatalf("AddMonthlySalesCents first: %d %v", total, err)
	}
	if total, err := repo.AddMonthlySalesCents(ctx, "m1", "2026-11", 2_500); err != nil || total != 7_500 {
		t.Fatalf("AddMonthlySalesCents second: %d %v", total, err)
	}
	if got, _ := repo.GetMonthlySalesCents(ctx, "m1", "2026-11"); got != 7_500 {
		t.Fatalf("GetMonthlySalesCents: %d", got)
	}
}

func TestPGIntegration_IdempotencyAndBlacklist(t *testing.T) {
	db := openMigratedPG(t)
	ctx := context.Background()

	idem := NewSQLIdempotencyRepository(db)
	if err := idem.Set(ctx, "k1", 201, []byte(`{"ok":true}`), time.Hour); err != nil {
		t.Fatalf("Set: %v", err)
	}
	code, body, found, err := idem.Get(ctx, "k1")
	if err != nil || !found || code != 201 || string(body) != `{"ok":true}` {
		t.Fatalf("Get: %d %s %v %v", code, body, found, err)
	}
	if err := idem.Set(ctx, "k1", 200, []byte(`{}`), -time.Minute); err != nil {
		t.Fatalf("Set overwrite: %v", err)
	}
	if _, _, found, _ := idem.Get(ctx, "k1"); found {
		t.Fatalf("expected expired key to be treated as missing")
	}

	bl := NewSQLBlacklistRepository(db)
	if err := bl.AddEntry(ctx, identity.BlacklistEntry{RFC: "xxx010101xxx", Status: identity.BlacklistDefinitivo, OficioSAT: "500-05"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	entry, found, err := bl.GetEntry(ctx, "XXX010101XXX")
	if err != nil || !found || entry.Status != identity.BlacklistDefinitivo {
		t.Fatalf("GetEntry: %+v %v %v", entry, found, err)
	}
}

func TestPGIntegration_EMVPaymentAndRBFRepositories(t *testing.T) {
	db := openMigratedPG(t)
	ctx := context.Background()

	emv := NewSQLEMVPaymentRepository(db)
	rec := EMVPaymentRecord{
		PaymentID: "p1", MerchantID: "m1", AmountCents: 12_345, Status: payments.PaymentAuthorized,
		MaskedPAN: "476173******0010", CryptogramType: payments.CryptogramARQC, CryptogramHex: "AABBCCDDEEFF0011", ATC: 7,
	}
	if err := emv.SavePayment(ctx, rec); err != nil {
		t.Fatalf("SavePayment: %v", err)
	}
	if err := emv.TransitionStatus(ctx, "p1", payments.PaymentAuthorized, payments.PaymentSettled); err != nil {
		t.Fatalf("TransitionStatus: %v", err)
	}
	if err := emv.TransitionStatus(ctx, "p1", payments.PaymentAuthorized, payments.PaymentVoided); !errors.Is(err, ErrInvalidStateTransition) {
		t.Fatalf("expected invalid transition, got %v", err)
	}
	got, found, err := emv.GetPayment(ctx, "p1")
	if err != nil || !found || got.Status != payments.PaymentSettled || got.AmountCents != 12_345 || got.ATC != 7 {
		t.Fatalf("GetPayment: %+v %v %v", got, found, err)
	}

	rbfRepo := NewSQLRBFRepository(db)
	loan := &rbf.RBFLoan{ID: "loan1", MerchantID: "m1", PrincipalCents: 100_000, FeeCents: 10_000,
		RepaymentCapCents: 110_000, HoldbackBps: 1_000, Status: rbf.LoanActive}
	if err := rbfRepo.SaveLoan(ctx, loan); err != nil {
		t.Fatalf("SaveLoan: %v", err)
	}
	after, err := rbfRepo.ApplyRepaymentWithLock(ctx, "m1", 30_000)
	if err != nil || after.RepaidCents != 30_000 {
		t.Fatalf("ApplyRepaymentWithLock: %+v %v", after, err)
	}
	stored, found, err := rbfRepo.GetLoan(ctx, "m1")
	if err != nil || !found || stored.RepaidCents != 30_000 || stored.RemainingCents() != 80_000 {
		t.Fatalf("GetLoan: %+v %v %v", stored, found, err)
	}
}

func TestPGIntegration_OutboxRepository(t *testing.T) {
	db := openMigratedPG(t)
	ctx := context.Background()
	repo := NewSQLOutboxRepository(db)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if err := repo.SaveEventTx(ctx, tx, "payment.settled", "p1", map[string]any{"amount_cents": 100}); err != nil {
		t.Fatalf("SaveEventTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	events, err := repo.FetchPendingEventsWithLock(ctx, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("FetchPending: %d %v", len(events), err)
	}
	if err := repo.MarkFailed(ctx, events[0].ID, "boom"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	events, _ = repo.FetchPendingEventsWithLock(ctx, 10)
	if len(events) != 1 || events[0].RetryCount != 1 || events[0].LastError != "boom" {
		t.Fatalf("unexpected after MarkFailed: %+v", events)
	}
	if err := repo.MarkPublished(ctx, events[0].ID); err != nil {
		t.Fatalf("MarkPublished: %v", err)
	}
	if events, _ = repo.FetchPendingEventsWithLock(ctx, 10); len(events) != 0 {
		t.Fatalf("expected no pending events, got %d", len(events))
	}
}
