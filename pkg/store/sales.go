package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Sale kinds.
const (
	SaleCard = "card"
	SaleSPEI = "spei"
	SaleCoDi = "codi"
)

// SaleRecord is one row of the unified sales view: card payments
// (card_payments) plus credited SPEI abonos and CoDi confirmations
// (merchant_sales).
type SaleRecord struct {
	ID            string
	MerchantID    string
	Kind          string // card | spei | codi
	Status        string // card: card_payments.status; spei: RECEIVED; codi: CONFIRMED
	AmountCents   int64
	Reference     string
	CardPaymentID string // card sales only
	CreatedAt     time.Time
}

// SalesCursor positions a page: items strictly older than (CreatedAt, ID).
type SalesCursor struct {
	CreatedAt time.Time
	ID        string
}

// before reports whether (t, id) sorts after the cursor in newest-first order.
func (c *SalesCursor) admits(t time.Time, id string) bool {
	if c == nil {
		return true
	}
	return t.Before(c.CreatedAt) || (t.Equal(c.CreatedAt) && id < c.ID)
}

// SalesQuery filters the sales view (newest first, stable by (created_at, id)).
type SalesQuery struct {
	MerchantID string // "" = every merchant (backoffice)
	ID         string // exact sale / card payment id
	Kind       string // "" = all kinds
	From, To   time.Time
	Before     *SalesCursor
	Limit      int
}

// SalesRepository lists sales. SQLMoneyStore and MemoryMoneyStore implement it.
type SalesRepository interface {
	ListSales(ctx context.Context, q SalesQuery) ([]SaleRecord, error)
}

var (
	_ SalesRepository = (*SQLMoneyStore)(nil)
	_ SalesRepository = (*MemoryMoneyStore)(nil)
)

func salesLimit(n int) int {
	if n <= 0 || n > 1000 {
		return 50
	}
	return n
}

// RecordSale stores a credited SPEI / CoDi sale in the money transaction
// (idempotent on the sale id).
func (t *sqlMoneyTx) RecordSale(ctx context.Context, s SaleRecord) error {
	_, err := t.tx.ExecContext(ctx, `
		INSERT INTO merchant_sales (id, merchant_id, kind, status, amount_cents, reference, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (id) DO NOTHING`,
		s.ID, s.MerchantID, s.Kind, s.Status, s.AmountCents, s.Reference, s.CreatedAt.UTC())
	if err != nil {
		return fmt.Errorf("record sale: %w", err)
	}
	return nil
}

// ListSales merges card_payments and merchant_sales in SQL.
func (s *SQLMoneyStore) ListSales(ctx context.Context, q SalesQuery) ([]SaleRecord, error) {
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	var conds []string
	if q.MerchantID != "" {
		conds = append(conds, "merchant_id = "+arg(q.MerchantID))
	}
	if q.ID != "" {
		conds = append(conds, "id = "+arg(q.ID))
	}
	if !q.From.IsZero() {
		conds = append(conds, "created_at >= "+arg(q.From.UTC()))
	}
	if !q.To.IsZero() {
		conds = append(conds, "created_at < "+arg(q.To.UTC()))
	}
	if q.Before != nil {
		conds = append(conds, fmt.Sprintf("(created_at, id) < (%s, %s)", arg(q.Before.CreatedAt.UTC()), arg(q.Before.ID)))
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	var parts []string
	if q.Kind == "" || q.Kind == SaleCard {
		parts = append(parts, `SELECT id, merchant_id, 'card' AS kind, status, amount_cents, reference, id AS card_payment_id, created_at FROM card_payments`+where)
	}
	if q.Kind != SaleCard {
		w := where
		if q.Kind != "" {
			if w == "" {
				w = " WHERE "
			} else {
				w += " AND "
			}
			w += "kind = " + arg(q.Kind)
		}
		parts = append(parts, `SELECT id, merchant_id, kind, status, amount_cents, reference, '' AS card_payment_id, created_at FROM merchant_sales`+w)
	}
	query := `SELECT id, merchant_id, kind, status, amount_cents, reference, card_payment_id, created_at FROM (` +
		strings.Join(parts, " UNION ALL ") + `) s ORDER BY created_at DESC, id DESC LIMIT ` + arg(salesLimit(q.Limit))
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list sales: %w", err)
	}
	defer rows.Close()
	var out []SaleRecord
	for rows.Next() {
		var r SaleRecord
		if err := rows.Scan(&r.ID, &r.MerchantID, &r.Kind, &r.Status, &r.AmountCents, &r.Reference, &r.CardPaymentID, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.CreatedAt = r.CreatedAt.UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecordSale buffers the sale until the unit of work commits.
func (t *memMoneyTx) RecordSale(_ context.Context, s SaleRecord) error {
	t.sales = append(t.sales, s)
	return nil
}

// ListSales implements SalesRepository over the committed in-memory state.
func (s *MemoryMoneyStore) ListSales(_ context.Context, q SalesQuery) ([]SaleRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []SaleRecord
	if q.Kind == "" || q.Kind == SaleCard {
		for _, cp := range s.card.payments {
			all = append(all, SaleRecord{ID: cp.ID, MerchantID: cp.MerchantID, Kind: SaleCard, Status: string(cp.Status),
				AmountCents: cp.AmountCents, Reference: cp.Reference, CardPaymentID: cp.ID, CreatedAt: cp.CreatedAt})
		}
	}
	if q.Kind != SaleCard {
		for _, sr := range s.sales {
			if q.Kind == "" || sr.Kind == q.Kind {
				all = append(all, sr)
			}
		}
	}
	var out []SaleRecord
	for _, r := range all {
		if (q.MerchantID != "" && r.MerchantID != q.MerchantID) || (q.ID != "" && r.ID != q.ID) ||
			(!q.From.IsZero() && r.CreatedAt.Before(q.From)) || (!q.To.IsZero() && !r.CreatedAt.Before(q.To)) ||
			!q.Before.admits(r.CreatedAt, r.ID) {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	if l := salesLimit(q.Limit); len(out) > l {
		out = out[:l]
	}
	return out, nil
}
