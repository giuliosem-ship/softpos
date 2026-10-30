package store

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"softpos/pkg/identity"
	"softpos/pkg/ledger"
)

func setupTestDB(t *testing.T, dbName string) (*sql.DB, func()) {
	t.Helper()
	db, err := sql.Open("mockpostgres", dbName)
	if err != nil {
		t.Fatalf("failed to open mock DB: %v", err)
	}
	return db, func() {
		_ = db.Close()
	}
}

func TestMigrationsRunner(t *testing.T) {
	db, cleanup := setupTestDB(t, "test_migrations")
	defer cleanup()

	tmpDir := t.TempDir()
	upScript := `
		CREATE TABLE ledger_accounts (id VARCHAR(128) PRIMARY KEY);
		CREATE TABLE sat_blacklist (rfc VARCHAR(13) PRIMARY KEY);
	`
	if err := os.WriteFile(filepath.Join(tmpDir, "001_initial.up.sql"), []byte(upScript), 0644); err != nil {
		t.Fatalf("failed to write migration file: %v", err)
	}

	ctx := context.Background()
	if err := RunMigrations(ctx, db, tmpDir); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}

	// Running a second time should be idempotent
	if err := RunMigrations(ctx, db, tmpDir); err != nil {
		t.Fatalf("second RunMigrations failed: %v", err)
	}
}

func TestSQLLedgerRepository_PessimisticLockingAndTransactions(t *testing.T) {
	db, cleanup := setupTestDB(t, "test_ledger_repo")
	defer cleanup()

	repo := NewSQLLedgerRepository(db)
	ctx := context.Background()

	// 1. Create accounts
	accAvail := ledger.Account{ID: "LIABILITY:MERCHANT:m1:AVAILABLE", Code: "LIABILITY:MERCHANT:m1:AVAILABLE", Name: "Merchant Available", Type: ledger.AccountLiability}
	accAcq := ledger.Account{ID: "ASSET:ACQUIRER_RECEIVABLE", Code: "ASSET:ACQUIRER_RECEIVABLE", Name: "Acquirer Receivable", Type: ledger.AccountAsset}
	accTax := ledger.Account{ID: "LIABILITY:MERCHANT:m1:TAX_ESCROW", Code: "LIABILITY:MERCHANT:m1:TAX_ESCROW", Name: "Merchant Escrow", Type: ledger.AccountLiability}

	if err := repo.CreateAccount(ctx, accAvail); err != nil {
		t.Fatalf("CreateAccount failed: %v", err)
	}
	if err := repo.CreateAccount(ctx, accAcq); err != nil {
		t.Fatalf("CreateAccount failed: %v", err)
	}
	if err := repo.CreateAccount(ctx, accTax); err != nil {
		t.Fatalf("CreateAccount failed: %v", err)
	}

	// Duplicate account test
	if err := repo.CreateAccount(ctx, accAvail); err == nil {
		t.Errorf("expected ErrDuplicateAccount, got nil")
	}

	// 2. Post atomic transaction with pessimistic locking (SELECT ... FOR UPDATE)
	// $1,000.00 MXN gross sale (100,000 cents):
	// Debit Asset: Acquirer Receivable 100,000 cents
	// Credit Liability: Merchant Available 84,000 cents
	// Credit Liability: Merchant Tax Escrow 16,000 cents
	entries := []ledger.Entry{
		{AccountID: accAcq.ID, Direction: ledger.Debit, Money: ledger.NewMoney(100000, "MXN")},
		{AccountID: accAvail.ID, Direction: ledger.Credit, Money: ledger.NewMoney(84000, "MXN")},
		{AccountID: accTax.ID, Direction: ledger.Credit, Money: ledger.NewMoney(16000, "MXN")},
	}
	journal, err := ledger.NewJournalEntry("jnl_001", time.Now().UTC(), "POS Sale Split", entries, map[string]string{"sale_id": "s_123"})
	if err != nil {
		t.Fatalf("failed to create journal entry: %v", err)
	}

	if err := repo.PostTransaction(ctx, journal); err != nil {
		t.Fatalf("PostTransaction failed: %v", err)
	}

	// Verify balances
	balAcq, err := repo.GetBalance(ctx, accAcq.ID)
	if err != nil || balAcq.AmountCents != 100000 {
		t.Errorf("expected Acquirer balance 100000, got %v (err: %v)", balAcq, err)
	}
	balAvail, err := repo.GetBalance(ctx, accAvail.ID)
	if err != nil || balAvail.AmountCents != 84000 {
		t.Errorf("expected Available balance 84000, got %v (err: %v)", balAvail, err)
	}
	balTax, err := repo.GetBalance(ctx, accTax.ID)
	if err != nil || balTax.AmountCents != 16000 {
		t.Errorf("expected Tax Escrow balance 16000, got %v (err: %v)", balTax, err)
	}

	// 3. Concurrency test: multiple goroutines posting transactions on the same account
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			e := []ledger.Entry{
				{AccountID: accAcq.ID, Direction: ledger.Debit, Money: ledger.NewMoney(1000, "MXN")},
				{AccountID: accAvail.ID, Direction: ledger.Credit, Money: ledger.NewMoney(1000, "MXN")},
			}
			j, _ := ledger.NewJournalEntry(fmt.Sprintf("concurrent_jnl_%d", idx), time.Now().UTC(), "concurrent sale", e, nil)
			_ = repo.PostTransaction(ctx, j)
		}(i)
	}
	wg.Wait()

	balFinal, _ := repo.GetBalance(ctx, accAvail.ID)
	expectedFinal := int64(84000 + 5*1000)
	if balFinal.AmountCents != expectedFinal {
		t.Errorf("expected final balance %d, got %d", expectedFinal, balFinal.AmountCents)
	}
}

