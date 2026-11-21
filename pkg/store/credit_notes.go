package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Credit-note jobs (MVP-23, migration 018): a refund or chargeback of a
// card sale enqueues one job in the same money transaction as its ledger
// movement; the credit-note worker (api.Server.ProcessCreditNotes) decides
// whether the sale was invoiced and issues the CFDI de egreso through the
// PAC port. One job per (merchant, sale, source kind, source id).

// Credit-note job sources, statuses and flags.
const (
	CreditSourceRefund     = "REFUND"
	CreditSourceChargeback = "CHARGEBACK"

	CreditJobPending     = "PENDING"
	CreditJobIssued      = "ISSUED"       // the credit-note invoice exists (its stamping follows the invoice lifecycle)
	CreditJobNotRequired = "NOT_REQUIRED" // the sale was not invoiced above the net amount

	// CreditFlagChargebackReversed: the chargeback was reversed (won) after
	// the job was created. Nothing is re-invoiced automatically; operators
	// review flagged ISSUED jobs (docs/CFDI.md).
	CreditFlagChargebackReversed = "CHARGEBACK_REVERSED"
)

// CreditNoteJob is one row of credit_note_jobs.
type CreditNoteJob struct {
	ID                  int64
	MerchantID          string
	SaleID              string
	SourceKind          string // REFUND | CHARGEBACK
	SourceID            string // provider refund id | chargeback id
	AmountCents         int64
	NetAfterCents       int64 // captured - refunded - charged back right after the movement
	Status              string
	CreditNoteInvoiceID string
	RelatedInvoiceID    string
	CreditedCents       int64
	Flag                string
	Attempts            int
	NextAttemptAt       time.Time
	LastError           string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// CreditNoteJobQuery filters jobs (oldest first by id).
type CreditNoteJobQuery struct {
	MerchantID string
	Statuses   []string
	Flagged    bool      // only jobs with a flag
	DueBefore  time.Time // zero = any next_attempt_at
	Limit      int
}

// CreditNoteJobRepository reads and updates credit-note jobs (outside the
// money transaction). SQLMoneyStore and MemoryMoneyStore implement it.
type CreditNoteJobRepository interface {
	ListCreditNoteJobs(ctx context.Context, q CreditNoteJobQuery) ([]CreditNoteJob, error)
	// UpdateCreditNoteJob writes status, invoice ids, credited amount,
	// attempts, next attempt and last error (the flag is kept).
	UpdateCreditNoteJob(ctx context.Context, job CreditNoteJob) error
}

var (
	_ CreditNoteJobRepository = (*SQLMoneyStore)(nil)
	_ CreditNoteJobRepository = (*MemoryMoneyStore)(nil)
)

func creditJobLimit(n int) int {
	if n <= 0 || n > 1000 {
		return 200
	}
	return n
}

const reversedBeforeIssue = "chargeback reversed before the credit note was issued"

// ----------------------------------------------------------------------------
// SQL
// ----------------------------------------------------------------------------

const creditJobColumns = `id, merchant_id, sale_id, source_kind, source_id, amount_cents, net_after_cents, status,
	credit_note_invoice_id, related_invoice_id, credited_cents, flag, attempts, next_attempt_at, last_error, created_at, updated_at`

func scanCreditJob(s rowScanner) (CreditNoteJob, error) {
	var j CreditNoteJob
	err := s.Scan(&j.ID, &j.MerchantID, &j.SaleID, &j.SourceKind, &j.SourceID, &j.AmountCents, &j.NetAfterCents, &j.Status,
		&j.CreditNoteInvoiceID, &j.RelatedInvoiceID, &j.CreditedCents, &j.Flag, &j.Attempts, &j.NextAttemptAt, &j.LastError,
		&j.CreatedAt, &j.UpdatedAt)
	j.NextAttemptAt, j.CreatedAt, j.UpdatedAt = j.NextAttemptAt.UTC(), j.CreatedAt.UTC(), j.UpdatedAt.UTC()
	return j, err
}

// EnqueueCreditNoteJob stores a PENDING job in the money transaction
// (idempotent on its source).
func (t *sqlMoneyTx) EnqueueCreditNoteJob(ctx context.Context, j CreditNoteJob) error {
	_, err := t.tx.ExecContext(ctx, `INSERT INTO credit_note_jobs (merchant_id, sale_id, source_kind, source_id, amount_cents, net_after_cents)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT ON CONSTRAINT credit_note_jobs_source DO NOTHING`,
		j.MerchantID, j.SaleID, j.SourceKind, j.SourceID, j.AmountCents, j.NetAfterCents)
	if err != nil {
		return fmt.Errorf("enqueue credit note job: %w", err)
	}
	return nil
}

// FlagChargebackReversed flags the chargeback's job; a still PENDING one
// becomes NOT_REQUIRED (the funds came back before any egreso).
func (t *sqlMoneyTx) FlagChargebackReversed(ctx context.Context, merchantID, saleID, chargebackID string) (CreditNoteJob, bool, error) {
	j, err := scanCreditJob(t.tx.QueryRowContext(ctx, `UPDATE credit_note_jobs SET flag = $5,
		status = CASE WHEN status = 'PENDING' THEN 'NOT_REQUIRED' ELSE status END,
		last_error = CASE WHEN status = 'PENDING' THEN $6 ELSE last_error END, updated_at = NOW()
		WHERE merchant_id = $1 AND sale_id = $2 AND source_kind = $3 AND source_id = $4 RETURNING `+creditJobColumns,
		merchantID, saleID, CreditSourceChargeback, chargebackID, CreditFlagChargebackReversed, reversedBeforeIssue))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return CreditNoteJob{}, false, nil
		}
		return CreditNoteJob{}, false, fmt.Errorf("flag credit note job: %w", err)
	}
	return j, true, nil
}

