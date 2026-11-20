package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"softpos/pkg/ledger"
	"softpos/pkg/payments"
)

var errAbort = errors.New("abort unit of work")

type moneyFixture struct {
	ms       MoneyStore
	payment  func() payments.PaymentStatus
	balance  func(code string) int64
	sales    func() int64
	opExists func(key string) bool
}

const (
	fixMerchant = "m_money"
	fixPayment  = "pay_money"
	fixYM       = "2026-11"
	fixAvail    = "LIABILITY:MERCHANT:m_money:AVAILABLE"
	fixRecv     = "ASSET:ACQUIRER_RECEIVABLE"
)

func memoryMoneyFixture(t *testing.T) moneyFixture {
	t.Helper()
	le := ledger.NewLedgerEngine()
	_, _ = le.CreateAccount(fixAvail, ledger.AccountLiability, "avail")
	_, _ = le.CreateAccount(fixRecv, ledger.AccountAsset, "recv")
	pe := payments.NewPaymentEngine()
	if _, err := pe.Authorize(fixPayment, fixMerchant, payments.EMVTags{AmountCents: 1000, CurrencyCode: "0484", CryptogramType: payments.CryptogramARQC}); err != nil {
		t.Fatal(err)
	}
	repo := NewMemoryMerchantRepository()
	_ = repo.SaveMerchant(context.Background(), MerchantRecord{ID: fixMerchant, RFC: "GARM850101ABC", Name: "M"})
	ms := NewMemoryMoneyStore(le, func() payments.PaymentProcessor { return pe }, func() MerchantRepository { return repo })
	return moneyFixture{
		ms:      ms,
		payment: func() payments.PaymentStatus { p, _ := pe.GetPayment(fixPayment); return p.Status },
		balance: func(code string) int64 { b, _ := le.GetBalance(code); return b.AmountCents },
		sales:   func() int64 { v, _ := repo.GetMonthlySalesCents(context.Background(), fixMerchant, fixYM); return v },
		opExists: func(key string) bool {
			ok := false
			_ = ms.InTx(context.Background(), func(tx MoneyTx) error {
				claimed, _, _, _ := tx.ClaimOperation(context.Background(), key)
				ok = !claimed
				return errAbort
			})
			return ok
		},
	}
}

