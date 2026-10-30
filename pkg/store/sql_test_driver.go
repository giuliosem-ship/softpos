package store

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

func init() {
	sql.Register("mockpostgres", &mockDriver{})
}

type mockDriver struct{}

func (d *mockDriver) Open(name string) (driver.Conn, error) {
	return getSharedDB(name).newConn(), nil
}

// In-memory mock database state
type mockDB struct {
	mu            sync.RWMutex
	accounts      map[string]*mockAccount
	journals      map[string]*mockJournal
	entries       []mockEntry
	blacklist     map[string]*mockBlacklist
	merchants     map[string]*mockMerchant
	monthlySales  map[string]int64 // key: merchantID:yearMonth
	idempotency   map[string]*mockIdemp
	migrations    map[string]time.Time
	rowLocks      map[string]*sync.Mutex
	locksMu       sync.Mutex
}

type mockAccount struct {
	id          string
	code        string
	name        string
	accountType string
	balance     int64
	currency    string
}

type mockJournal struct {
	id          string
	description string
	metaJSON    string
	createdAt   time.Time
}

type mockEntry struct {
	id        int64
	journalID string
	accountID string
	direction string
	amount    int64
	currency  string
}

type mockBlacklist struct {
	rfc    string
	status string
	oficio string
}

type mockMerchant struct {
	id        string
	rfc       string
	name      string
	isRESICO  bool
	clabe     string
	csdJSON   []byte
	createdAt time.Time
	updatedAt time.Time
}

type mockIdemp struct {
	key        string
	statusCode int
	body       []byte
	expiresAt  time.Time
}

var (
	sharedDBs   = make(map[string]*mockDB)
	sharedDBsMu sync.Mutex
)

func getSharedDB(name string) *mockDB {
	sharedDBsMu.Lock()
	defer sharedDBsMu.Unlock()
	if db, ok := sharedDBs[name]; ok {
		return db
	}
	db := &mockDB{
		accounts:     make(map[string]*mockAccount),
		journals:     make(map[string]*mockJournal),
		blacklist:    make(map[string]*mockBlacklist),
		merchants:    make(map[string]*mockMerchant),
		monthlySales: make(map[string]int64),
		idempotency:  make(map[string]*mockIdemp),
		migrations:   make(map[string]time.Time),
		rowLocks:     make(map[string]*sync.Mutex),
	}
	sharedDBs[name] = db
	return db
}

func (db *mockDB) getRowLock(key string) *sync.Mutex {
	db.locksMu.Lock()
	defer db.locksMu.Unlock()
	lock, ok := db.rowLocks[key]
	if !ok {
		lock = &sync.Mutex{}
		db.rowLocks[key] = lock
	}
	return lock
}

func (db *mockDB) newConn() *mockConn {
	return &mockConn{db: db}
}

type mockConn struct {
	db       *mockDB
	inTx     bool
	heldLock []*sync.Mutex
}

func (c *mockConn) Prepare(query string) (driver.Stmt, error) {
	return &mockStmt{conn: c, query: query}, nil
}

func (c *mockConn) Close() error {
	c.releaseLocks()
	return nil
}

func (c *mockConn) releaseLocks() {
	for _, l := range c.heldLock {
		l.Unlock()
	}
	c.heldLock = nil
}

func (c *mockConn) Begin() (driver.Tx, error) {
	c.inTx = true
	return c, nil
}

func (c *mockConn) Commit() error {
	c.inTx = false
	c.releaseLocks()
	return nil
}

func (c *mockConn) Rollback() error {
	c.inTx = false
	c.releaseLocks()
	return nil
}

type mockStmt struct {
	conn  *mockConn
	query string
}

func (s *mockStmt) Close() error { return nil }
func (s *mockStmt) NumInput() int { return -1 }

func cleanSQL(s string) string {
	return strings.Join(strings.Fields(strings.ToUpper(s)), " ")
}

