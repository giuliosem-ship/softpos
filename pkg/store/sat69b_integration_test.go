package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"softpos/migrations"
	"softpos/pkg/identity"
	"softpos/pkg/store/pgtest"
)

// TestPGIntegration_SAT69BAtomicSwap: loads the SAT fixture into a real
// PostgreSQL, checks the shrink guard and that concurrent readers only ever
// see the complete old or the complete new list while it is swapped.
func TestPGIntegration_SAT69BAtomicSwap(t *testing.T) {
	db := pgtest.Open(t)
	ctx := context.Background()
	if err := RunMigrationsFS(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	repo := NewSQLBlacklistRepository(db)
	// A pre-ingestion row (AddEntry) counts as the old list.
	if err := repo.AddEntry(ctx, identity.BlacklistEntry{RFC: "OLD010101AAA", Status: identity.BlacklistDefinitivo}); err != nil {
		t.Fatal(err)
	}
	full, err := os.ReadFile(filepath.Join("..", "identity", "testdata", "sat69b_latin1.csv"))
	if err != nil {
		t.Fatal(err)
	}
	syncer := &identity.List69BSyncer{Store: repo, MaxShrinkPct: 50}
	res, err := syncer.Load(ctx, full, "file:latin1", "it", false)
	if err != nil || res.Load.Status != identity.Load69BLoaded || res.Load.RowCount != 6 || res.Load.PreviousCount != 1 {
		t.Fatalf("load: %+v %v", res.Load, err)
	}
	svc := identity.NewBlacklistService()
	svc.SetRepository(repo)
	if st, err := svc.Lookup(ctx, "ZURC660122LUA"); err != nil || st != identity.BlacklistPresunto {
		t.Fatalf("lookup presunto: %q %v", st, err)
	}
	if st, err := svc.Lookup(ctx, "OLD010101AAA"); err != nil || st != identity.BlacklistClean {
		t.Fatalf("old rows must be swapped out: %q %v", st, err)
	}
	var nombre string
	if err := db.QueryRow(`SELECT nombre FROM sat_blacklist WHERE rfc = 'AAA080808HL8'`).Scan(&nombre); err != nil || nombre != "ASESORES EN AVALÚOS Y ACTIVOS, S.A. DE C.V." {
		t.Fatalf("nombre = %q %v", nombre, err)
	}
	meta, found, err := repo.List69BMeta(ctx)
	if err != nil || !found || meta.RowCount != 6 || meta.Published != "31 de octubre de 2026" || meta.MalformedCount != 3 {
		t.Fatalf("meta: %+v %v", meta, err)
	}
	if res, err := syncer.Load(ctx, full, "file:latin1", "it", false); err != nil || res.Load.Status != identity.Load69BUnchanged {
		t.Fatalf("unchanged: %+v %v", res.Load, err)
	}

	small, _ := os.ReadFile(filepath.Join("..", "identity", "testdata", "sat69b_utf8_bom_small.csv"))
	if _, err := syncer.Load(ctx, small, "file:small", "it", false); !errors.Is(err, identity.ErrList69BShrank) {
		t.Fatalf("shrink: %v", err)
	}
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM sat_blacklist`).Scan(&n)
	if n != 6 {
		t.Fatalf("refused load changed the list: %d rows", n)
	}

	// Concurrent readers during repeated swaps 6 <-> 2 rows.
	var stop atomic.Bool
	var bad atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				var c int
				if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sat_blacklist`).Scan(&c); err == nil && c != 6 && c != 2 {
					bad.Add(1)
				}
			}
		}()
	}
	for i := 0; i < 6; i++ {
		file := small
		if i%2 == 1 {
			file = full
		}
		if _, err := syncer.Load(ctx, file, "file:swap", "it", true); err != nil {
			t.Fatal(err)
		}
	}
	stop.Store(true)
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("readers saw %d partial lists", bad.Load())
	}
	loads, err := repo.ListLoads(ctx, 50)
	if err != nil || len(loads) != 9 || loads[len(loads)-1].Status != identity.Load69BLoaded {
		t.Fatalf("history: %d %v", len(loads), err)
	}
}
