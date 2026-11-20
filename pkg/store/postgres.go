package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"softpos/pkg/identity"
	"softpos/pkg/ledger"
)

// Common errors
var (
	ErrAccountNotFound     = ledger.ErrAccountNotFound
	ErrDuplicateAccount    = ledger.ErrDuplicateAccount
	ErrUnbalancedJournal   = ledger.ErrUnbalancedJournal
	ErrMerchantNotFound    = errors.New("merchant not found")
	ErrDuplicateMerchant   = errors.New("merchant already exists")
	ErrDuplicateCLABE      = errors.New("clabe already assigned to another merchant")
	ErrDuplicateJournal    = errors.New("ledger journal already posted")
	ErrInvalidFeeBps       = errors.New("platform fee must be between 0 and 10000 bps")
	ErrSettlementCLABEChanged  = errors.New("settlement CLABE changed or not registered")
	ErrInvalidSettlementStatus = errors.New("invalid settlement account status")
)

// MerchantRecord represents persisted merchant profile data.
type MerchantRecord struct {
	ID               string
	RFC              string
	Name             string
	IsRESICO         bool
	VirtualCLABE     string
	EncryptedCSDJSON []byte
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// SettlementAccountStatus is the ownership-verification state of a merchant's
// settlement (payout) CLABE.
type SettlementAccountStatus string

const (
	SettlementNone     SettlementAccountStatus = "NONE"
	SettlementPending  SettlementAccountStatus = "PENDING"
	SettlementVerified SettlementAccountStatus = "VERIFIED"
	SettlementRejected SettlementAccountStatus = "REJECTED"
)

func (s SettlementAccountStatus) Valid() bool {
	switch s {
	case SettlementNone, SettlementPending, SettlementVerified, SettlementRejected:
		return true
	}
	return false
}

// SettlementAccount is the merchant's own bank account for payouts (distinct
// from the virtual collection CLABE).
type SettlementAccount struct {
	CLABE     string
	Status    SettlementAccountStatus
	UpdatedAt time.Time
}

// ----------------------------------------------------------------------------
// SQLLedgerRepository with Pessimistic Row-Level Locking (SELECT ... FOR UPDATE)
// ----------------------------------------------------------------------------

type SQLLedgerRepository struct {
	db *sql.DB
}

func NewSQLLedgerRepository(db *sql.DB) *SQLLedgerRepository {
	return &SQLLedgerRepository{db: db}
}

func (r *SQLLedgerRepository) CreateAccount(ctx context.Context, acc ledger.Account) error {
	now := time.Now().UTC()
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO ledger_accounts (id, code, name, account_type, balance_cents, currency, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 0, 'MXN', $5, $5)
	`, acc.ID, acc.Code, acc.Name, string(acc.Type), now)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") || strings.Contains(strings.ToLower(err.Error()), "duplicate") {
			return ErrDuplicateAccount
		}
		return fmt.Errorf("failed to create ledger account: %w", err)
	}
	return nil
}

func (r *SQLLedgerRepository) GetAccount(ctx context.Context, code string) (ledger.Account, error) {
	var id, accCode, name, accType string
	err := r.db.QueryRowContext(ctx, `
		SELECT id, code, name, account_type
		FROM ledger_accounts
		WHERE code = $1 OR id = $1
	`, code).Scan(&id, &accCode, &name, &accType)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ledger.Account{}, ErrAccountNotFound
		}
		return ledger.Account{}, fmt.Errorf("failed to get account: %w", err)
	}
	return ledger.Account{
		ID:   id,
		Code: accCode,
		Name: name,
		Type: ledger.AccountType(accType),
	}, nil
}

func (r *SQLLedgerRepository) GetBalance(ctx context.Context, accountCode string) (ledger.Money, error) {
	var balanceCents int64
	var accType, currency string
	err := r.db.QueryRowContext(ctx, `
		SELECT balance_cents, account_type, currency
		FROM ledger_accounts
		WHERE code = $1 OR id = $1
	`, accountCode).Scan(&balanceCents, &accType, &currency)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ledger.Money{}, ErrAccountNotFound
		}
		return ledger.Money{}, fmt.Errorf("failed to get account balance: %w", err)
	}

	if currency == "" {
		currency = "MXN"
	}
	return ledger.NewMoney(balanceCents, currency), nil
}

// PostTransaction executes an atomic double-entry transaction using pessimistic locking
// (SELECT ... FOR UPDATE) on all involved accounts to guarantee no race conditions or lost updates.
func (r *SQLLedgerRepository) PostTransaction(ctx context.Context, journal *ledger.JournalEntry) error {
	if journal == nil {
		return errors.New("journal entry cannot be nil")
	}
	if !journal.IsBalanced() {
		return ErrUnbalancedJournal
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	if err := postJournalTx(ctx, tx, journal); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit ledger transaction: %w", err)
	}
	return nil
}

// postJournalTx posts a balanced journal inside tx: it locks every involved
// account (SELECT ... FOR UPDATE, sorted to avoid deadlocks), inserts the
// journal and its entries and updates the balances. The caller commits.
func postJournalTx(ctx context.Context, tx *sql.Tx, journal *ledger.JournalEntry) error {
	if journal == nil {
		return errors.New("journal entry cannot be nil")
	}
	if !journal.IsBalanced() {
		return ErrUnbalancedJournal
	}
	// Collect unique account IDs and sort them to prevent deadlocks across concurrent threads
	accountIDs := make([]string, 0, len(journal.Entries))
	seen := make(map[string]bool)
	for _, e := range journal.Entries {
		if !seen[e.AccountID] {
			seen[e.AccountID] = true
			accountIDs = append(accountIDs, e.AccountID)
		}
	}
	sort.Strings(accountIDs)

	// Pessimistic Locking: SELECT ... FOR UPDATE on each account
	accountTypes := make(map[string]ledger.AccountType)
	currentBalances := make(map[string]int64)

	for _, accID := range accountIDs {
		var id, accType string
		var balCents int64
		// Execute explicit pessimistic locking statement
		err := tx.QueryRowContext(ctx, `
			SELECT id, account_type, balance_cents
			FROM ledger_accounts
			WHERE id = $1 OR code = $1
			FOR UPDATE
		`, accID).Scan(&id, &accType, &balCents)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrAccountNotFound
			}
			return fmt.Errorf("failed to acquire pessimistic lock on account %s: %w", accID, err)
		}
		accountTypes[accID] = ledger.AccountType(accType)
		currentBalances[accID] = balCents
	}

	// Insert Journal Entry header
	metaBytes, err := json.Marshal(journal.Metadata)
	if err != nil {
		metaBytes = []byte("{}")
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO ledger_journals (id, description, metadata_json, created_at)
		VALUES ($1, $2, $3, $4)
	`, journal.ID, journal.Description, metaBytes, journal.Timestamp.UTC())
	if err != nil {
		if isUniqueViolation(err) {
			return ErrDuplicateJournal
		}
		return fmt.Errorf("failed to insert journal: %w", err)
	}

	// Calculate new balances based on account standard normal balances:
	// Asset/Expense: Debit increases (+), Credit decreases (-)
	// Liability/Equity/Revenue: Credit increases (+), Debit decreases (-)
	newBalances := make(map[string]int64)
	for k, v := range currentBalances {
		newBalances[k] = v
	}

	for _, entry := range journal.Entries {
		accType := accountTypes[entry.AccountID]
		isDebitNormal := (accType == ledger.AccountAsset || accType == ledger.AccountExpense)

		if entry.Direction == ledger.Debit {
			if isDebitNormal {
				newBalances[entry.AccountID] += entry.Money.AmountCents
			} else {
				newBalances[entry.AccountID] -= entry.Money.AmountCents
			}
		} else { // Credit
			if isDebitNormal {
				newBalances[entry.AccountID] -= entry.Money.AmountCents
			} else {
				newBalances[entry.AccountID] += entry.Money.AmountCents
			}
		}

		// Insert entry row
		_, err := tx.ExecContext(ctx, `
			INSERT INTO ledger_entries (journal_id, account_id, direction, amount_cents, currency, created_at)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, journal.ID, entry.AccountID, string(entry.Direction), entry.Money.AmountCents, entry.Money.Currency, journal.Timestamp.UTC())
		if err != nil {
			return fmt.Errorf("failed to insert entry for account %s: %w", entry.AccountID, err)
		}
	}

	// Update locked accounts with new balances
	now := time.Now().UTC()
	for _, accID := range accountIDs {
		_, err := tx.ExecContext(ctx, `
			UPDATE ledger_accounts
			SET balance_cents = $1, updated_at = $2
			WHERE id = $3 OR code = $3
		`, newBalances[accID], now, accID)
		if err != nil {
			return fmt.Errorf("failed to update balance for account %s: %w", accID, err)
		}
	}

	return nil
}

// ----------------------------------------------------------------------------
// SQLBlacklistRepository (SAT Article 69-B)
// ----------------------------------------------------------------------------

type SQLBlacklistRepository struct {
	db *sql.DB
}

func NewSQLBlacklistRepository(db *sql.DB) *SQLBlacklistRepository {
	return &SQLBlacklistRepository{db: db}
}

func (r *SQLBlacklistRepository) AddEntry(ctx context.Context, entry identity.BlacklistEntry) error {
	normalizedRFC := strings.ToUpper(strings.TrimSpace(entry.RFC))
	now := time.Now().UTC()

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO sat_blacklist (rfc, status, oficio_sat, updated_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (rfc) DO UPDATE
		SET status = EXCLUDED.status,
		    oficio_sat = EXCLUDED.oficio_sat,
		    updated_at = EXCLUDED.updated_at
	`, normalizedRFC, string(entry.Status), entry.OficioSAT, now)
	if err != nil {
		return fmt.Errorf("failed to upsert blacklist entry: %w", err)
	}
	return nil
}

// HasEntries reports whether any 69-B record was loaded (an empty table
// means "not checked", never "clean").
func (r *SQLBlacklistRepository) HasEntries(ctx context.Context) (bool, error) {
	var ok bool
	if err := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sat_blacklist)`).Scan(&ok); err != nil {
		return false, fmt.Errorf("failed to query blacklist presence: %w", err)
	}
	return ok, nil
}

func (r *SQLBlacklistRepository) GetEntry(ctx context.Context, rfc string) (identity.BlacklistEntry, bool, error) {
	normalizedRFC := strings.ToUpper(strings.TrimSpace(rfc))
	var status, oficio string
	err := r.db.QueryRowContext(ctx, `
		SELECT status, oficio_sat
		FROM sat_blacklist
		WHERE rfc = $1
	`, normalizedRFC).Scan(&status, &oficio)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return identity.BlacklistEntry{RFC: normalizedRFC, Status: identity.BlacklistClean}, false, nil
		}
		return identity.BlacklistEntry{}, false, fmt.Errorf("failed to query blacklist: %w", err)
	}
	return identity.BlacklistEntry{
		RFC:       normalizedRFC,
		Status:    identity.BlacklistStatus(status),
		OficioSAT: oficio,
	}, true, nil
}

// ----------------------------------------------------------------------------
// SQLMerchantRepository (Merchants, Virtual CLABEs, CSDs & Monthly Sales)
// ----------------------------------------------------------------------------

type SQLMerchantRepository struct {
	db *sql.DB
}

func NewSQLMerchantRepository(db *sql.DB) *SQLMerchantRepository {
	return &SQLMerchantRepository{db: db}
}

func (r *SQLMerchantRepository) SaveMerchant(ctx context.Context, m MerchantRecord) error {
	now := time.Now().UTC()
	var clabeVal any
	if m.VirtualCLABE != "" {
		clabeVal = m.VirtualCLABE
	}
	var csdVal any
	if len(m.EncryptedCSDJSON) > 0 {
		csdVal = m.EncryptedCSDJSON
	}

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO merchants (id, rfc, name, is_resico, virtual_clabe, encrypted_csd_json, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
		ON CONFLICT (id) DO UPDATE
		SET rfc = EXCLUDED.rfc,
		    name = EXCLUDED.name,
		    is_resico = EXCLUDED.is_resico,
		    virtual_clabe = COALESCE(EXCLUDED.virtual_clabe, merchants.virtual_clabe),
		    encrypted_csd_json = COALESCE(EXCLUDED.encrypted_csd_json, merchants.encrypted_csd_json),
		    updated_at = EXCLUDED.updated_at
	`, m.ID, m.RFC, m.Name, m.IsRESICO, clabeVal, csdVal, now)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrDuplicateCLABE
		}
		return fmt.Errorf("failed to save merchant: %w", err)
	}
	return nil
}

