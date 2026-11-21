package store

import (
	"context"
	"errors"
	"testing"
)

type creditJobStore interface {
	MoneyStore
	CreditNoteJobRepository
}

// checkCreditJobResolvedConcurrently: a worker holding a stale PENDING
// snapshot must not resurrect a job that a chargeback reversal resolved
// meanwhile (NOT_REQUIRED + flag); writing ISSUED (the egreso already
// exists) still wins and reports the flag so the worker can alert.
func checkCreditJobResolvedConcurrently(t *testing.T, ms creditJobStore, merchant string) {
	t.Helper()
	ctx := context.Background()
	for _, src := range []string{"cb_wait", "cb_issue"} {
		if err := ms.InTx(ctx, func(tx MoneyTx) error {
			return tx.EnqueueCreditNoteJob(ctx, CreditNoteJob{MerchantID: merchant, SaleID: "sale_race", SourceKind: CreditSourceChargeback,
				SourceID: src, AmountCents: 100, NetAfterCents: 0})
		}); err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := ms.ListCreditNoteJobs(ctx, CreditNoteJobQuery{MerchantID: merchant})
	if err != nil || len(jobs) != 2 {
		t.Fatalf("jobs: %v %+v", err, jobs)
	}
	for _, j := range jobs { // the reversal lands while the worker holds its snapshot
		if err := ms.InTx(ctx, func(tx MoneyTx) error {
			_, _, err := tx.FlagChargebackReversed(ctx, merchant, "sale_race", j.SourceID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	waitSnap, issueSnap := jobs[0], jobs[1]
	waitSnap.Attempts, waitSnap.LastError = 1, "INVOICE_NOT_STAMPED: PENDING"
	if _, err := ms.UpdateCreditNoteJob(ctx, waitSnap); !errors.Is(err, ErrCreditJobResolved) {
		t.Fatalf("stale PENDING write: %v", err)
	}
	issueSnap.Status, issueSnap.CreditNoteInvoiceID, issueSnap.CreditedCents = CreditJobIssued, "inv_cn", 100
	out, err := ms.UpdateCreditNoteJob(ctx, issueSnap)
	if err != nil || out.Status != CreditJobIssued || out.Flag != CreditFlagChargebackReversed {
		t.Fatalf("ISSUED write: %v %+v", err, out)
	}
	after, _ := ms.ListCreditNoteJobs(ctx, CreditNoteJobQuery{MerchantID: merchant, Statuses: []string{CreditJobPending}})
	if len(after) != 0 {
		t.Fatalf("a reversed job came back to PENDING: %+v", after)
	}
}

func TestCreditNoteJobs_ResolvedConcurrently_Memory(t *testing.T) {
	checkCreditJobResolvedConcurrently(t, memoryMoneyFixture(t).ms.(*MemoryMoneyStore), fixMerchant)
}
