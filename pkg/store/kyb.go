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

	"softpos/pkg/kyb"
)

// ErrKYBConflict is returned by UpdateKYB when the stored version differs
// from the caller's (concurrent update): reload and retry.
var ErrKYBConflict = errors.New("kyb record changed concurrently")

// KYBRecord is a merchant's onboarding state (merchant_kyb): application
// data, automatic check results, operator decision and the block flag.
type KYBRecord struct {
	MerchantID         string
	Status             kyb.Status
	Application        kyb.Application
	PersonaMoral       bool // derived from the RFC length, never from a client flag
	RFCValid           bool
	List69B            kyb.List69B
	DocumentsComplete  bool
	IdentityCheck      kyb.IdentityResult
	IdentityReference  string
	IdentityDetail     string
	AcquirerMerchantID string
	Reason             string
	SubmittedAt        *time.Time
	DecidedAt          *time.Time
	DecidedBy          string
	Blocked            bool
	BlockedReason      string
	BlockedAt          *time.Time
	Version            int64
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// Active reports whether the merchant may move money (APPROVED, not blocked).
func (r KYBRecord) Active() bool { return r.Status == kyb.StatusApproved && !r.Blocked }

// Document statuses.
const (
	KYBDocReceived   = "RECEIVED"
	KYBDocSuperseded = "SUPERSEDED" // a newer upload of the same type replaced it
)

// KYBDocument is the metadata of an uploaded KYC document (kyb_documents).
type KYBDocument struct {
	ID          string
	MerchantID  string
	Type        kyb.DocumentType
	ContentType string // sniffed on upload
	SizeBytes   int64
	SHA256      string // of the plaintext
	BlobKey     string
	Status      string
	UploadedBy  string
	CreatedAt   time.Time
}

// KYBQuery filters the operator listing (newest update first).
type KYBQuery struct {
	Status kyb.Status // "" = any
	// Blocked filters by the block flag when non-nil.
	Blocked *bool
	Limit   int
}

// KYBRepository persists onboarding state and document metadata.
type KYBRepository interface {
	GetKYB(ctx context.Context, merchantID string) (KYBRecord, bool, error)
	// CreateKYB inserts rec if the merchant has no row (created=false otherwise).
	CreateKYB(ctx context.Context, rec KYBRecord) (created bool, err error)
	// UpdateKYB writes every mutable column only if the stored version equals
	// rec.Version (ErrKYBConflict otherwise) and returns the stored record.
	UpdateKYB(ctx context.Context, rec KYBRecord) (KYBRecord, error)
	ListKYB(ctx context.Context, q KYBQuery) ([]KYBRecord, error)
	// AddKYBDocument stores the metadata; earlier RECEIVED documents of the
	// same type become SUPERSEDED.
	AddKYBDocument(ctx context.Context, doc KYBDocument) error
	// ListKYBDocuments returns the merchant's documents (oldest first);
	// superseded ones only when includeSuperseded.
	ListKYBDocuments(ctx context.Context, merchantID string, includeSuperseded bool) ([]KYBDocument, error)
	GetKYBDocument(ctx context.Context, merchantID, documentID string) (KYBDocument, bool, error)
}

// DocumentBlobStore stores encrypted document envelopes. SQLKYBRepository
// (kyb_document_blobs, bytea) and MemoryKYBRepository implement it; an
// object store (S3) can replace it without touching the handlers.
type DocumentBlobStore interface {
	PutBlob(ctx context.Context, key string, envelope []byte) error
	GetBlob(ctx context.Context, key string) ([]byte, bool, error)
}

var (
	_ KYBRepository     = (*SQLKYBRepository)(nil)
	_ DocumentBlobStore = (*SQLKYBRepository)(nil)
	_ KYBRepository     = (*MemoryKYBRepository)(nil)
	_ DocumentBlobStore = (*MemoryKYBRepository)(nil)
)

func normalizeKYB(rec KYBRecord) KYBRecord {
	if rec.Status == "" {
		rec.Status = kyb.StatusDraft
	}
	if rec.List69B == "" {
		rec.List69B = kyb.List69BUnknown
	}
	if rec.IdentityCheck == "" {
		rec.IdentityCheck = kyb.IdentityUnknown
	}
	return rec
}

// maxKYBListing caps ListKYB (the backoffice applies its own page limits;
// batch jobs such as the 69-B retroscreen read every approved merchant).
const maxKYBListing = 100000

func kybLimit(n int) int {
	if n <= 0 {
		return 100
	}
	return min(n, maxKYBListing)
}

// ----------------------------------------------------------------------------
// SQL implementation
// ----------------------------------------------------------------------------

// SQLKYBRepository implements KYBRepository and DocumentBlobStore on PostgreSQL.
type SQLKYBRepository struct {
	db *sql.DB
}

func NewSQLKYBRepository(db *sql.DB) *SQLKYBRepository { return &SQLKYBRepository{db: db} }

const kybColumns = `merchant_id, status, application_json, persona_moral, rfc_valid, list_69b, documents_complete,
	identity_check, identity_reference, identity_detail, acquirer_merchant_id, reason, submitted_at, decided_at,
	decided_by, blocked, blocked_reason, blocked_at, version, created_at, updated_at`

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

func ptrTime(nt sql.NullTime) *time.Time {
	if !nt.Valid {
		return nil
	}
	t := nt.Time.UTC()
	return &t
}

func scanKYB(row rowScanner) (KYBRecord, error) {
	var r KYBRecord
	var app []byte
	var status, list, idc string
	var sub, dec, blk sql.NullTime
	err := row.Scan(&r.MerchantID, &status, &app, &r.PersonaMoral, &r.RFCValid, &list, &r.DocumentsComplete,
		&idc, &r.IdentityReference, &r.IdentityDetail, &r.AcquirerMerchantID, &r.Reason, &sub, &dec,
		&r.DecidedBy, &r.Blocked, &r.BlockedReason, &blk, &r.Version, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return KYBRecord{}, err
	}
	if len(app) > 0 {
		if err := json.Unmarshal(app, &r.Application); err != nil {
			return KYBRecord{}, fmt.Errorf("corrupt kyb application: %w", err)
		}
	}
	r.Status, r.List69B, r.IdentityCheck = kyb.Status(status), kyb.List69B(list), kyb.IdentityResult(idc)
	r.SubmittedAt, r.DecidedAt, r.BlockedAt = ptrTime(sub), ptrTime(dec), ptrTime(blk)
	return r, nil
}

func (r *SQLKYBRepository) GetKYB(ctx context.Context, merchantID string) (KYBRecord, bool, error) {
	rec, err := scanKYB(r.db.QueryRowContext(ctx, `SELECT `+kybColumns+` FROM merchant_kyb WHERE merchant_id = $1`, merchantID))
	if errors.Is(err, sql.ErrNoRows) {
		return KYBRecord{}, false, nil
	}
	if err != nil {
		return KYBRecord{}, false, fmt.Errorf("get kyb: %w", err)
	}
	return rec, true, nil
}

func (r *SQLKYBRepository) CreateKYB(ctx context.Context, rec KYBRecord) (bool, error) {
	rec = normalizeKYB(rec)
	app, err := json.Marshal(rec.Application)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO merchant_kyb (merchant_id, status, application_json, persona_moral, rfc_valid, list_69b,
			documents_complete, identity_check, identity_reference, identity_detail, acquirer_merchant_id, reason,
			submitted_at, decided_at, decided_by, blocked, blocked_reason, blocked_at, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, 1, $19, $19)
		ON CONFLICT (merchant_id) DO NOTHING`,
		rec.MerchantID, string(rec.Status), app, rec.PersonaMoral, rec.RFCValid, string(rec.List69B),
		rec.DocumentsComplete, string(rec.IdentityCheck), rec.IdentityReference, rec.IdentityDetail,
		rec.AcquirerMerchantID, rec.Reason, nullTime(rec.SubmittedAt), nullTime(rec.DecidedAt), rec.DecidedBy,
		rec.Blocked, rec.BlockedReason, nullTime(rec.BlockedAt), now)
	if err != nil {
		if isForeignKeyViolation(err) {
			return false, ErrMerchantNotFound
		}
		return false, fmt.Errorf("create kyb: %w", err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (r *SQLKYBRepository) UpdateKYB(ctx context.Context, rec KYBRecord) (KYBRecord, error) {
	rec = normalizeKYB(rec)
	app, err := json.Marshal(rec.Application)
	if err != nil {
		return KYBRecord{}, err
	}
	out, err := scanKYB(r.db.QueryRowContext(ctx, `
		UPDATE merchant_kyb SET status = $2, application_json = $3, persona_moral = $4, rfc_valid = $5,
			list_69b = $6, documents_complete = $7, identity_check = $8, identity_reference = $9,
			identity_detail = $10, acquirer_merchant_id = $11, reason = $12, submitted_at = $13, decided_at = $14,
			decided_by = $15, blocked = $16, blocked_reason = $17, blocked_at = $18, version = version + 1,
			updated_at = NOW()
		WHERE merchant_id = $1 AND version = $19
		RETURNING `+kybColumns,
		rec.MerchantID, string(rec.Status), app, rec.PersonaMoral, rec.RFCValid, string(rec.List69B),
		rec.DocumentsComplete, string(rec.IdentityCheck), rec.IdentityReference, rec.IdentityDetail,
		rec.AcquirerMerchantID, rec.Reason, nullTime(rec.SubmittedAt), nullTime(rec.DecidedAt), rec.DecidedBy,
		rec.Blocked, rec.BlockedReason, nullTime(rec.BlockedAt), rec.Version))
	if errors.Is(err, sql.ErrNoRows) {
		return KYBRecord{}, ErrKYBConflict
	}
	if err != nil {
		return KYBRecord{}, fmt.Errorf("update kyb: %w", err)
	}
	return out, nil
}

func (r *SQLKYBRepository) ListKYB(ctx context.Context, q KYBQuery) ([]KYBRecord, error) {
	var where []string
	var args []any
	if q.Status != "" {
		args = append(args, string(q.Status))
		where = append(where, fmt.Sprintf("status = $%d", len(args)))
	}
	if q.Blocked != nil {
		args = append(args, *q.Blocked)
		where = append(where, fmt.Sprintf("blocked = $%d", len(args)))
	}
	query := `SELECT ` + kybColumns + ` FROM merchant_kyb`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	args = append(args, kybLimit(q.Limit))
	query += fmt.Sprintf(" ORDER BY updated_at DESC, merchant_id LIMIT $%d", len(args))
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list kyb: %w", err)
	}
	defer rows.Close()
	var out []KYBRecord
	for rows.Next() {
		rec, err := scanKYB(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

const kybDocColumns = `id, merchant_id, doc_type, content_type, size_bytes, sha256, blob_key, status, uploaded_by, created_at`

func scanKYBDoc(row rowScanner) (KYBDocument, error) {
	var d KYBDocument
	var t string
	if err := row.Scan(&d.ID, &d.MerchantID, &t, &d.ContentType, &d.SizeBytes, &d.SHA256, &d.BlobKey, &d.Status, &d.UploadedBy, &d.CreatedAt); err != nil {
		return KYBDocument{}, err
	}
	d.Type = kyb.DocumentType(t)
	return d, nil
}

func (r *SQLKYBRepository) AddKYBDocument(ctx context.Context, d KYBDocument) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE kyb_documents SET status = 'SUPERSEDED'
		WHERE merchant_id = $1 AND doc_type = $2 AND status = 'RECEIVED'`, d.MerchantID, string(d.Type)); err != nil {
		return fmt.Errorf("supersede kyb documents: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO kyb_documents (`+kybDocColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'RECEIVED', $8, NOW())`,
		d.ID, d.MerchantID, string(d.Type), d.ContentType, d.SizeBytes, d.SHA256, d.BlobKey, d.UploadedBy); err != nil {
		if isForeignKeyViolation(err) {
			return ErrMerchantNotFound
		}
		return fmt.Errorf("insert kyb document: %w", err)
	}
	return tx.Commit()
}

func (r *SQLKYBRepository) ListKYBDocuments(ctx context.Context, merchantID string, includeSuperseded bool) ([]KYBDocument, error) {
	q := `SELECT ` + kybDocColumns + ` FROM kyb_documents WHERE merchant_id = $1`
	if !includeSuperseded {
		q += ` AND status = 'RECEIVED'`
	}
	rows, err := r.db.QueryContext(ctx, q+` ORDER BY created_at, id`, merchantID)
	if err != nil {
		return nil, fmt.Errorf("list kyb documents: %w", err)
	}
	defer rows.Close()
	var out []KYBDocument
	for rows.Next() {
		d, err := scanKYBDoc(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (r *SQLKYBRepository) GetKYBDocument(ctx context.Context, merchantID, id string) (KYBDocument, bool, error) {
	d, err := scanKYBDoc(r.db.QueryRowContext(ctx, `SELECT `+kybDocColumns+` FROM kyb_documents WHERE merchant_id = $1 AND id = $2`, merchantID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return KYBDocument{}, false, nil
	}
	if err != nil {
		return KYBDocument{}, false, fmt.Errorf("get kyb document: %w", err)
	}
	return d, true, nil
}

func (r *SQLKYBRepository) PutBlob(ctx context.Context, key string, envelope []byte) error {
	if _, err := r.db.ExecContext(ctx, `INSERT INTO kyb_document_blobs (blob_key, envelope) VALUES ($1, $2)`, key, envelope); err != nil {
		return fmt.Errorf("put kyb blob: %w", err)
	}
	return nil
}

func (r *SQLKYBRepository) GetBlob(ctx context.Context, key string) ([]byte, bool, error) {
	var b []byte
	err := r.db.QueryRowContext(ctx, `SELECT envelope FROM kyb_document_blobs WHERE blob_key = $1`, key).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("get kyb blob: %w", err)
	}
	return b, true, nil
}

// ----------------------------------------------------------------------------
// In-memory implementation (no-DB dev/test mode)
// ----------------------------------------------------------------------------

// MemoryKYBRepository is the goroutine-safe in-process KYBRepository and
// DocumentBlobStore with the same semantics as the SQL implementation.
type MemoryKYBRepository struct {
	mu    sync.Mutex
	recs  map[string]KYBRecord
	docs  []KYBDocument
	blobs map[string][]byte
}

func NewMemoryKYBRepository() *MemoryKYBRepository {
	return &MemoryKYBRepository{recs: map[string]KYBRecord{}, blobs: map[string][]byte{}}
}

func (m *MemoryKYBRepository) GetKYB(_ context.Context, merchantID string) (KYBRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.recs[merchantID]
	return r, ok, nil
}

func (m *MemoryKYBRepository) CreateKYB(_ context.Context, rec KYBRecord) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.recs[rec.MerchantID]; ok {
		return false, nil
	}
	rec = normalizeKYB(rec)
	now := time.Now().UTC()
	rec.Version, rec.CreatedAt, rec.UpdatedAt = 1, now, now
	m.recs[rec.MerchantID] = rec
	return true, nil
}

func (m *MemoryKYBRepository) UpdateKYB(_ context.Context, rec KYBRecord) (KYBRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.recs[rec.MerchantID]
	if !ok || cur.Version != rec.Version {
		return KYBRecord{}, ErrKYBConflict
	}
	rec = normalizeKYB(rec)
	rec.Version, rec.CreatedAt = cur.Version+1, cur.CreatedAt
	rec.UpdatedAt = time.Now().UTC()
	if !rec.UpdatedAt.After(cur.UpdatedAt) {
		rec.UpdatedAt = cur.UpdatedAt.Add(time.Microsecond)
	}
	m.recs[rec.MerchantID] = rec
	return rec, nil
}

func (m *MemoryKYBRepository) ListKYB(_ context.Context, q KYBQuery) ([]KYBRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []KYBRecord
	for _, r := range m.recs {
		if (q.Status == "" || r.Status == q.Status) && (q.Blocked == nil || r.Blocked == *q.Blocked) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].MerchantID < out[j].MerchantID
	})
	if l := kybLimit(q.Limit); len(out) > l {
		out = out[:l]
	}
	return out, nil
}

func (m *MemoryKYBRepository) AddKYBDocument(_ context.Context, d KYBDocument) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.docs {
		if m.docs[i].MerchantID == d.MerchantID && m.docs[i].Type == d.Type && m.docs[i].Status == KYBDocReceived {
			m.docs[i].Status = KYBDocSuperseded
		}
	}
	d.Status, d.CreatedAt = KYBDocReceived, time.Now().UTC()
	m.docs = append(m.docs, d)
	return nil
}

func (m *MemoryKYBRepository) ListKYBDocuments(_ context.Context, merchantID string, includeSuperseded bool) ([]KYBDocument, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []KYBDocument
	for _, d := range m.docs {
		if d.MerchantID == merchantID && (includeSuperseded || d.Status == KYBDocReceived) {
			out = append(out, d)
		}
	}
	return out, nil
}

func (m *MemoryKYBRepository) GetKYBDocument(_ context.Context, merchantID, id string) (KYBDocument, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.docs {
		if d.MerchantID == merchantID && d.ID == id {
			return d, true, nil
		}
	}
	return KYBDocument{}, false, nil
}

func (m *MemoryKYBRepository) PutBlob(_ context.Context, key string, envelope []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.blobs[key]; dup {
		return fmt.Errorf("put kyb blob: duplicate key %s", key)
	}
	m.blobs[key] = append([]byte(nil), envelope...)
	return nil
}

func (m *MemoryKYBRepository) GetBlob(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.blobs[key]
	return append([]byte(nil), b...), ok, nil
}
