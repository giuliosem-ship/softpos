package store

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestMemoryMerchantRepository_CRUDAndCLABE(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryMerchantRepository()

	if err := repo.SaveMerchant(ctx, MerchantRecord{ID: "m1", RFC: "AAA010101AAA", Name: "A", IsRESICO: true}); err != nil {
		t.Fatalf("SaveMerchant: %v", err)
	}
	if err := repo.SetMerchantCLABE(ctx, "m1", "646180123400000007"); err != nil {
		t.Fatalf("SetMerchantCLABE: %v", err)
	}
	// Upsert without CLABE keeps the CLABE (SQL COALESCE semantics).
	_ = repo.SaveMerchant(ctx, MerchantRecord{ID: "m1", RFC: "AAA010101AAA", Name: "A2", IsRESICO: true})
	m, found, _ := repo.GetMerchant(ctx, "m1")
	if !found || m.Name != "A2" || m.VirtualCLABE != "646180123400000007" {
		t.Fatalf("unexpected merchant: %+v", m)
	}
	if byC, found, _ := repo.GetMerchantByCLABE(ctx, "646180123400000007"); !found || byC.ID != "m1" {
		t.Fatalf("GetMerchantByCLABE: %+v", byC)
	}
	if c, found, _ := repo.GetMerchantCLABE(ctx, "m1"); !found || c != "646180123400000007" {
		t.Fatalf("GetMerchantCLABE: %q", c)
	}

	_ = repo.SaveMerchant(ctx, MerchantRecord{ID: "m2", RFC: "BBB010101BBB", Name: "B"})
	if err := repo.SetMerchantCLABE(ctx, "m2", "646180123400000007"); !errors.Is(err, ErrDuplicateCLABE) {
		t.Fatalf("expected ErrDuplicateCLABE, got %v", err)
	}
	if err := repo.SetMerchantCLABE(ctx, "ghost", "646180123400000010"); !errors.Is(err, ErrMerchantNotFound) {
		t.Fatalf("expected ErrMerchantNotFound, got %v", err)
	}
	// Re-assigning m1 frees the old CLABE.
	if err := repo.SetMerchantCLABE(ctx, "m1", "646180123400000010"); err != nil {
		t.Fatalf("reassign: %v", err)
	}
	if _, found, _ := repo.GetMerchantByCLABE(ctx, "646180123400000007"); found {
		t.Fatalf("old CLABE should be released")
	}
}

func TestMemoryMerchantRepository_CSDAndMonthlySales(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryMerchantRepository()

	if err := repo.SaveMerchantCSD(ctx, "ghost", []byte(`{}`)); !errors.Is(err, ErrMerchantNotFound) {
		t.Fatalf("expected ErrMerchantNotFound, got %v", err)
	}
	_ = repo.SaveMerchant(ctx, MerchantRecord{ID: "m1", RFC: "AAA010101AAA", Name: "A"})
	csd := []byte(`{"k":"v"}`)
	if err := repo.SaveMerchantCSD(ctx, "m1", csd); err != nil {
		t.Fatalf("SaveMerchantCSD: %v", err)
	}
	csd[0] = 'X' // caller mutation must not leak into the store
	got, found, _ := repo.GetMerchantCSD(ctx, "m1")
	if !found || string(got) != `{"k":"v"}` {
		t.Fatalf("GetMerchantCSD: %s", got)
	}

	if _, err := repo.AddMonthlySalesCents(ctx, "ghost", "2026-11", 1); !errors.Is(err, ErrMerchantNotFound) {
		t.Fatalf("expected ErrMerchantNotFound, got %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = repo.AddMonthlySalesCents(ctx, "m1", "2026-11", 100)
		}()
	}
	wg.Wait()
	if total, _ := repo.GetMonthlySalesCents(ctx, "m1", "2026-11"); total != 5_000 {
		t.Fatalf("expected 5000, got %d", total)
	}
	if total, _ := repo.GetMonthlySalesCents(ctx, "m1", "2026-12"); total != 0 {
		t.Fatalf("expected new month to start at 0, got %d", total)
	}
}

func TestMemoryMerchantRepository_AssignMerchantCLABEOnlyOnce(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryMerchantRepository()
	_ = repo.SaveMerchant(ctx, MerchantRecord{ID: "m1", RFC: "AAA010101AAA", Name: "A"})
	_ = repo.SaveMerchant(ctx, MerchantRecord{ID: "m2", RFC: "BBB010101BBB", Name: "B"})
	if got, err := repo.AssignMerchantCLABE(ctx, "m1", "646180123400000007"); err != nil || got != "646180123400000007" {
		t.Fatalf("first assign: %q %v", got, err)
	}
	if got, err := repo.AssignMerchantCLABE(ctx, "m1", "646180123400000010"); err != nil || got != "646180123400000007" {
		t.Fatalf("second assign must keep the first CLABE: %q %v", got, err)
	}
	if _, err := repo.AssignMerchantCLABE(ctx, "m2", "646180123400000007"); !errors.Is(err, ErrDuplicateCLABE) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := repo.AssignMerchantCLABE(ctx, "ghost", "646180123400000023"); !errors.Is(err, ErrMerchantNotFound) {
		t.Fatalf("ghost: %v", err)
	}
}
