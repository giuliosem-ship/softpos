package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"softpos/pkg/ledger"
)

// ----------------------------------------------------------------------------
// Operator audit log (operator_audit_log, migration 014)
// ----------------------------------------------------------------------------

// AuditEntry is one operator action: who, what, on which merchant / object,
// when, and the state before and after (JSON documents, may be nil).
type AuditEntry struct {
	ID         int64
	OperatorID string
	Action     string // e.g. kyb.approve, merchant.block, document.download
	MerchantID string
	Target     string // object id (document, payout, reconciliation item)
	Before     json.RawMessage
	After      json.RawMessage
	Detail     string
	CreatedAt  time.Time
}

// AuditQuery filters the audit listing (newest first).
type AuditQuery struct {
	MerchantID string
	Action     string
	Limit      int
}

// OperatorAuditRepository appends and lists operator actions. Rows are never
// updated or deleted by the application.
type OperatorAuditRepository interface {
	AppendAudit(ctx context.Context, e AuditEntry) error
	ListAudit(ctx context.Context, q AuditQuery) ([]AuditEntry, error)
}

var (
	_ OperatorAuditRepository = (*SQLAuditRepository)(nil)
	_ OperatorAuditRepository = (*MemoryAuditRepository)(nil)
)

func auditLimit(n int) int {
	if n <= 0 || n > 500 {
		return 100
	}
	return n
}

func jsonOrNull(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return []byte(b)
}

// SQLAuditRepository implements OperatorAuditRepository on PostgreSQL.
type SQLAuditRepository struct{ db *sql.DB }

func NewSQLAuditRepository(db *sql.DB) *SQLAuditRepository { return &SQLAuditRepository{db: db} }