func (r *SQLMerchantRepository) GetMerchant(ctx context.Context, id string) (MerchantRecord, bool, error) {
	var m MerchantRecord
	var clabe sql.NullString
	var csd []byte
	err := r.db.QueryRowContext(ctx, `
		SELECT id, rfc, name, is_resico, virtual_clabe, encrypted_csd_json, created_at, updated_at
		FROM merchants
		WHERE id = $1
	`, id).Scan(&m.ID, &m.RFC, &m.Name, &m.IsRESICO, &clabe, &csd, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MerchantRecord{}, false, nil
		}
		return MerchantRecord{}, false, fmt.Errorf("failed to get merchant: %w", err)
	}
	if clabe.Valid {
		m.VirtualCLABE = clabe.String
	}
	m.EncryptedCSDJSON = csd
	return m, true, nil
}

func (r *SQLMerchantRepository) GetMerchantByCLABE(ctx context.Context, clabe string) (MerchantRecord, bool, error) {
	var m MerchantRecord
	var csd []byte
	var vCLABE sql.NullString
	err := r.db.QueryRowContext(ctx, `
		SELECT id, rfc, name, is_resico, virtual_clabe, encrypted_csd_json, created_at, updated_at
		FROM merchants
		WHERE virtual_clabe = $1
	`, clabe).Scan(&m.ID, &m.RFC, &m.Name, &m.IsRESICO, &vCLABE, &csd, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MerchantRecord{}, false, nil
		}
		return MerchantRecord{}, false, fmt.Errorf("failed to get merchant by CLABE: %w", err)
	}
	if vCLABE.Valid {
		m.VirtualCLABE = vCLABE.String
	}
	m.EncryptedCSDJSON = csd
	return m, true, nil
}

