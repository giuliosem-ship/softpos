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
)

// CFDI invoices (MVP-22, migration 017). An invoice links the sales it
// covers; a sale is in at most one *active* link (unique partial index), so a
// sale can never be invoiced twice. Links are released when the invoice is
// CANCELED or FAILED and transferred to a replacement (motivo 01).

// Invoice kinds and statuses.
const (
	InvoiceIndividual = "INDIVIDUAL"
	InvoiceGlobal     = "GLOBAL"

	InvoicePending         = "PENDING"
	InvoiceStamped         = "STAMPED"
	InvoiceFailed          = "FAILED"
	InvoiceCancelRequested = "CANCEL_REQUESTED"
	InvoiceCanceled        = "CANCELED"

	CancelRequested = "REQUESTED"
	CancelAccepted  = "ACCEPTED"
	CancelRejected  = "REJECTED"
)

var (
	// ErrInvoiceExists: the merchant already used the idempotency key (the
	// existing invoice is returned with it).
	ErrInvoiceExists = errors.New("invoice idempotency key already used")
	// ErrGlobalInvoiceExists: a live factura global already covers the period.
	ErrGlobalInvoiceExists = errors.New("a live global invoice exists for the period")
	// ErrInvoiceConflict: the invoice is not in the expected status.
	ErrInvoiceConflict = errors.New("invoice changed concurrently")
	// ErrInvoiceNotFound: no such invoice.
	ErrInvoiceNotFound = errors.New("invoice not found")
)

// SaleInvoicedError: SaleID is already in active invoice InvoiceID.
type SaleInvoicedError struct{ SaleID, InvoiceID string }

func (e *SaleInvoicedError) Error() string {
	return fmt.Sprintf("sale %s already in invoice %s", e.SaleID, e.InvoiceID)
}

// InvoiceRecord is one invoice.
type InvoiceRecord struct {
	ID                     string
	MerchantID             string
	Kind                   string
	Status                 string
	IdempotencyKey         string
	RequestHash            string
	Serie                  string
	Folio                  int64
	UUID                   string
	SubtotalCents          int64
	IVACents               int64
	ISRRetenidoCents       int64
	TotalCents             int64
	FormaPago              string
	Receptor               json.RawMessage
	GlobalPeriod           string
	ReplacesInvoiceID      string
	XML                    []byte
	SealedAt               *time.Time
	ErrorCode              string
	ErrorDetail            string
	StampAttempts          int
	StampLeaseUntil        *time.Time
	CancelMotivo           string
	CancelFolioSustitucion string
	CancelStatus           string
	CancelRequestedAt      *time.Time
	CancelResolvedAt       *time.Time
	EmailedAt              *time.Time
	CreatedAt              time.Time
	StampedAt              *time.Time
	UpdatedAt              time.Time
	SaleIDs                []string // ordered (position)
}

// Live reports whether the invoice holds its sales (not FAILED/CANCELED).
func (r InvoiceRecord) Live() bool {
	return r.Status == InvoicePending || r.Status == InvoiceStamped || r.Status == InvoiceCancelRequested
}

// InvoiceSaleLine is one sale of an invoice with its amounts.
type InvoiceSaleLine struct {
	SaleID    string
	BaseCents int64
	IVACents  int64
}

// LinkOp is what UpdateInvoice does with the invoice's sale links.
type LinkOp int

const (
	LinksKeep LinkOp = iota
	// LinksRelease frees the sales (CANCELED / FAILED); when the invoice
	// replaced another one, that one's links come back if it is still live.
	LinksRelease
)

// InvoiceQuery filters invoices (newest first by (created_at, id)).
type InvoiceQuery struct {
	MerchantID string   // "" = every merchant (backoffice / workers)
	Statuses   []string // empty = any
	Kind       string
	Before     *SalesCursor
	Limit      int
}