func TestSQLBlacklistRepository(t *testing.T) {
	db, cleanup := setupTestDB(t, "test_blacklist_repo")
	defer cleanup()

	repo := NewSQLBlacklistRepository(db)
	ctx := context.Background()

	// 1. Query clean RFC
	cleanEntry, exists, err := repo.GetEntry(ctx, "GARM850101ABC")
	if err != nil || exists || cleanEntry.Status != identity.BlacklistClean {
		t.Errorf("expected clean unlisted RFC, got %+v (exists: %v, err: %v)", cleanEntry, exists, err)
	}

	// 2. Add 69-B Definitivo entry
	badEntry := identity.BlacklistEntry{
		RFC:       "EFO110101XYZ",
		Status:    identity.BlacklistDefinitivo,
		OficioSAT: "500-05-2023-9999",
	}
	if err := repo.AddEntry(ctx, badEntry); err != nil {
		t.Fatalf("AddEntry failed: %v", err)
	}

	got, exists, err := repo.GetEntry(ctx, "EFO110101XYZ")
	if err != nil || !exists || got.Status != identity.BlacklistDefinitivo || got.OficioSAT != "500-05-2023-9999" {
		t.Errorf("expected Definitivo status, got %+v (exists: %v, err: %v)", got, exists, err)
	}
}

func TestSQLMerchantRepository_CatalogAndPessimisticSalesTiers(t *testing.T) {
	db, cleanup := setupTestDB(t, "test_merchant_repo")
	defer cleanup()

	repo := NewSQLMerchantRepository(db)
	ctx := context.Background()

	// 1. Save and Get Merchant
	m := MerchantRecord{
		ID:           "merch_001",
		RFC:          "TAQ850101XYZ",
		Name:         "Taqueria El Pastor",
		IsRESICO:     true,
		VirtualCLABE: "646180123400000007",
	}
	if err := repo.SaveMerchant(ctx, m); err != nil {
		t.Fatalf("SaveMerchant failed: %v", err)
	}

	got, exists, err := repo.GetMerchant(ctx, "merch_001")
	if err != nil || !exists || got.Name != "Taqueria El Pastor" {
		t.Errorf("expected merchant retrieved, got %+v (exists: %v, err: %v)", got, exists, err)
	}

	byCLABE, exists, err := repo.GetMerchantByCLABE(ctx, "646180123400000007")
	if err != nil || !exists || byCLABE.ID != "merch_001" {
		t.Errorf("expected merchant by CLABE, got %+v (exists: %v, err: %v)", byCLABE, exists, err)
	}

	// 2. Save and Get Merchant CSD
	testCSD := []byte(`{"encrypted_key":"deadbeef","cert":"cafe"}`)
	if err := repo.SaveMerchantCSD(ctx, "merch_001", testCSD); err != nil {
		t.Fatalf("SaveMerchantCSD failed: %v", err)
	}
	gotCSD, exists, err := repo.GetMerchantCSD(ctx, "merch_001")
	if err != nil || !exists || string(gotCSD) != string(testCSD) {
		t.Errorf("expected CSD retrieved, got %s (exists: %v, err: %v)", string(gotCSD), exists, err)
	}

	// 3. Add monthly sales with pessimistic locking
	yearMonth := "2026-10"
	s1, err := repo.AddMonthlySalesCents(ctx, "merch_001", yearMonth, 2500000)
	if err != nil || s1 != 2500000 {
		t.Errorf("expected 2500000 sales cents, got %d (err: %v)", s1, err)
	}

	s2, err := repo.AddMonthlySalesCents(ctx, "merch_001", yearMonth, 1000000)
	if err != nil || s2 != 3500000 {
		t.Errorf("expected 3500000 accumulated sales cents, got %d (err: %v)", s2, err)
	}

	gotSales, err := repo.GetMonthlySalesCents(ctx, "merch_001", yearMonth)
	if err != nil || gotSales != 3500000 {
		t.Errorf("expected 3500000 sales cents, got %d", gotSales)
	}
}