func pgMoneyFixture(t *testing.T) moneyFixture {
	t.Helper()
	db := openMigratedPG(t)
	ctx := context.Background()
	lrepo := NewSQLLedgerRepository(db)
	for _, a := range []ledger.Account{{ID: fixAvail, Code: fixAvail, Type: ledger.AccountLiability}, {ID: fixRecv, Code: fixRecv, Type: ledger.AccountAsset}} {
		if err := lrepo.CreateAccount(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	mrepo := NewSQLMerchantRepository(db)
	if err := mrepo.SaveMerchant(ctx, MerchantRecord{ID: fixMerchant, RFC: "GARM850101ABC", Name: "M"}); err != nil {
		t.Fatal(err)
	}
	pe := NewSQLPaymentEngine(NewSQLEMVPaymentRepository(db))
	if _, err := pe.Authorize(fixPayment, fixMerchant, payments.EMVTags{AmountCents: 1000, CurrencyCode: "0484", CryptogramType: payments.CryptogramARQC}); err != nil {
		t.Fatal(err)
	}
	return moneyFixture{
		ms:      NewSQLMoneyStore(db),
		payment: func() payments.PaymentStatus { p, _ := pe.GetPayment(fixPayment); return p.Status },
		balance: func(code string) int64 { b, _ := lrepo.GetBalance(ctx, code); return b.AmountCents },
		sales:   func() int64 { v, _ := mrepo.GetMonthlySalesCents(ctx, fixMerchant, fixYM); return v },
		opExists: func(key string) bool {
			var n int
			_ = db.QueryRow(`SELECT COUNT(*) FROM money_operations WHERE op_key = $1 AND status_code > 0`, key).Scan(&n)
			return n == 1
		},
	}
}

// writeEverything performs one write of every kind and then returns ret.
func writeEverything(t *testing.T, ms MoneyStore, ret error) error {
	t.Helper()
	ctx := context.Background()
	return ms.InTx(ctx, func(tx MoneyTx) error {
		if _, err := tx.GetPaymentForUpdate(ctx, fixPayment); err != nil {
			return err
		}
		if claimed, _, _, err := tx.ClaimOperation(ctx, "op:1"); err != nil || !claimed {
			t.Fatalf("claim: %v %v", claimed, err)
		}
		if _, err := tx.MonthlySalesForUpdate(ctx, fixMerchant, fixYM); err != nil {
			return err
		}
		jnl, _ := ledger.NewJournalEntry("j_"+fixPayment, time.Now(), "t", []ledger.Entry{
			{AccountID: fixRecv, Direction: ledger.Debit, Money: ledger.NewMoney(1000, "MXN")},
			{AccountID: fixAvail, Direction: ledger.Credit, Money: ledger.NewMoney(1000, "MXN")},
		}, nil)
		if err := tx.PostJournal(ctx, jnl); err != nil {
			return err
		}
		bal, err := tx.LockAccountBalances(ctx, fixAvail)
		if err != nil || bal[fixAvail] != 1000 {
			t.Fatalf("balance inside tx must see the pending journal: %v %v", bal, err)
		}
		if err := tx.SetPaymentStatus(ctx, fixPayment, payments.PaymentAuthorized, payments.PaymentSettled); err != nil {
			return err
		}
		if err := tx.AddMonthlySales(ctx, fixMerchant, fixYM, 862); err != nil {
			return err
		}
		if err := tx.InsertSettlement(ctx, SettlementRecord{PaymentID: fixPayment, MerchantID: fixMerchant, GrossCents: 1000, BaseCents: 862, YearMonth: fixYM, JournalID: jnl.ID}); err != nil {
			return err
		}
		if err := tx.EnqueueEvent(ctx, "payment.settled", fixPayment, map[string]int{"x": 1}); err != nil {
			return err
		}
		if err := tx.CompleteOperation(ctx, "op:1", 200, []byte(`{"ok":true}`)); err != nil {
			return err
		}
		return ret
	})
}

func checkMoneyStoreAtomicity(t *testing.T, f moneyFixture) {
	if err := writeEverything(t, f.ms, errAbort); !errors.Is(err, errAbort) {
		t.Fatalf("want errAbort, got %v", err)
	}
	if f.payment() != payments.PaymentAuthorized || f.balance(fixAvail) != 0 || f.sales() != 0 || f.opExists("op:1") {
		t.Fatalf("rolled-back unit of work leaked state: status=%s avail=%d sales=%d op=%v", f.payment(), f.balance(fixAvail), f.sales(), f.opExists("op:1"))
	}
	if err := writeEverything(t, f.ms, nil); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if f.payment() != payments.PaymentSettled || f.balance(fixAvail) != 1000 || f.sales() != 862 || !f.opExists("op:1") {
		t.Fatalf("committed state missing: status=%s avail=%d sales=%d", f.payment(), f.balance(fixAvail), f.sales())
	}
	// Second claim of the same key replays; second settlement / journal are rejected.
	ctx := context.Background()
	err := f.ms.InTx(ctx, func(tx MoneyTx) error {
		claimed, code, body, err := tx.ClaimOperation(ctx, "op:1")
		if err != nil || claimed || code != 200 || string(body) != `{"ok":true}` {
			t.Fatalf("replay claim: claimed=%v code=%d body=%s err=%v", claimed, code, body, err)
		}
		if err := tx.InsertSettlement(ctx, SettlementRecord{PaymentID: fixPayment, MerchantID: fixMerchant, GrossCents: 1000, YearMonth: fixYM, JournalID: "x"}); !errors.Is(err, ErrDuplicateSettlement) {
			t.Fatalf("duplicate settlement: %v", err)
		}
		if err := tx.AddMonthlySales(ctx, fixMerchant, fixYM, -5000); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.sales() != 0 {
		t.Fatalf("negative delta must floor the accumulator at 0, got %d", f.sales())
	}
}

func TestMemoryMoneyStore_AtomicCommitAndRollback(t *testing.T) {
	checkMoneyStoreAtomicity(t, memoryMoneyFixture(t))
}

func TestPGIntegration_SQLMoneyStoreAtomicCommitAndRollback(t *testing.T) {
	checkMoneyStoreAtomicity(t, pgMoneyFixture(t))
}