func (s *mockStmt) Exec(args []driver.Value) (driver.Result, error) {
	q := cleanSQL(s.query)
	db := s.conn.db

	switch {
	case strings.HasPrefix(q, "CREATE TABLE"):
		return driver.RowsAffected(0), nil

	case strings.HasPrefix(q, "INSERT INTO SCHEMA_MIGRATIONS"):
		version := fmt.Sprint(args[0])
		db.mu.Lock()
		db.migrations[version] = time.Now()
		db.mu.Unlock()
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(q, "INSERT INTO LEDGER_ACCOUNTS"):
		id := fmt.Sprint(args[0])
		code := fmt.Sprint(args[1])
		name := fmt.Sprint(args[2])
		accType := fmt.Sprint(args[3])
		db.mu.Lock()
		defer db.mu.Unlock()
		if _, exists := db.accounts[code]; exists {
			return nil, errors.New("duplicate key value violates unique constraint")
		}
		db.accounts[code] = &mockAccount{
			id:          id,
			code:        code,
			name:        name,
			accountType: accType,
			balance:     0,
			currency:    "MXN",
		}
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(q, "INSERT INTO LEDGER_JOURNALS"):
		id := fmt.Sprint(args[0])
		desc := fmt.Sprint(args[1])
		meta := fmt.Sprint(args[2])
		createdAt := args[3].(time.Time)
		db.mu.Lock()
		db.journals[id] = &mockJournal{id: id, description: desc, metaJSON: meta, createdAt: createdAt}
		db.mu.Unlock()
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(q, "INSERT INTO LEDGER_ENTRIES"):
		jID := fmt.Sprint(args[0])
		accID := fmt.Sprint(args[1])
		dir := fmt.Sprint(args[2])
		amt := args[3].(int64)
		curr := fmt.Sprint(args[4])
		db.mu.Lock()
		db.entries = append(db.entries, mockEntry{
			id:        int64(len(db.entries) + 1),
			journalID: jID,
			accountID: accID,
			direction: dir,
			amount:    amt,
			currency:  curr,
		})
		db.mu.Unlock()
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(q, "UPDATE LEDGER_ACCOUNTS"):
		newBal := args[0].(int64)
		accID := fmt.Sprint(args[2])
		db.mu.Lock()
		for _, acc := range db.accounts {
			if acc.id == accID || acc.code == accID {
				acc.balance = newBal
			}
		}
		db.mu.Unlock()
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(q, "INSERT INTO SAT_BLACKLIST"):
		rfc := fmt.Sprint(args[0])
		status := fmt.Sprint(args[1])
		oficio := fmt.Sprint(args[2])
		db.mu.Lock()
		db.blacklist[rfc] = &mockBlacklist{rfc: rfc, status: status, oficio: oficio}
		db.mu.Unlock()
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(q, "INSERT INTO MERCHANTS"):
		id := fmt.Sprint(args[0])
		rfc := fmt.Sprint(args[1])
		name := fmt.Sprint(args[2])
		isRESICO := args[3].(bool)
		clabe := ""
		if args[4] != nil {
			clabe = fmt.Sprint(args[4])
		}
		var csd []byte
		if args[5] != nil {
			csd = args[5].([]byte)
		}
		db.mu.Lock()
		db.merchants[id] = &mockMerchant{
			id: id, rfc: rfc, name: name, isRESICO: isRESICO, clabe: clabe, csdJSON: csd,
		}
		db.mu.Unlock()
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(q, "UPDATE MERCHANTS SET VIRTUAL_CLABE"):
		clabe := fmt.Sprint(args[0])
		id := fmt.Sprint(args[2])
		db.mu.Lock()
		if m, ok := db.merchants[id]; ok {
			m.clabe = clabe
		}
		db.mu.Unlock()
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(q, "UPDATE MERCHANTS SET ENCRYPTED_CSD_JSON"):
		csd := args[0].([]byte)
		id := fmt.Sprint(args[2])
		db.mu.Lock()
		if m, ok := db.merchants[id]; ok {
			m.csdJSON = csd
		}
		db.mu.Unlock()
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(q, "INSERT INTO MERCHANT_MONTHLY_SALES"):
		mID := fmt.Sprint(args[0])
		ym := fmt.Sprint(args[1])
		cents := args[2].(int64)
		db.mu.Lock()
		db.monthlySales[mID+":"+ym] = cents
		db.mu.Unlock()
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(q, "UPDATE MERCHANT_MONTHLY_SALES"):
		cents := args[0].(int64)
		mID := fmt.Sprint(args[2])
		ym := fmt.Sprint(args[3])
		db.mu.Lock()
		db.monthlySales[mID+":"+ym] = cents
		db.mu.Unlock()
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(q, "INSERT INTO IDEMPOTENCY_KEYS"):
		key := fmt.Sprint(args[0])
		status := int(args[1].(int64))
		body := args[2].([]byte)
		expiresAt := args[3].(time.Time)
		db.mu.Lock()
		db.idempotency[key] = &mockIdemp{
			key: key, statusCode: status, body: body, expiresAt: expiresAt,
		}
		db.mu.Unlock()
		return driver.RowsAffected(1), nil
	}

	return driver.RowsAffected(0), nil
}

