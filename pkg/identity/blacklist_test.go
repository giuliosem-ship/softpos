package identity

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestBlacklistServiceBasics(t *testing.T) {
	service := NewBlacklistService()

	// Seed known EFOS cases
	service.AddEntry("EFO110101XYZ", BlacklistDefinitivo, "500-05-2023-1234")
	service.AddEntry("PRE120202ABC", BlacklistPresunto, "500-05-2024-5678")
	service.AddEntry("DES130303QWE", BlacklistDesvirtuado, "500-05-2024-9999")
	service.AddEntry("SEN140404RTY", BlacklistSentenciaFavorable, "500-05-2024-7777")

	// 1. Clean RFC
	if st := service.GetStatus("GARM850101ABC"); st != BlacklistClean {
		t.Errorf("expected BlacklistClean, got %v", st)
	}
	ok, err := service.ValidateMerchantEligibility("GARM850101ABC")
	if err != nil || !ok {
		t.Errorf("expected clean RFC to be eligible, got ok=%v, err=%v", ok, err)
	}

	// 2. Desvirtuado & Sentencia Favorable
	if st := service.GetStatus("DES130303QWE"); st != BlacklistDesvirtuado {
		t.Errorf("expected BlacklistDesvirtuado, got %v", st)
	}
	ok, err = service.ValidateMerchantEligibility("DES130303QWE")
	if err != nil || !ok {
		t.Errorf("expected DESVIRTUADO to be eligible")
	}

	if st := service.GetStatus("SEN140404RTY"); st != BlacklistSentenciaFavorable {
		t.Errorf("expected BlacklistSentenciaFavorable, got %v", st)
	}
	ok, err = service.ValidateMerchantEligibility("SEN140404RTY")
	if err != nil || !ok {
		t.Errorf("expected SENTENCIA_FAVORABLE to be eligible")
	}

	// 3. Definitivo blocks onboarding
	if st := service.GetStatus("EFO110101XYZ"); st != BlacklistDefinitivo {
		t.Errorf("expected BlacklistDefinitivo, got %v", st)
	}
	_, err = service.ValidateMerchantEligibility("EFO110101XYZ")
	if err != ErrBlacklistedEntity {
		t.Errorf("expected ErrBlacklistedEntity for DEFINITIVO, got %v", err)
	}

	// 4. Presunto blocks onboarding
	if st := service.GetStatus("PRE120202ABC"); st != BlacklistPresunto {
		t.Errorf("expected BlacklistPresunto, got %v", st)
	}
	_, err = service.ValidateMerchantEligibility("PRE120202ABC")
	if err != ErrBlacklistedEntity {
		t.Errorf("expected ErrBlacklistedEntity for PRESUNTO, got %v", err)
	}

	// 5. Check Oficio
	if of := service.GetOficio("EFO110101XYZ"); of != "500-05-2023-1234" {
		t.Errorf("expected oficio 500-05-2023-1234, got %s", of)
	}
}

func TestBlacklistConcurrency(t *testing.T) {
	service := NewBlacklistService()
	var wg sync.WaitGroup

	// Concurrently write and read
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func(id int) {
			defer wg.Done()
			service.AddEntry("EFO110101XYZ", BlacklistDefinitivo, "oficio")
		}(i)
		go func(id int) {
			defer wg.Done()
			_ = service.GetStatus("EFO110101XYZ")
			_, _ = service.ValidateMerchantEligibility("EFO110101XYZ")
		}(i)
	}

	wg.Wait()
}

type failingBlacklistRepo struct{}

func (failingBlacklistRepo) AddEntry(context.Context, BlacklistEntry) error { return nil }
func (failingBlacklistRepo) GetEntry(context.Context, string) (BlacklistEntry, bool, error) {
	return BlacklistEntry{}, false, errors.New("db down")
}

// Audit (batch G): an RFC absent from a list that was never loaded, or a
// lookup that failed, is not "clean".
func TestBlacklistLookupIsNotCleanWithoutAList(t *testing.T) {
	ctx := context.Background()
	s := NewBlacklistService()
	if _, err := s.Lookup(ctx, "GARM850101ABC"); !errors.Is(err, ErrBlacklistNotLoaded) {
		t.Fatalf("empty list: want ErrBlacklistNotLoaded, got %v", err)
	}
	s.AddEntry("EFO110101XYZ", BlacklistDefinitivo, "500-05-2023-1234")
	if st, err := s.Lookup(ctx, "GARM850101ABC"); err != nil || st != BlacklistClean {
		t.Fatalf("loaded list, absent RFC: %v %v", st, err)
	}
	if st, err := s.Lookup(ctx, " efo110101xyz "); err != nil || st != BlacklistDefinitivo {
		t.Fatalf("listed RFC: %v %v", st, err)
	}
	f := NewBlacklistService()
	f.SetRepository(failingBlacklistRepo{})
	f.AddEntry("EFO110101XYZ", BlacklistDefinitivo, "x")
	if _, err := f.Lookup(ctx, "GARM850101ABC"); !errors.Is(err, ErrBlacklistUnavailable) {
		t.Fatalf("repository failure: want ErrBlacklistUnavailable, got %v", err)
	}
}