// InvoiceRepository persists invoices.
type InvoiceRepository interface {
	// CreateInvoice allocates the next folio of rec.Serie, inserts rec and
	// links lines in one transaction (rec.StampLeaseUntil is the creator's
	// stamping lease). ErrInvoiceExists returns the existing invoice;
	// *SaleInvoicedError and ErrGlobalInvoiceExists abort. With
	// rec.ReplacesInvoiceID the replaced invoice's links of the same sales
	// are transferred.
	CreateInvoice(ctx context.Context, rec InvoiceRecord, lines []InvoiceSaleLine) (InvoiceRecord, error)
	// GetInvoice loads one invoice (merchantID "" = any merchant).
	GetInvoice(ctx context.Context, merchantID, id string) (InvoiceRecord, bool, error)
	// FindInvoiceByUUID loads the merchant's invoice with that UUID.
	FindInvoiceByUUID(ctx context.Context, merchantID, uuid string) (InvoiceRecord, bool, error)
	// FindInvoiceByIdempotencyKey loads the merchant's invoice created with key.
	FindInvoiceByIdempotencyKey(ctx context.Context, merchantID, key string) (InvoiceRecord, bool, error)
	ListInvoices(ctx context.Context, q InvoiceQuery) ([]InvoiceRecord, error)
	InvoiceLines(ctx context.Context, invoiceID string) ([]InvoiceSaleLine, error)
	// ActiveInvoicesForSales maps each sale with an active link to its invoice.
	ActiveInvoicesForSales(ctx context.Context, merchantID string, saleIDs []string) (map[string]string, error)
	// ClaimStamping takes the stamping lease of a PENDING or FAILED invoice
	// whose lease expired (attempts+1). A FAILED one goes back to PENDING
	// and re-links its sales (*SaleInvoicedError if one was taken).
	// claimed=false: not retryable now (status / lease).
	ClaimStamping(ctx context.Context, merchantID, id string, until time.Time) (InvoiceRecord, bool, error)
	// UpdateInvoice writes the mutable columns of rec if the stored status
	// is expect (ErrInvoiceConflict otherwise) and applies op to its links.
	UpdateInvoice(ctx context.Context, rec InvoiceRecord, expect string, op LinkOp) (InvoiceRecord, error)
}

var (
	_ InvoiceRepository = (*SQLInvoiceRepository)(nil)
	_ InvoiceRepository = (*MemoryInvoiceRepository)(nil)
)

func invoiceLimit(n int) int {
	if n <= 0 {
		return 50
	}
	return min(n, 1000)
}

// ----------------------------------------------------------------------------
// SQL implementation
// ----------------------------------------------------------------------------

// SQLInvoiceRepository implements InvoiceRepository on PostgreSQL.
type SQLInvoiceRepository struct{ db *sql.DB }

func NewSQLInvoiceRepository(db *sql.DB) *SQLInvoiceRepository { return &SQLInvoiceRepository{db: db} }

const invoiceColumns = `id, merchant_id, kind, status, idempotency_key, request_hash, serie, folio, COALESCE(uuid, ''),
	subtotal_cents, iva_cents, isr_retenido_cents, total_cents, forma_pago, receptor_json, global_period, replaces_invoice_id,
	xml, sealed_at, error_code, error_detail, stamp_attempts, stamp_lease_until, cancel_motivo, cancel_folio_sustitucion,
	cancel_status, cancel_requested_at, cancel_resolved_at, emailed_at, created_at, stamped_at, updated_at`

func utcPtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	u := t.Time.UTC()
	return &u
}

