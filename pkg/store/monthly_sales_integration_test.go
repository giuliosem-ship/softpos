package store

import (
	"context"
	"sync"
	"testing"
)

// Concurrent first-of-month increments must not lose sales: with no row yet,
// SELECT ... FOR UPDATE locks nothing and both transactions try to INSERT.
func TestPGIntegration_MonthlySalesConcurrentFirstIncrement(t *testing.T) {
	db := openMigratedPG(t)
	ctx := context.Background()
	repo := NewSQLMerchantRepository(db)
	const workers = 16
	for round := 0; round < 5; round++ {
		ym := "2026-0" + string(rune('1'+round))
		id := "m_conc_" + string(rune('a'+round))
		if err := repo.SaveMerchant(ctx, MerchantRecord{ID: id, RFC: "AAA010101AAA", Name: "x", IsRESICO: true}); err != nil {
			t.Fatalf("SaveMerchant: %v", err)
		}
		var wg sync.WaitGroup
		errs := make(chan error, workers)
		start := make(chan struct{})
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if _, err := repo.AddMonthlySalesCents(ctx, id, ym, 100); err != nil {
					errs <- err
				}
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Errorf("round %d: concurrent AddMonthlySalesCents failed: %v", round, err)
		}
		got, err := repo.GetMonthlySalesCents(ctx, id, ym)
		if err != nil {
			t.Fatalf("GetMonthlySalesCents: %v", err)
		}
		if got != workers*100 {
			t.Fatalf("round %d: expected %d cents accumulated, got %d (lost updates)", round, workers*100, got)
		}
	}
}