func (s *SQLMoneyStore) ListCreditNoteJobs(ctx context.Context, q CreditNoteJobQuery) ([]CreditNoteJob, error) {
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	conds := []string{"TRUE"}
	if q.MerchantID != "" {
		conds = append(conds, "merchant_id = "+arg(q.MerchantID))
	}
	if len(q.Statuses) > 0 {
		conds = append(conds, "status = ANY("+arg(q.Statuses)+")")
	}
	if q.Flagged {
		conds = append(conds, "flag <> ''")
	}
	if !q.DueBefore.IsZero() {
		conds = append(conds, "next_attempt_at <= "+arg(q.DueBefore.UTC()))
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+creditJobColumns+` FROM credit_note_jobs WHERE `+strings.Join(conds, " AND ")+
		` ORDER BY id LIMIT `+arg(creditJobLimit(q.Limit)), args...)
	if err != nil {
		return nil, fmt.Errorf("list credit note jobs: %w", err)
	}
	defer rows.Close()
	var out []CreditNoteJob
	for rows.Next() {
		j, err := scanCreditJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *SQLMoneyStore) UpdateCreditNoteJob(ctx context.Context, j CreditNoteJob) error {
	res, err := s.db.ExecContext(ctx, `UPDATE credit_note_jobs SET status = $2, credit_note_invoice_id = $3, related_invoice_id = $4,
		credited_cents = $5, attempts = $6, next_attempt_at = $7, last_error = $8, updated_at = NOW() WHERE id = $1`,
		j.ID, j.Status, j.CreditNoteInvoiceID, j.RelatedInvoiceID, j.CreditedCents, j.Attempts, j.NextAttemptAt.UTC(), j.LastError)
	if err != nil {
		return fmt.Errorf("update credit note job: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("credit note job %d not found", j.ID)
	}
	return nil
}

// ----------------------------------------------------------------------------
// Memory
// ----------------------------------------------------------------------------

func creditJobKey(j CreditNoteJob) string {
	return j.MerchantID + "\x00" + j.SaleID + "\x00" + j.SourceKind + "\x00" + j.SourceID
}

func (t *memMoneyTx) EnqueueCreditNoteJob(_ context.Context, j CreditNoteJob) error {
	t.creditJobs = append(t.creditJobs, j)
	return nil
}

func (t *memMoneyTx) FlagChargebackReversed(_ context.Context, merchantID, saleID, chargebackID string) (CreditNoteJob, bool, error) {
	key := creditJobKey(CreditNoteJob{MerchantID: merchantID, SaleID: saleID, SourceKind: CreditSourceChargeback, SourceID: chargebackID})
	for i, j := range t.store.creditJobs {
		if creditJobKey(j) == key {
			if j.Status == CreditJobPending {
				j.Status, j.LastError = CreditJobNotRequired, reversedBeforeIssue
			}
			j.Flag, j.UpdatedAt = CreditFlagChargebackReversed, time.Now().UTC()
			t.creditFlags = append(t.creditFlags, memCreditUpdate{index: i, job: j})
			return j, true, nil
		}
	}
	return CreditNoteJob{}, false, nil
}

type memCreditUpdate struct {
	index int
	job   CreditNoteJob
}

func (t *memMoneyTx) commitCreditJobs() {
	for _, u := range t.creditFlags {
		t.store.creditJobs[u.index] = u.job
	}
	seen := map[string]bool{}
	for _, j := range t.store.creditJobs {
		seen[creditJobKey(j)] = true
	}
	now := time.Now().UTC()
	for _, j := range t.creditJobs {
		if seen[creditJobKey(j)] {
			continue
		}
		seen[creditJobKey(j)] = true
		t.store.creditSeq++
		j.ID, j.Status, j.NextAttemptAt, j.CreatedAt, j.UpdatedAt = t.store.creditSeq, CreditJobPending, now, now, now
		t.store.creditJobs = append(t.store.creditJobs, j)
	}
}

func (s *MemoryMoneyStore) ListCreditNoteJobs(_ context.Context, q CreditNoteJobQuery) ([]CreditNoteJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []CreditNoteJob
	for _, j := range s.creditJobs {
		if (q.MerchantID != "" && j.MerchantID != q.MerchantID) || (len(q.Statuses) > 0 && !containsStr(q.Statuses, j.Status)) ||
			(q.Flagged && j.Flag == "") || (!q.DueBefore.IsZero() && j.NextAttemptAt.After(q.DueBefore)) {
			continue
		}
		out = append(out, j)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].ID < out[k].ID })
	if l := creditJobLimit(q.Limit); len(out) > l {
		out = out[:l]
	}
	return out, nil
}

func (s *MemoryMoneyStore) UpdateCreditNoteJob(_ context.Context, j CreditNoteJob) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, cur := range s.creditJobs {
		if cur.ID == j.ID {
			j.Flag, j.MerchantID, j.SaleID, j.SourceKind, j.SourceID = cur.Flag, cur.MerchantID, cur.SaleID, cur.SourceKind, cur.SourceID
			j.AmountCents, j.NetAfterCents, j.CreatedAt, j.UpdatedAt = cur.AmountCents, cur.NetAfterCents, cur.CreatedAt, time.Now().UTC()
			s.creditJobs[i] = j
			return nil
		}
	}
	return fmt.Errorf("credit note job %d not found", j.ID)
}

// SetCreditNoteJobCreatedAt backdates a job (tests of the worker's grace period).
func (s *MemoryMoneyStore) SetCreditNoteJobCreatedAt(id int64, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.creditJobs {
		if s.creditJobs[i].ID == id {
			s.creditJobs[i].CreatedAt = at
		}
	}
}