func scanInvoice(s rowScanner) (InvoiceRecord, error) {
	var r InvoiceRecord
	var receptor []byte
	var sealed, lease, creq, cres, emailed, stamped sql.NullTime
	err := s.Scan(&r.ID, &r.MerchantID, &r.Kind, &r.Status, &r.IdempotencyKey, &r.RequestHash, &r.Serie, &r.Folio, &r.UUID,
		&r.SubtotalCents, &r.IVACents, &r.ISRRetenidoCents, &r.TotalCents, &r.FormaPago, &receptor, &r.GlobalPeriod, &r.ReplacesInvoiceID,
		&r.XML, &sealed, &r.ErrorCode, &r.ErrorDetail, &r.StampAttempts, &lease, &r.CancelMotivo, &r.CancelFolioSustitucion,
		&r.CancelStatus, &creq, &cres, &emailed, &r.CreatedAt, &stamped, &r.UpdatedAt)
	if err != nil {
		return r, err
	}
	r.Receptor = json.RawMessage(receptor)
	r.SealedAt, r.StampLeaseUntil, r.CancelRequestedAt, r.CancelResolvedAt, r.EmailedAt, r.StampedAt =
		utcPtr(sealed), utcPtr(lease), utcPtr(creq), utcPtr(cres), utcPtr(emailed), utcPtr(stamped)
	r.CreatedAt, r.UpdatedAt = r.CreatedAt.UTC(), r.UpdatedAt.UTC()
	return r, nil
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func loadSaleIDs(ctx context.Context, q queryer, recs []InvoiceRecord) error {
	for i := range recs {
		rows, err := q.QueryContext(ctx, `SELECT sale_id FROM invoice_sales WHERE invoice_id = $1 ORDER BY position`, recs[i].ID)
		if err != nil {
			return err
		}
		recs[i].SaleIDs = nil
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			recs[i].SaleIDs = append(recs[i].SaleIDs, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

func getInvoice(ctx context.Context, q queryer, where string, args ...any) (InvoiceRecord, bool, error) {
	rec, err := scanInvoice(q.QueryRowContext(ctx, `SELECT `+invoiceColumns+` FROM invoices WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return InvoiceRecord{}, false, nil
	}
	if err != nil {
		return InvoiceRecord{}, false, fmt.Errorf("get invoice: %w", err)
	}
	recs := []InvoiceRecord{rec}
	if err := loadSaleIDs(ctx, q, recs); err != nil {
		return InvoiceRecord{}, false, err
	}
	return recs[0], true, nil
}

func (r *SQLInvoiceRepository) CreateInvoice(ctx context.Context, rec InvoiceRecord, lines []InvoiceSaleLine) (InvoiceRecord, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return InvoiceRecord{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if existing, found, err := getInvoice(ctx, tx, `merchant_id = $1 AND idempotency_key = $2`, rec.MerchantID, rec.IdempotencyKey); err != nil {
		return InvoiceRecord{}, err
	} else if found {
		return existing, ErrInvoiceExists
	}
	if err := tx.QueryRowContext(ctx, `INSERT INTO invoice_folios (merchant_id, serie, last_folio) VALUES ($1, $2, 1)
		ON CONFLICT (merchant_id, serie) DO UPDATE SET last_folio = invoice_folios.last_folio + 1 RETURNING last_folio`,
		rec.MerchantID, rec.Serie).Scan(&rec.Folio); err != nil {
		return InvoiceRecord{}, fmt.Errorf("allocate folio: %w", err)
	}
	now := time.Now().UTC()
	rec.CreatedAt, rec.UpdatedAt = now, now
	res, err := tx.ExecContext(ctx, `INSERT INTO invoices (id, merchant_id, kind, status, idempotency_key, request_hash, serie, folio,
		subtotal_cents, iva_cents, isr_retenido_cents, total_cents, forma_pago, receptor_json, global_period, replaces_invoice_id,
		stamp_attempts, stamp_lease_until, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$19) ON CONFLICT DO NOTHING`,
		rec.ID, rec.MerchantID, rec.Kind, rec.Status, rec.IdempotencyKey, rec.RequestHash, rec.Serie, rec.Folio,
		rec.SubtotalCents, rec.IVACents, rec.ISRRetenidoCents, rec.TotalCents, rec.FormaPago, []byte(rec.Receptor), rec.GlobalPeriod,
		rec.ReplacesInvoiceID, rec.StampAttempts, rec.StampLeaseUntil, now)
	if err != nil {
		return InvoiceRecord{}, fmt.Errorf("insert invoice: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		_ = tx.Rollback()
		if existing, found, err := getInvoice(ctx, r.db, `merchant_id = $1 AND idempotency_key = $2`, rec.MerchantID, rec.IdempotencyKey); err == nil && found {
			return existing, ErrInvoiceExists
		}
		return InvoiceRecord{}, ErrGlobalInvoiceExists
	}
	if rec.ReplacesInvoiceID != "" {
		ids := make([]string, len(lines))
		for i, l := range lines {
			ids[i] = l.SaleID
		}
		if _, err := tx.ExecContext(ctx, `UPDATE invoice_sales SET active = FALSE WHERE invoice_id = $1 AND sale_id = ANY($2)`,
			rec.ReplacesInvoiceID, ids); err != nil {
			return InvoiceRecord{}, fmt.Errorf("transfer sales: %w", err)
		}
	}
	for i, l := range lines {
		res, err := tx.ExecContext(ctx, `INSERT INTO invoice_sales (invoice_id, merchant_id, sale_id, position, base_cents, iva_cents, active)
			VALUES ($1, $2, $3, $4, $5, $6, TRUE) ON CONFLICT DO NOTHING`, rec.ID, rec.MerchantID, l.SaleID, i, l.BaseCents, l.IVACents)
		if err != nil {
			return InvoiceRecord{}, fmt.Errorf("link sale: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var holder string
			_ = tx.QueryRowContext(ctx, `SELECT invoice_id FROM invoice_sales WHERE merchant_id = $1 AND sale_id = $2 AND active`,
				rec.MerchantID, l.SaleID).Scan(&holder)
			return InvoiceRecord{}, &SaleInvoicedError{SaleID: l.SaleID, InvoiceID: holder}
		}
		rec.SaleIDs = append(rec.SaleIDs, l.SaleID)
	}
	if err := tx.Commit(); err != nil {
		return InvoiceRecord{}, fmt.Errorf("commit invoice: %w", err)
	}
	return rec, nil
}

func (r *SQLInvoiceRepository) GetInvoice(ctx context.Context, merchantID, id string) (InvoiceRecord, bool, error) {
	if merchantID == "" {
		return getInvoice(ctx, r.db, `id = $1`, id)
	}
	return getInvoice(ctx, r.db, `id = $1 AND merchant_id = $2`, id, merchantID)
}

func (r *SQLInvoiceRepository) FindInvoiceByUUID(ctx context.Context, merchantID, uuid string) (InvoiceRecord, bool, error) {
	return getInvoice(ctx, r.db, `uuid = $1 AND merchant_id = $2`, strings.ToUpper(uuid), merchantID)
}

func (r *SQLInvoiceRepository) FindInvoiceByIdempotencyKey(ctx context.Context, merchantID, key string) (InvoiceRecord, bool, error) {
	return getInvoice(ctx, r.db, `merchant_id = $1 AND idempotency_key = $2`, merchantID, key)
}

func (r *SQLInvoiceRepository) ListInvoices(ctx context.Context, q InvoiceQuery) ([]InvoiceRecord, error) {
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
	if q.Kind != "" {
		conds = append(conds, "kind = "+arg(q.Kind))
	}
	if q.Before != nil {
		conds = append(conds, fmt.Sprintf("(created_at, id) < (%s, %s)", arg(q.Before.CreatedAt.UTC()), arg(q.Before.ID)))
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+invoiceColumns+` FROM invoices WHERE `+strings.Join(conds, " AND ")+
		` ORDER BY created_at DESC, id DESC LIMIT `+arg(invoiceLimit(q.Limit)), args...)
	if err != nil {
		return nil, fmt.Errorf("list invoices: %w", err)
	}
	var out []InvoiceRecord
	for rows.Next() {
		rec, err := scanInvoice(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, rec)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, loadSaleIDs(ctx, r.db, out)
}

func (r *SQLInvoiceRepository) InvoiceLines(ctx context.Context, invoiceID string) ([]InvoiceSaleLine, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT sale_id, base_cents, iva_cents FROM invoice_sales WHERE invoice_id = $1 ORDER BY position`, invoiceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InvoiceSaleLine
	for rows.Next() {
		var l InvoiceSaleLine
		if err := rows.Scan(&l.SaleID, &l.BaseCents, &l.IVACents); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (r *SQLInvoiceRepository) ActiveInvoicesForSales(ctx context.Context, merchantID string, saleIDs []string) (map[string]string, error) {
	out := map[string]string{}
	if len(saleIDs) == 0 {
		return out, nil
	}
	rows, err := r.db.QueryContext(ctx, `SELECT sale_id, invoice_id FROM invoice_sales WHERE merchant_id = $1 AND active AND sale_id = ANY($2)`,
		merchantID, saleIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var s, i string
		if err := rows.Scan(&s, &i); err != nil {
			return nil, err
		}
		out[s] = i
	}
	return out, rows.Err()
}

func (r *SQLInvoiceRepository) ClaimStamping(ctx context.Context, merchantID, id string, until time.Time) (InvoiceRecord, bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return InvoiceRecord{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	where := `id = $1 AND status IN ('PENDING', 'FAILED') AND (stamp_lease_until IS NULL OR stamp_lease_until < NOW()) FOR UPDATE`
	args := []any{id}
	if merchantID != "" {
		where = `merchant_id = $2 AND ` + where
		args = append(args, merchantID)
	}
	rec, found, err := getInvoice(ctx, tx, where, args...)
	if err != nil || !found {
		return InvoiceRecord{}, false, err
	}
	if rec.Status == InvoiceFailed {
		res, err := tx.ExecContext(ctx, `UPDATE invoice_sales s SET active = TRUE WHERE s.invoice_id = $1 AND NOT s.active
			AND NOT EXISTS (SELECT 1 FROM invoice_sales o WHERE o.merchant_id = s.merchant_id AND o.sale_id = s.sale_id AND o.active)`, id)
		if err != nil {
			return InvoiceRecord{}, false, err
		}
		if n, _ := res.RowsAffected(); int(n) != len(rec.SaleIDs) {
			var sale, holder string
			_ = tx.QueryRowContext(ctx, `SELECT o.sale_id, o.invoice_id FROM invoice_sales s JOIN invoice_sales o
				ON o.merchant_id = s.merchant_id AND o.sale_id = s.sale_id AND o.active AND o.invoice_id <> s.invoice_id
				WHERE s.invoice_id = $1 LIMIT 1`, id).Scan(&sale, &holder)
			return InvoiceRecord{}, false, &SaleInvoicedError{SaleID: sale, InvoiceID: holder}
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE invoices SET status = 'PENDING', stamp_attempts = stamp_attempts + 1,
		stamp_lease_until = $2, updated_at = NOW() WHERE id = $1`, id, until.UTC()); err != nil {
		return InvoiceRecord{}, false, err
	}
	rec, _, err = getInvoice(ctx, tx, `id = $1`, id)
	if err != nil {
		return InvoiceRecord{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return InvoiceRecord{}, false, err
	}
	return rec, true, nil
}

func (r *SQLInvoiceRepository) UpdateInvoice(ctx context.Context, rec InvoiceRecord, expect string, op LinkOp) (InvoiceRecord, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return InvoiceRecord{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var uuid any
	if rec.UUID != "" {
		uuid = rec.UUID
	}
	res, err := tx.ExecContext(ctx, `UPDATE invoices SET status = $3, uuid = $4, xml = $5, sealed_at = $6, error_code = $7,
		error_detail = $8, stamp_lease_until = $9, cancel_motivo = $10, cancel_folio_sustitucion = $11, cancel_status = $12,
		cancel_requested_at = $13, cancel_resolved_at = $14, emailed_at = $15, stamped_at = $16, updated_at = NOW()
		WHERE id = $1 AND status = $2`,
		rec.ID, expect, rec.Status, uuid, rec.XML, rec.SealedAt, truncate(rec.ErrorCode, 64), rec.ErrorDetail, rec.StampLeaseUntil,
		rec.CancelMotivo, rec.CancelFolioSustitucion, rec.CancelStatus, rec.CancelRequestedAt, rec.CancelResolvedAt, rec.EmailedAt, rec.StampedAt)
	if err != nil {
		return InvoiceRecord{}, fmt.Errorf("update invoice: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return InvoiceRecord{}, ErrInvoiceConflict
	}
	if op == LinksRelease {
		if _, err := tx.ExecContext(ctx, `UPDATE invoice_sales SET active = FALSE WHERE invoice_id = $1`, rec.ID); err != nil {
			return InvoiceRecord{}, err
		}
		if rec.ReplacesInvoiceID != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE invoice_sales s SET active = TRUE FROM invoices i
				WHERE s.invoice_id = $1 AND i.id = s.invoice_id AND i.status IN ('STAMPED', 'CANCEL_REQUESTED')
				AND s.sale_id IN (SELECT sale_id FROM invoice_sales WHERE invoice_id = $2)
				AND NOT EXISTS (SELECT 1 FROM invoice_sales o WHERE o.merchant_id = s.merchant_id AND o.sale_id = s.sale_id AND o.active)`,
				rec.ReplacesInvoiceID, rec.ID); err != nil {
				return InvoiceRecord{}, err
			}
		}
	}
	out, _, err := getInvoice(ctx, tx, `id = $1`, rec.ID)
	if err != nil {
		return InvoiceRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return InvoiceRecord{}, err
	}
	return out, nil
}

// ----------------------------------------------------------------------------
// In-memory implementation
// ----------------------------------------------------------------------------

type memLink struct {
	InvoiceSaleLine
	merchantID string
	active     bool
}

// MemoryInvoiceRepository is the in-memory InvoiceRepository.
type MemoryInvoiceRepository struct {
	mu     sync.Mutex
	recs   map[string]InvoiceRecord
	links  map[string][]*memLink // invoice id -> links (position order)
	folios map[string]int64
}

func NewMemoryInvoiceRepository() *MemoryInvoiceRepository {
	return &MemoryInvoiceRepository{recs: map[string]InvoiceRecord{}, links: map[string][]*memLink{}, folios: map[string]int64{}}
}

func (m *MemoryInvoiceRepository) holder(merchantID, saleID string) string {
	for id, ls := range m.links {
		for _, l := range ls {
			if l.active && l.merchantID == merchantID && l.SaleID == saleID {
				return id
			}
		}
	}
	return ""
}

func (m *MemoryInvoiceRepository) withSales(rec InvoiceRecord) InvoiceRecord {
	rec.SaleIDs = nil
	for _, l := range m.links[rec.ID] {
		rec.SaleIDs = append(rec.SaleIDs, l.SaleID)
	}
	rec.XML = append([]byte(nil), rec.XML...)
	return rec
}

func (m *MemoryInvoiceRepository) CreateInvoice(_ context.Context, rec InvoiceRecord, lines []InvoiceSaleLine) (InvoiceRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.recs {
		if r.MerchantID == rec.MerchantID && r.IdempotencyKey == rec.IdempotencyKey {
			return m.withSales(r), ErrInvoiceExists
		}
	}
	if rec.Kind == InvoiceGlobal {
		for _, r := range m.recs {
			if r.MerchantID == rec.MerchantID && r.Kind == InvoiceGlobal && r.GlobalPeriod == rec.GlobalPeriod && r.Live() {
				return InvoiceRecord{}, ErrGlobalInvoiceExists
			}
		}
	}
	transfer := map[string]bool{}
	if rec.ReplacesInvoiceID != "" {
		for _, l := range lines {
			transfer[l.SaleID] = true
		}
	}
	for _, l := range lines {
		if h := m.holder(rec.MerchantID, l.SaleID); h != "" && !(h == rec.ReplacesInvoiceID && transfer[l.SaleID]) {
			return InvoiceRecord{}, &SaleInvoicedError{SaleID: l.SaleID, InvoiceID: h}
		}
	}
	for _, l := range m.links[rec.ReplacesInvoiceID] {
		if transfer[l.SaleID] {
			l.active = false
		}
	}
	key := rec.MerchantID + "|" + rec.Serie
	m.folios[key]++
	rec.Folio = m.folios[key]
	now := time.Now().UTC()
	rec.CreatedAt, rec.UpdatedAt = now, now
	for _, l := range lines {
		m.links[rec.ID] = append(m.links[rec.ID], &memLink{InvoiceSaleLine: l, merchantID: rec.MerchantID, active: true})
	}
	m.recs[rec.ID] = rec
	return m.withSales(rec), nil
}

func (m *MemoryInvoiceRepository) GetInvoice(_ context.Context, merchantID, id string) (InvoiceRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.recs[id]
	if !ok || (merchantID != "" && r.MerchantID != merchantID) {
		return InvoiceRecord{}, false, nil
	}
	return m.withSales(r), true, nil
}

func (m *MemoryInvoiceRepository) FindInvoiceByUUID(_ context.Context, merchantID, uuid string) (InvoiceRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.recs {
		if r.MerchantID == merchantID && r.UUID != "" && strings.EqualFold(r.UUID, uuid) {
			return m.withSales(r), true, nil
		}
	}
	return InvoiceRecord{}, false, nil
}

func (m *MemoryInvoiceRepository) FindInvoiceByIdempotencyKey(_ context.Context, merchantID, key string) (InvoiceRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.recs {
		if r.MerchantID == merchantID && r.IdempotencyKey == key {
			return m.withSales(r), true, nil
		}
	}
	return InvoiceRecord{}, false, nil
}

func (m *MemoryInvoiceRepository) ListInvoices(_ context.Context, q InvoiceQuery) ([]InvoiceRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []InvoiceRecord
	for _, r := range m.recs {
		if (q.MerchantID != "" && r.MerchantID != q.MerchantID) || (q.Kind != "" && r.Kind != q.Kind) ||
			(len(q.Statuses) > 0 && !containsStr(q.Statuses, r.Status)) || !q.Before.admits(r.CreatedAt, r.ID) {
			continue
		}
		out = append(out, m.withSales(r))
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	if l := invoiceLimit(q.Limit); len(out) > l {
		out = out[:l]
	}
	return out, nil
}

func containsStr(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func (m *MemoryInvoiceRepository) InvoiceLines(_ context.Context, invoiceID string) ([]InvoiceSaleLine, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []InvoiceSaleLine
	for _, l := range m.links[invoiceID] {
		out = append(out, l.InvoiceSaleLine)
	}
	return out, nil
}

func (m *MemoryInvoiceRepository) ActiveInvoicesForSales(_ context.Context, merchantID string, saleIDs []string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for _, s := range saleIDs {
		if h := m.holder(merchantID, s); h != "" {
			out[s] = h
		}
	}
	return out, nil
}

func (m *MemoryInvoiceRepository) ClaimStamping(_ context.Context, merchantID, id string, until time.Time) (InvoiceRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.recs[id]
	now := time.Now()
	if !ok || (merchantID != "" && r.MerchantID != merchantID) || (r.Status != InvoicePending && r.Status != InvoiceFailed) ||
		(r.StampLeaseUntil != nil && r.StampLeaseUntil.After(now)) {
		return InvoiceRecord{}, false, nil
	}
	if r.Status == InvoiceFailed {
		for _, l := range m.links[id] {
			if !l.active {
				if h := m.holder(r.MerchantID, l.SaleID); h != "" {
					return InvoiceRecord{}, false, &SaleInvoicedError{SaleID: l.SaleID, InvoiceID: h}
				}
			}
		}
		for _, l := range m.links[id] {
			l.active = true
		}
	}
	u := until.UTC()
	r.Status, r.StampAttempts, r.StampLeaseUntil, r.UpdatedAt = InvoicePending, r.StampAttempts+1, &u, now.UTC()
	m.recs[id] = r
	return m.withSales(r), true, nil
}

func (m *MemoryInvoiceRepository) UpdateInvoice(_ context.Context, rec InvoiceRecord, expect string, op LinkOp) (InvoiceRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.recs[rec.ID]
	if !ok || cur.Status != expect {
		return InvoiceRecord{}, ErrInvoiceConflict
	}
	if rec.UUID != "" {
		for id, r := range m.recs {
			if id != rec.ID && strings.EqualFold(r.UUID, rec.UUID) {
				return InvoiceRecord{}, fmt.Errorf("duplicate uuid %s", rec.UUID)
			}
		}
	}
	// Immutable columns keep their stored values.
	rec.MerchantID, rec.Kind, rec.IdempotencyKey, rec.RequestHash, rec.Serie, rec.Folio = cur.MerchantID, cur.Kind, cur.IdempotencyKey, cur.RequestHash, cur.Serie, cur.Folio
	rec.SubtotalCents, rec.IVACents, rec.ISRRetenidoCents, rec.TotalCents = cur.SubtotalCents, cur.IVACents, cur.ISRRetenidoCents, cur.TotalCents
	rec.FormaPago, rec.Receptor, rec.GlobalPeriod, rec.ReplacesInvoiceID, rec.StampAttempts = cur.FormaPago, cur.Receptor, cur.GlobalPeriod, cur.ReplacesInvoiceID, cur.StampAttempts
	rec.CreatedAt, rec.UpdatedAt = cur.CreatedAt, time.Now().UTC()
	rec.XML = append([]byte(nil), rec.XML...)
	m.recs[rec.ID] = rec
	if op == LinksRelease {
		mine := map[string]bool{}
		for _, l := range m.links[rec.ID] {
			l.active = false
			mine[l.SaleID] = true
		}
		if old, ok := m.recs[rec.ReplacesInvoiceID]; ok && (old.Status == InvoiceStamped || old.Status == InvoiceCancelRequested) {
			for _, l := range m.links[old.ID] {
				if mine[l.SaleID] && m.holder(old.MerchantID, l.SaleID) == "" {
					l.active = true
				}
			}
		}
	}
	return m.withSales(rec), nil
}