func (s *mockStmt) Query(args []driver.Value) (driver.Rows, error) {
	q := cleanSQL(s.query)
	db := s.conn.db

	switch {
	case strings.Contains(q, "SELECT EXISTS(SELECT 1 FROM SCHEMA_MIGRATIONS"):
		version := fmt.Sprint(args[0])
		db.mu.RLock()
		_, exists := db.migrations[version]
		db.mu.RUnlock()
		return &singleRow{[1]driver.Value{exists}}, nil

	case strings.Contains(q, "FROM LEDGER_ACCOUNTS") && strings.Contains(q, "FOR UPDATE"):
		accID := fmt.Sprint(args[0])
		if s.conn.inTx {
			// Acquire row-level lock
			lock := db.getRowLock("acc:" + accID)
			lock.Lock()
			s.conn.heldLock = append(s.conn.heldLock, lock)
		}
		db.mu.RLock()
		defer db.mu.RUnlock()
		for _, acc := range db.accounts {
			if acc.id == accID || acc.code == accID {
				return &singleRow{[3]driver.Value{acc.id, acc.accountType, acc.balance}}, nil
			}
		}
		return &emptyRows{}, nil

	case strings.Contains(q, "SELECT ID, CODE, NAME, ACCOUNT_TYPE FROM LEDGER_ACCOUNTS"):
		accID := fmt.Sprint(args[0])
		db.mu.RLock()
		defer db.mu.RUnlock()
		for _, acc := range db.accounts {
			if acc.id == accID || acc.code == accID {
				return &singleRow{[4]driver.Value{acc.id, acc.code, acc.name, acc.accountType}}, nil
			}
		}
		return &emptyRows{}, nil

	case strings.Contains(q, "SELECT BALANCE_CENTS, ACCOUNT_TYPE, CURRENCY FROM LEDGER_ACCOUNTS"):
		accID := fmt.Sprint(args[0])
		db.mu.RLock()
		defer db.mu.RUnlock()
		for _, acc := range db.accounts {
			if acc.id == accID || acc.code == accID {
				return &singleRow{[3]driver.Value{acc.balance, acc.accountType, acc.currency}}, nil
			}
		}
		return &emptyRows{}, nil

	case strings.Contains(q, "FROM SAT_BLACKLIST"):
		rfc := fmt.Sprint(args[0])
		db.mu.RLock()
		defer db.mu.RUnlock()
		if b, ok := db.blacklist[rfc]; ok {
			return &singleRow{[2]driver.Value{b.status, b.oficio}}, nil
		}
		return &emptyRows{}, nil

	case strings.Contains(q, "SELECT ID, RFC, NAME, IS_RESICO, VIRTUAL_CLABE, ENCRYPTED_CSD_JSON") && strings.Contains(q, "WHERE ID = $1"):
		id := fmt.Sprint(args[0])
		db.mu.RLock()
		defer db.mu.RUnlock()
		if m, ok := db.merchants[id]; ok {
			var clabeVal any
			if m.clabe != "" {
				clabeVal = m.clabe
			}
			return &singleRow{[8]driver.Value{m.id, m.rfc, m.name, m.isRESICO, clabeVal, m.csdJSON, time.Now(), time.Now()}}, nil
		}
		return &emptyRows{}, nil

	case strings.Contains(q, "WHERE VIRTUAL_CLABE = $1"):
		clabe := fmt.Sprint(args[0])
		db.mu.RLock()
		defer db.mu.RUnlock()
		for _, m := range db.merchants {
			if m.clabe == clabe {
				return &singleRow{[8]driver.Value{m.id, m.rfc, m.name, m.isRESICO, m.clabe, m.csdJSON, time.Now(), time.Now()}}, nil
			}
		}
		return &emptyRows{}, nil

	case strings.Contains(q, "SELECT VIRTUAL_CLABE FROM MERCHANTS"):
		id := fmt.Sprint(args[0])
		db.mu.RLock()
		defer db.mu.RUnlock()
		if m, ok := db.merchants[id]; ok {
			var clabeVal any
			if m.clabe != "" {
				clabeVal = m.clabe
			}
			return &singleRow{[1]driver.Value{clabeVal}}, nil
		}
		return &emptyRows{}, nil

	case strings.Contains(q, "SELECT ENCRYPTED_CSD_JSON FROM MERCHANTS"):
		id := fmt.Sprint(args[0])
		db.mu.RLock()
		defer db.mu.RUnlock()
		if m, ok := db.merchants[id]; ok {
			return &singleRow{[1]driver.Value{m.csdJSON}}, nil
		}
		return &emptyRows{}, nil

	case strings.Contains(q, "FROM MERCHANT_MONTHLY_SALES"):
		mID := fmt.Sprint(args[0])
		ym := fmt.Sprint(args[1])
		if strings.Contains(q, "FOR UPDATE") && s.conn.inTx {
			lock := db.getRowLock("sales:" + mID + ":" + ym)
			lock.Lock()
			s.conn.heldLock = append(s.conn.heldLock, lock)
		}
		db.mu.RLock()
		defer db.mu.RUnlock()
		if sales, ok := db.monthlySales[mID+":"+ym]; ok {
			return &singleRow{[1]driver.Value{sales}}, nil
		}
		return &emptyRows{}, nil

	case strings.Contains(q, "FROM IDEMPOTENCY_KEYS"):
		key := fmt.Sprint(args[0])
		db.mu.RLock()
		defer db.mu.RUnlock()
		if idemp, ok := db.idempotency[key]; ok {
			return &singleRow{[3]driver.Value{int64(idemp.statusCode), idemp.body, idemp.expiresAt}}, nil
		}
		return &emptyRows{}, nil
	}

	return &emptyRows{}, nil
}