func (r *SQLAuditRepository) AppendAudit(ctx context.Context, e AuditEntry) error {
	if e.OperatorID == "" || e.Action == "" {
		return errors.New("audit entry needs operator and action")
	}
	if _, err := r.db.ExecContext(ctx, `INSERT INTO operator_audit_log (operator_id, action, merchant_id, target, before_json, after_json, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, e.OperatorID, e.Action, e.MerchantID, e.Target, jsonOrNull(e.Before), jsonOrNull(e.After), e.Detail); err != nil {
		return fmt.Errorf("append operator audit: %w", err)
	}
	return nil
}

func (r *SQLAuditRepository) ListAudit(ctx context.Context, q AuditQuery) ([]AuditEntry, error) {
	var conds []string
	var args []any
	if q.MerchantID != "" {
		args = append(args, q.MerchantID)
		conds = append(conds, fmt.Sprintf("merchant_id = $%d", len(args)))
	}
	if q.Action != "" {
		args = append(args, q.Action)
		conds = append(conds, fmt.Sprintf("action = $%d", len(args)))
	}
	query := `SELECT id, operator_id, action, merchant_id, target, before_json, after_json, detail, created_at FROM operator_audit_log`
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, auditLimit(q.Limit))
	query += fmt.Sprintf(" ORDER BY id DESC LIMIT $%d", len(args))
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list operator audit: %w", err)
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var before, after []byte
		if err := rows.Scan(&e.ID, &e.OperatorID, &e.Action, &e.MerchantID, &e.Target, &before, &after, &e.Detail, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Before, e.After = before, after
		out = append(out, e)
	}
	return out, rows.Err()
}

// MemoryAuditRepository is the in-process OperatorAuditRepository.
type MemoryAuditRepository struct {
	mu   sync.Mutex
	rows []AuditEntry
}

func NewMemoryAuditRepository() *MemoryAuditRepository { return &MemoryAuditRepository{} }

func (m *MemoryAuditRepository) AppendAudit(_ context.Context, e AuditEntry) error {
	if e.OperatorID == "" || e.Action == "" {
		return errors.New("audit entry needs operator and action")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e.ID = int64(len(m.rows) + 1)
	e.CreatedAt = time.Now().UTC()
	m.rows = append(m.rows, e)
	return nil
}

func (m *MemoryAuditRepository) ListAudit(_ context.Context, q AuditQuery) ([]AuditEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []AuditEntry
	for i := len(m.rows) - 1; i >= 0 && len(out) < auditLimit(q.Limit); i-- {
		e := m.rows[i]
		if (q.MerchantID == "" || e.MerchantID == q.MerchantID) && (q.Action == "" || e.Action == q.Action) {
			out = append(out, e)
		}
	}
	return out, nil
}

// ----------------------------------------------------------------------------
// Payout queries (backoffice)
// ----------------------------------------------------------------------------

// PayoutQuery filters the payout listing (newest first).
type PayoutQuery struct {
	MerchantID string
	Status     PayoutStatus
	Limit      int
}

// PayoutQueryRepository lists and reads payouts outside a unit of work.
// SQLMoneyStore and MemoryMoneyStore implement it.
type PayoutQueryRepository interface {
	ListPayouts(ctx context.Context, q PayoutQuery) ([]PayoutRecord, error)
	GetPayout(ctx context.Context, claveRastreo string) (PayoutRecord, bool, error)
}

var (
	_ PayoutQueryRepository = (*SQLMoneyStore)(nil)
	_ PayoutQueryRepository = (*MemoryMoneyStore)(nil)
)

func (s *SQLMoneyStore) ListPayouts(ctx context.Context, q PayoutQuery) ([]PayoutRecord, error) {
	var conds []string
	var args []any
	if q.MerchantID != "" {
		args = append(args, q.MerchantID)
		conds = append(conds, fmt.Sprintf("merchant_id = $%d", len(args)))
	}
	if q.Status != "" {
		args = append(args, string(q.Status))
		conds = append(conds, fmt.Sprintf("status = $%d", len(args)))
	}
	query := `SELECT ` + payoutColumns + ` FROM payouts`
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, auditLimit(q.Limit))
	query += fmt.Sprintf(" ORDER BY created_at DESC, clave_rastreo DESC LIMIT $%d", len(args))
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list payouts: %w", err)
	}
	defer rows.Close()
	var out []PayoutRecord
	for rows.Next() {
		p, err := scanPayout(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *SQLMoneyStore) GetPayout(ctx context.Context, clave string) (PayoutRecord, bool, error) {
	p, err := scanPayout(s.db.QueryRowContext(ctx, `SELECT `+payoutColumns+` FROM payouts WHERE clave_rastreo = $1`, clave))
	if errors.Is(err, sql.ErrNoRows) {
		return PayoutRecord{}, false, nil
	}
	if err != nil {
		return PayoutRecord{}, false, fmt.Errorf("get payout: %w", err)
	}
	return p, true, nil
}

func (s *MemoryMoneyStore) ListPayouts(_ context.Context, q PayoutQuery) ([]PayoutRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []PayoutRecord
	for _, p := range s.payouts {
		if (q.MerchantID == "" || p.MerchantID == q.MerchantID) && (q.Status == "" || p.Status == q.Status) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ClaveRastreo > out[j].ClaveRastreo
	})
	if l := auditLimit(q.Limit); len(out) > l {
		out = out[:l]
	}
	return out, nil
}

func (s *MemoryMoneyStore) GetPayout(_ context.Context, clave string) (PayoutRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.payouts[clave]
	return p, ok, nil
}

// ----------------------------------------------------------------------------
// Journal lines per account (backoffice ledger view)
// ----------------------------------------------------------------------------

// JournalLine is one ledger entry with its journal header.
type JournalLine struct {
	JournalID   string
	Description string
	AccountID   string
	Direction   ledger.Direction
	AmountCents int64
	Metadata    map[string]string
	CreatedAt   time.Time
}

// JournalReader lists the entries posted to some accounts (newest first).
// SQLMoneyStore and MemoryMoneyStore implement it.
type JournalReader interface {
	ListAccountEntries(ctx context.Context, accountIDs []string, limit int) ([]JournalLine, error)
}

var (
	_ JournalReader = (*SQLMoneyStore)(nil)
	_ JournalReader = (*MemoryMoneyStore)(nil)
)

func (s *SQLMoneyStore) ListAccountEntries(ctx context.Context, accountIDs []string, limit int) ([]JournalLine, error) {
	if len(accountIDs) == 0 {
		return nil, nil
	}
	ph := make([]string, len(accountIDs))
	args := make([]any, 0, len(accountIDs)+1)
	for i, a := range accountIDs {
		args = append(args, a)
		ph[i] = fmt.Sprintf("$%d", i+1)
	}
	args = append(args, auditLimit(limit))
	rows, err := s.db.QueryContext(ctx, `
		SELECT j.id, j.description, j.metadata_json, e.account_id, e.direction, e.amount_cents, j.created_at
		FROM ledger_entries e JOIN ledger_journals j ON j.id = e.journal_id
		WHERE e.account_id IN (`+strings.Join(ph, ", ")+`)
		ORDER BY j.created_at DESC, e.id DESC
		LIMIT $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("list account entries: %w", err)
	}
	defer rows.Close()
	var out []JournalLine
	for rows.Next() {
		var l JournalLine
		var meta []byte
		var dir string
		if err := rows.Scan(&l.JournalID, &l.Description, &meta, &l.AccountID, &dir, &l.AmountCents, &l.CreatedAt); err != nil {
			return nil, err
		}
		l.Direction = ledger.Direction(dir)
		if len(meta) > 0 {
			_ = json.Unmarshal(meta, &l.Metadata)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *MemoryMoneyStore) ListAccountEntries(_ context.Context, accountIDs []string, limit int) ([]JournalLine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := map[string]bool{}
	for _, a := range accountIDs {
		want[a] = true
	}
	var out []JournalLine
	for i := len(s.journals) - 1; i >= 0 && len(out) < auditLimit(limit); i-- {
		j := s.journals[i]
		for k := len(j.Entries) - 1; k >= 0 && len(out) < auditLimit(limit); k-- {
			e := j.Entries[k]
			if want[e.AccountID] && e.Money.AmountCents != 0 {
				out = append(out, JournalLine{JournalID: j.ID, Description: j.Description, AccountID: e.AccountID,
					Direction: e.Direction, AmountCents: e.Money.AmountCents, Metadata: j.Metadata, CreatedAt: j.Timestamp})
			}
		}
	}
	return out, nil
}