func (r *SQLMerchantRepository) GetMerchantCLABE(ctx context.Context, merchantID string) (string, bool, error) {
	var clabe sql.NullString
	err := r.db.QueryRowContext(ctx, `
		SELECT virtual_clabe
		FROM merchants
		WHERE id = $1
	`, merchantID).Scan(&clabe)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("failed to query merchant CLABE: %w", err)
	}
	if !clabe.Valid || clabe.String == "" {
		return "", false, nil
	}
	return clabe.String, true, nil
}

func (r *SQLMerchantRepository) SetMerchantCLABE(ctx context.Context, merchantID, clabe string) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE merchants
		SET virtual_clabe = $1, updated_at = $2
		WHERE id = $3
	`, clabe, time.Now().UTC(), merchantID)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrDuplicateCLABE
		}
		return fmt.Errorf("failed to set merchant CLABE: %w", err)
	}
	return requireRowAffected(res, ErrMerchantNotFound)
}

// AssignMerchantCLABE is a conditional UPDATE (only while virtual_clabe is
// NULL), so concurrent assignments keep the first CLABE.
func (r *SQLMerchantRepository) AssignMerchantCLABE(ctx context.Context, merchantID, clabe string) (string, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE merchants
		SET virtual_clabe = $1, updated_at = $2
		WHERE id = $3 AND (virtual_clabe IS NULL OR virtual_clabe = '')
	`, clabe, time.Now().UTC(), merchantID)
	if err != nil {
		if isUniqueViolation(err) {
			return "", ErrDuplicateCLABE
		}
		return "", fmt.Errorf("failed to assign merchant CLABE: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return "", err
	} else if n == 1 {
		return clabe, nil
	}
	current, found, err := r.GetMerchantCLABE(ctx, merchantID)
	if err != nil {
		return "", err
	}
	if !found {
		if _, exists, gErr := r.GetMerchant(ctx, merchantID); gErr != nil {
			return "", gErr
		} else if !exists {
			return "", ErrMerchantNotFound
		}
		return "", fmt.Errorf("merchant %s CLABE neither assigned nor present", merchantID)
	}
	return current, nil
}

