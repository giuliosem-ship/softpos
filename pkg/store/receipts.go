package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Sale receipts (MVP-26, migration 019): one public receipt per sale. The
// public token carries only the random receipt id and its expiry, signed by
// the API (api.Server receipt tokens); this table maps the id to the sale.

// ReceiptRecord is one row of sale_receipts.
type ReceiptRecord struct {
	ID         string // 24 hex chars (12 random bytes)
	MerchantID string
	SaleID     string
	ExpiresAt  time.Time // second precision
	InvoiceID  string    // invoice requested through the receipt (autofactura)
	CreatedAt  time.Time
}

// ReceiptRepository persists receipts.
type ReceiptRepository interface {
	// EnsureReceipt returns the sale's receipt, creating it with rec (id,
	// expiry) when missing; an expired one gets rec's expiry (a new token).
	EnsureReceipt(ctx context.Context, rec ReceiptRecord) (ReceiptRecord, error)
	GetReceipt(ctx context.Context, id string) (ReceiptRecord, bool, error)
	// SetReceiptInvoice records the autofactura invoice of a receipt.
	SetReceiptInvoice(ctx context.Context, id, invoiceID string) error
}

var (
	_ ReceiptRepository = (*SQLReceiptRepository)(nil)
	_ ReceiptRepository = (*MemoryReceiptRepository)(nil)
)

// SQLReceiptRepository implements ReceiptRepository on PostgreSQL.
type SQLReceiptRepository struct{ db *sql.DB }

func NewSQLReceiptRepository(db *sql.DB) *SQLReceiptRepository { return &SQLReceiptRepository{db: db} }

const receiptColumns = `id, merchant_id, sale_id, expires_at, invoice_id, created_at`

func scanReceipt(s rowScanner) (ReceiptRecord, error) {
	var r ReceiptRecord
	err := s.Scan(&r.ID, &r.MerchantID, &r.SaleID, &r.ExpiresAt, &r.InvoiceID, &r.CreatedAt)
	r.ExpiresAt, r.CreatedAt = r.ExpiresAt.UTC(), r.CreatedAt.UTC()
	return r, err
}

func (r *SQLReceiptRepository) EnsureReceipt(ctx context.Context, rec ReceiptRecord) (ReceiptRecord, error) {
	out, err := scanReceipt(r.db.QueryRowContext(ctx, `INSERT INTO sale_receipts (id, merchant_id, sale_id, expires_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT ON CONSTRAINT sale_receipts_one_per_sale DO UPDATE SET expires_at =
			CASE WHEN sale_receipts.expires_at <= NOW() THEN EXCLUDED.expires_at ELSE sale_receipts.expires_at END
		RETURNING `+receiptColumns, rec.ID, rec.MerchantID, rec.SaleID, rec.ExpiresAt.UTC().Truncate(time.Second)))
	if err != nil {
		return ReceiptRecord{}, fmt.Errorf("ensure receipt: %w", err)
	}
	return out, nil
}

func (r *SQLReceiptRepository) GetReceipt(ctx context.Context, id string) (ReceiptRecord, bool, error) {
	out, err := scanReceipt(r.db.QueryRowContext(ctx, `SELECT `+receiptColumns+` FROM sale_receipts WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ReceiptRecord{}, false, nil
	}
	if err != nil {
		return ReceiptRecord{}, false, fmt.Errorf("get receipt: %w", err)
	}
	return out, true, nil
}

func (r *SQLReceiptRepository) SetReceiptInvoice(ctx context.Context, id, invoiceID string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE sale_receipts SET invoice_id = $2 WHERE id = $1`, id, invoiceID)
	return err
}

// MemoryReceiptRepository is the in-memory ReceiptRepository.
type MemoryReceiptRepository struct {
	mu   sync.Mutex
	byID map[string]ReceiptRecord
}

func NewMemoryReceiptRepository() *MemoryReceiptRepository {
	return &MemoryReceiptRepository{byID: map[string]ReceiptRecord{}}
}

func (m *MemoryReceiptRepository) EnsureReceipt(_ context.Context, rec ReceiptRecord) (ReceiptRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, r := range m.byID {
		if r.MerchantID == rec.MerchantID && r.SaleID == rec.SaleID {
			if !r.ExpiresAt.After(time.Now()) {
				r.ExpiresAt = rec.ExpiresAt.UTC().Truncate(time.Second)
				m.byID[id] = r
			}
			return r, nil
		}
	}
	rec.ExpiresAt, rec.CreatedAt = rec.ExpiresAt.UTC().Truncate(time.Second), time.Now().UTC()
	m.byID[rec.ID] = rec
	return rec, nil
}

func (m *MemoryReceiptRepository) GetReceipt(_ context.Context, id string) (ReceiptRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.byID[id]
	return r, ok, nil
}

func (m *MemoryReceiptRepository) SetReceiptInvoice(_ context.Context, id, invoiceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.byID[id]; ok {
		r.InvoiceID = invoiceID
		m.byID[id] = r
	}
	return nil
}

// ExpireReceipt backdates a receipt's expiry (tests).
func (m *MemoryReceiptRepository) ExpireReceipt(id string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.byID[id]; ok {
		r.ExpiresAt = at.UTC().Truncate(time.Second)
		m.byID[id] = r
	}
}