func TestSQLIdempotencyRepository(t *testing.T) {
	db, cleanup := setupTestDB(t, "test_idemp_repo")
	defer cleanup()

	repo := NewSQLIdempotencyRepository(db)
	ctx := context.Background()

	key := "req_test_123"
	body := []byte(`{"status":"SUCCESS"}`)

	if err := repo.Set(ctx, key, 200, body, 1*time.Hour); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	status, resBody, exists, err := repo.Get(ctx, key)
	if err != nil || !exists || status != 200 || string(resBody) != string(body) {
		t.Errorf("expected key found, got status=%d body=%s (exists: %v, err: %v)", status, string(resBody), exists, err)
	}

	// Expired key returns exists = false
	if err := repo.Set(ctx, "expired_key", 200, body, -1*time.Minute); err != nil {
		t.Fatalf("Set expired failed: %v", err)
	}
	_, _, existsExp, _ := repo.Get(ctx, "expired_key")
	if existsExp {
		t.Errorf("expected expired key to not exist")
	}
}

func TestRedisIdempotencyStore_WithLocalRESPMock(t *testing.T) {
	// Start an in-process mock Redis TCP server using standard net.Listener
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen on ephemeral port: %v", err)
	}
	defer listener.Close()

	redisStorage := make(map[string][]byte)
	var storageMu sync.Mutex

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufioReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					line = strings.TrimSpace(line)
					if strings.HasPrefix(line, "*") {
						count, _ := strconv.Atoi(line[1:])
						args := make([]string, 0, count)
						for i := 0; i < count; i++ {
							_, _ = r.ReadString('\n') // $len
							arg, _ := r.ReadString('\n')
							args = append(args, strings.TrimRight(arg, "\r\n"))
						}
						if len(args) == 0 {
							continue
						}
						cmd := strings.ToUpper(args[0])
						switch cmd {
						case "PING":
							_, _ = c.Write([]byte("+PONG\r\n"))
						case "SET":
							k := args[1]
							v := args[2]
							storageMu.Lock()
							redisStorage[k] = []byte(v)
							storageMu.Unlock()
							_, _ = c.Write([]byte("+OK\r\n"))
						case "GET":
							k := args[1]
							storageMu.Lock()
							v, ok := redisStorage[k]
							storageMu.Unlock()
							if !ok {
								_, _ = c.Write([]byte("$-1\r\n"))
							} else {
								_, _ = c.Write([]byte(fmt.Sprintf("$%d\r\n%s\r\n", len(v), string(v))))
							}
						}
					}
				}
			}(conn)
		}
	}()

	client := NewRedisClient(listener.Addr().String(), "")
	defer client.Close()

	ctx := context.Background()
	if err := client.Ping(ctx); err != nil {
		t.Fatalf("Redis Ping failed: %v", err)
	}

	store := NewRedisIdempotencyStore(client)

	key := "emv:auth:pay_001"
	body := []byte(`{"authorized":true}`)

	if err := store.Set(ctx, key, 200, body, 15*time.Minute); err != nil {
		t.Fatalf("Redis Set failed: %v", err)
	}

	code, gotBody, exists, err := store.Get(ctx, key)
	if err != nil || !exists || code != 200 || string(gotBody) != string(body) {
		t.Errorf("expected 200 and body, got code=%d body=%s (exists: %v, err: %v)", code, string(gotBody), exists, err)
	}

	// Missing key
	_, _, existsMissing, err := store.Get(ctx, "non_existent")
	if err != nil || existsMissing {
		t.Errorf("expected non_existent key to return exists=false, got exists=%v (err: %v)", existsMissing, err)
	}
}

func bufioReader(c net.Conn) *bufioReaderHelper {
	return &bufioReaderHelper{r: bufio.NewReader(c)}
}

type bufioReaderHelper struct {
	r *bufio.Reader
}

func (b *bufioReaderHelper) ReadString(delim byte) (string, error) {
	return b.r.ReadString(delim)
}