func (r *SQLMerchantRepository) SaveMerchantCSD(ctx context.Context, merchantID string, encryptedCSDJSON []byte) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE merchants
		SET encrypted_csd_json = $1, updated_at = $2
		WHERE id = $3
	`, encryptedCSDJSON, time.Now().UTC(), merchantID)
	if err != nil {
		return fmt.Errorf("failed to save merchant CSD: %w", err)
	}
	return requireRowAffected(res, ErrMerchantNotFound)
}

func (r *SQLMerchantRepository) GetMerchantCSD(ctx context.Context, merchantID string) ([]byte, bool, error) {
	var csd []byte
	err := r.db.QueryRowContext(ctx, `
		SELECT encrypted_csd_json
		FROM merchants
		WHERE id = $1
	`, merchantID).Scan(&csd)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("failed to get merchant CSD: %w", err)
	}
	if len(csd) == 0 {
		return nil, false, nil
	}
	return csd, true, nil
}

func (r *SQLMerchantRepository) GetMonthlySalesCents(ctx context.Context, merchantID, yearMonth string) (int64, error) {
	var sales int64
	err := r.db.QueryRowContext(ctx, `
		SELECT accumulated_cents
		FROM merchant_monthly_sales
		WHERE merchant_id = $1 AND year_month = $2
	`, merchantID, yearMonth).Scan(&sales)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to query monthly sales: %w", err)
	}
	return sales, nil
}

// AddMonthlySalesCents uses transactional pessimistic locking (SELECT ... FOR UPDATE) to increment sales safely.
func (r *SQLMerchantRepository) AddMonthlySalesCents(ctx context.Context, merchantID, yearMonth string, amountCents int64) (int64, error) {
	if amountCents <= 0 {
		return r.GetMonthlySalesCents(ctx, merchantID, yearMonth)
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to begin tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	var currentSales int64
	err = tx.QueryRowContext(ctx, `
		SELECT accumulated_cents
		FROM merchant_monthly_sales
		WHERE merchant_id = $1 AND year_month = $2
		FOR UPDATE
	`, merchantID, yearMonth).Scan(&currentSales)

	now := time.Now().UTC()
	var newSales int64
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// No row yet, so FOR UPDATE locked nothing: a concurrent first
			// increment may insert the same key. Upsert-add so neither is lost.
			_, err = tx.ExecContext(ctx, `
				INSERT INTO merchant_monthly_sales (merchant_id, year_month, accumulated_cents, updated_at)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (merchant_id, year_month) DO UPDATE
				SET accumulated_cents = merchant_monthly_sales.accumulated_cents + EXCLUDED.accumulated_cents,
				    updated_at = EXCLUDED.updated_at
			`, merchantID, yearMonth, amountCents, now)
			if err != nil {
				if isForeignKeyViolation(err) {
					return 0, ErrMerchantNotFound
				}
				return 0, fmt.Errorf("failed to insert monthly sales: %w", err)
			}
			if err := tx.QueryRowContext(ctx, `
				SELECT accumulated_cents
				FROM merchant_monthly_sales
				WHERE merchant_id = $1 AND year_month = $2
			`, merchantID, yearMonth).Scan(&newSales); err != nil {
				return 0, fmt.Errorf("failed to read monthly sales after insert: %w", err)
			}
		} else {
			return 0, fmt.Errorf("failed to lock monthly sales row: %w", err)
		}
	} else {
		newSales = currentSales + amountCents
		_, err = tx.ExecContext(ctx, `
			UPDATE merchant_monthly_sales
			SET accumulated_cents = $1, updated_at = $2
			WHERE merchant_id = $3 AND year_month = $4
		`, newSales, now, merchantID, yearMonth)
		if err != nil {
			return 0, fmt.Errorf("failed to update monthly sales: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("failed to commit monthly sales: %w", err)
	}
	return newSales, nil
}

func (r *SQLMerchantRepository) GetMerchantPlatformFeeBps(ctx context.Context, merchantID string) (int64, bool, error) {
	var bps sql.NullInt64
	err := r.db.QueryRowContext(ctx, `SELECT platform_fee_bps FROM merchants WHERE id = $1`, merchantID).Scan(&bps)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("failed to get merchant platform fee: %w", err)
	}
	return bps.Int64, bps.Valid, nil
}

func (r *SQLMerchantRepository) SetMerchantPlatformFeeBps(ctx context.Context, merchantID string, bps int64) error {
	if bps < 0 || bps > 10000 {
		return ErrInvalidFeeBps
	}
	res, err := r.db.ExecContext(ctx, `UPDATE merchants SET platform_fee_bps = $1, updated_at = $2 WHERE id = $3`, bps, time.Now().UTC(), merchantID)
	if err != nil {
		return fmt.Errorf("failed to set merchant platform fee: %w", err)
	}
	return requireRowAffected(res, ErrMerchantNotFound)
}

func (r *SQLMerchantRepository) GetSettlementAccount(ctx context.Context, merchantID string) (SettlementAccount, bool, error) {
	var clabe sql.NullString
	var status string
	var updated sql.NullTime
	err := r.db.QueryRowContext(ctx, `
		SELECT settlement_clabe, settlement_clabe_status, settlement_clabe_updated_at FROM merchants WHERE id = $1
	`, merchantID).Scan(&clabe, &status, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SettlementAccount{}, false, nil
		}
		return SettlementAccount{}, false, fmt.Errorf("failed to get settlement account: %w", err)
	}
	if !clabe.Valid || clabe.String == "" {
		return SettlementAccount{}, false, nil
	}
	return SettlementAccount{CLABE: clabe.String, Status: SettlementAccountStatus(status), UpdatedAt: updated.Time}, true, nil
}

func (r *SQLMerchantRepository) SetSettlementAccount(ctx context.Context, merchantID, clabe string, status SettlementAccountStatus) error {
	if !status.Valid() {
		return ErrInvalidSettlementStatus
	}
	now := time.Now().UTC()
	res, err := r.db.ExecContext(ctx, `
		UPDATE merchants
		SET settlement_clabe = $1, settlement_clabe_status = $2, settlement_clabe_updated_at = $3, updated_at = $3
		WHERE id = $4
	`, clabe, string(status), now, merchantID)
	if err != nil {
		return fmt.Errorf("failed to set settlement account: %w", err)
	}
	return requireRowAffected(res, ErrMerchantNotFound)
}

func (r *SQLMerchantRepository) SetSettlementAccountStatus(ctx context.Context, merchantID, clabe string, status SettlementAccountStatus) error {
	if !status.Valid() {
		return ErrInvalidSettlementStatus
	}
	now := time.Now().UTC()
	res, err := r.db.ExecContext(ctx, `
		UPDATE merchants
		SET settlement_clabe_status = $1, settlement_clabe_updated_at = $2, updated_at = $2
		WHERE id = $3 AND settlement_clabe = $4
	`, string(status), now, merchantID, clabe)
	if err != nil {
		return fmt.Errorf("failed to set settlement account status: %w", err)
	}
	return requireRowAffected(res, ErrSettlementCLABEChanged)
}

// ----------------------------------------------------------------------------
// SQLIdempotencyRepository
// ----------------------------------------------------------------------------

type SQLIdempotencyRepository struct {
	db *sql.DB
}

func NewSQLIdempotencyRepository(db *sql.DB) *SQLIdempotencyRepository {
	return &SQLIdempotencyRepository{db: db}
}

func (r *SQLIdempotencyRepository) Get(ctx context.Context, key string) (int, []byte, bool, error) {
	var statusCode int
	var body []byte
	var expiresAt time.Time

	err := r.db.QueryRowContext(ctx, `
		SELECT status_code, response_body, expires_at
		FROM idempotency_keys
		WHERE idempotency_key = $1
	`, key).Scan(&statusCode, &body, &expiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil, false, nil
		}
		return 0, nil, false, fmt.Errorf("failed to query idempotency key: %w", err)
	}

	if time.Now().After(expiresAt) {
		return 0, nil, false, nil
	}
	return statusCode, body, true, nil
}

func (r *SQLIdempotencyRepository) Set(ctx context.Context, key string, statusCode int, body []byte, ttl time.Duration) error {
	now := time.Now().UTC()
	expiresAt := now.Add(ttl)

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO idempotency_keys (idempotency_key, status_code, response_body, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (idempotency_key) DO UPDATE
		SET status_code = EXCLUDED.status_code,
		    response_body = EXCLUDED.response_body,
		    expires_at = EXCLUDED.expires_at
	`, key, statusCode, body, expiresAt, now)
	if err != nil {
		return fmt.Errorf("failed to save idempotency key: %w", err)
	}
	return nil
}

// isUniqueViolation reports whether err is a PostgreSQL unique_violation (23505).
func isUniqueViolation(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "23505") || strings.Contains(msg, "unique") || strings.Contains(msg, "duplicate key")
}

// isForeignKeyViolation reports whether err is a PostgreSQL foreign_key_violation (23503).
func isForeignKeyViolation(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "23503") || strings.Contains(msg, "foreign key")
}

func requireRowAffected(res sql.Result, notFound error) error {
	n, err := res.RowsAffected()
	if err != nil {
		return nil // driver cannot report affected rows; assume success
	}
	if n == 0 {
		return notFound
	}
	return nil
}
