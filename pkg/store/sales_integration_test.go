package store

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"softpos/migrations"
	"softpos/pkg/ledger"
	"softpos/pkg/store/pgtest"
)

// migrationsUpTo returns the embedded migrations with version <= max.
func migrationsUpTo(t *testing.T, max string) fstest.MapFS {
	t.Helper()
	out := fstest.MapFS{}
	entries, _ := fs.ReadDir(migrations.FS, ".")
	for _, e := range entries {
		v, _, _ := strings.Cut(e.Name(), "_")
		if v <= max {
			b, _ := fs.ReadFile(migrations.FS, e.Name())
			out[e.Name()] = &fstest.MapFile{Data: b}
		}
	}
	return out
}

// TestPGIntegration_SalesBackfillAndUnifiedListing: SPEI / CoDi journals
// posted before migration 013 are backfilled into merchant_sales; new sales
// recorded in the money transaction; ListSales merges card payments, pages by
// (created_at, id) and filters for the backoffice.
func TestPGIntegration_SalesBackfillAndUnifiedListing(t *testing.T) {
	db := pgtest.Open(t)
	ctx := context.Background()
	if err := RunMigrationsFS(ctx, db, migrationsUpTo(t, "012")); err != nil {
		t.Fatal(err)
	}
	led := NewSQLLedgerRepository(db)
	for _, code := range []string{"ASSET:STP_CONCENTRATOR", "LIABILITY:MERCHANT:m_s1:AVAILABLE", "LIABILITY:MERCHANT:m_s1:TAX_ESCROW", "REVENUE:FEES", "ASSET:RBF_LOANS_RECEIVABLE:m_s1", "ASSET:ACQUIRER_RECEIVABLE"} {
		typ := ledger.AccountLiability
		if strings.HasPrefix(code, "ASSET") {
			typ = ledger.AccountAsset
		} else if strings.HasPrefix(code, "REVENUE") {
			typ = ledger.AccountRevenue
		}
		if err := led.CreateAccount(ctx, ledger.Account{ID: code, Code: code, Type: typ, Name: code}); err != nil {
			t.Fatal(err)
		}
	}
	ms := NewSQLMoneyStore(db)
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	if err := ms.InTx(ctx, func(tx MoneyTx) error {
		spei, _ := ledger.NewJournalEntry("spei_in_40012:OLD1", t0, "SPEI", []ledger.Entry{
			{AccountID: "ASSET:STP_CONCENTRATOR", Direction: ledger.Debit, Money: ledger.NewMoney(7000, "MXN")},
			{AccountID: "LIABILITY:MERCHANT:m_s1:AVAILABLE", Direction: ledger.Credit, Money: ledger.NewMoney(7000, "MXN")},
		}, map[string]string{"clave_rastreo": "OLD1"})
		if err := tx.PostJournal(ctx, spei); err != nil {
			return err
		}
		codi, err := ledger.NewSplitPaymentJournal("codi_OLDCODI", t0.Add(time.Minute), 11600, "m_s1", 0, 1700, 0)
		if err != nil {
			return err
		}
		codi.Metadata = map[string]string{"tracking_key": "OLDCODI", "merchant_id": "m_s1"}
		return tx.PostJournal(ctx, codi)
	}); err != nil {
		t.Fatalf("pre-013 journals: %v", err)
	}
	if err := RunMigrationsFS(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	got, err := ms.ListSales(ctx, SalesQuery{MerchantID: "m_s1"})
	if err != nil || len(got) != 2 || got[0].ID != "codi_OLDCODI" || got[0].AmountCents != 11600 || got[1].ID != "spei_40012:OLD1" || got[1].AmountCents != 7000 {
		t.Fatalf("backfill: %+v %v", got, err)
	}

	// New sales and card payments in the same view.
	if err := ms.InTx(ctx, func(tx MoneyTx) error {
		for i, id := range []string{"spei_40012:N1", "spei_40012:N2"} {
			if err := tx.RecordSale(ctx, SaleRecord{ID: id, MerchantID: "m_s1", Kind: SaleSPEI, Status: "RECEIVED", AmountCents: int64(100 + i), CreatedAt: t0.Add(time.Hour)}); err != nil {
				return err
			}
		}
		// idempotent
		if err := tx.RecordSale(ctx, SaleRecord{ID: "spei_40012:N1", MerchantID: "m_s1", Kind: SaleSPEI, Status: "RECEIVED", AmountCents: 999, CreatedAt: t0}); err != nil {
			return err
		}
		_, _, err := tx.CreateCardPayment(ctx, CardPaymentRecord{ID: "cp_s1", MerchantID: "m_s1", Provider: "mock", IntentIdempotencyKey: "k1",
			RequestHash: "h", AmountCents: 500, Currency: "MXN", Status: CardCreated})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var pages [][]SaleRecord
	var cursor *SalesCursor
	for {
		page, err := ms.ListSales(ctx, SalesQuery{MerchantID: "m_s1", Before: cursor, Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		pages = append(pages, page)
		last := page[len(page)-1]
		cursor = &SalesCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	var ids []string
	for _, p := range pages {
		for _, r := range p {
			ids = append(ids, r.ID)
		}
	}
	// card created now (newest), then the two tied at t0+1h by id desc, then backfill.
	want := []string{"cp_s1", "spei_40012:N2", "spei_40012:N1", "codi_OLDCODI", "spei_40012:OLD1"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("pages = %v, want %v", ids, want)
	}
	if n1, _ := ms.ListSales(ctx, SalesQuery{ID: "spei_40012:N1"}); len(n1) != 1 || n1[0].AmountCents != 100 {
		t.Fatalf("by id (first write wins): %+v", n1)
	}
	if cards, _ := ms.ListSales(ctx, SalesQuery{Kind: SaleCard}); len(cards) != 1 || cards[0].CardPaymentID != "cp_s1" {
		t.Fatalf("kind filter: %+v", cards)
	}
	if ranged, _ := ms.ListSales(ctx, SalesQuery{From: t0, To: t0.Add(30 * time.Minute)}); len(ranged) != 2 {
		t.Fatalf("date range: %+v", ranged)
	}
}