type emptyRows struct{}

func (r *emptyRows) Columns() []string              { return []string{} }
func (r *emptyRows) Close() error                  { return nil }
func (r *emptyRows) Next(dest []driver.Value) error { return io.EOF }

type singleRow struct {
	values any
}

func (r *singleRow) Columns() []string {
	switch v := r.values.(type) {
	case [1]driver.Value:
		return []string{"col1"}
	case [2]driver.Value:
		return []string{"col1", "col2"}
	case [3]driver.Value:
		return []string{"col1", "col2", "col3"}
	case [4]driver.Value:
		return []string{"col1", "col2", "col3", "col4"}
	case [8]driver.Value:
		return []string{"col1", "col2", "col3", "col4", "col5", "col6", "col7", "col8"}
	default:
		_ = v
		return []string{}
	}
}

func (r *singleRow) Close() error { return nil }

func (r *singleRow) Next(dest []driver.Value) error {
	if r.values == nil {
		return io.EOF
	}
	switch v := r.values.(type) {
	case [1]driver.Value:
		dest[0] = v[0]
	case [2]driver.Value:
		dest[0], dest[1] = v[0], v[1]
	case [3]driver.Value:
		dest[0], dest[1], dest[2] = v[0], v[1], v[2]
	case [4]driver.Value:
		dest[0], dest[1], dest[2], dest[3] = v[0], v[1], v[2], v[3]
	case [8]driver.Value:
		for i := 0; i < 8; i++ {
			dest[i] = v[i]
		}
	}
	r.values = nil
	return nil
}

var _ driver.Driver = &mockDriver{}
var _ driver.Conn = &mockConn{}
var _ driver.Tx = &mockConn{}
var _ driver.Stmt = &mockStmt{}
