package store

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"softpos/pkg/kyb"
)

func kybRepoContract(t *testing.T, repo KYBRepository, blobs DocumentBlobStore, seedMerchant func(id string)) {
	t.Helper()
	ctx := context.Background()
	seedMerchant("m_kyb_1")
	seedMerchant("m_kyb_2")
	if _, found, err := repo.GetKYB(ctx, "m_kyb_1"); err != nil || found {
		t.Fatalf("empty get: %v %v", found, err)
	}
	created, err := repo.CreateKYB(ctx, KYBRecord{MerchantID: "m_kyb_1"})
	if err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	if again, err := repo.CreateKYB(ctx, KYBRecord{MerchantID: "m_kyb_1", Status: kyb.StatusApproved}); err != nil || again {
		t.Fatalf("create must be insert-if-absent: %v %v", again, err)
	}
	rec, _, _ := repo.GetKYB(ctx, "m_kyb_1")
	if rec.Status != kyb.StatusDraft || rec.List69B != kyb.List69BUnknown || rec.IdentityCheck != kyb.IdentityUnknown || rec.Version != 1 {
		t.Fatalf("defaults: %+v", rec)
	}

	// Optimistic concurrency: of N concurrent updates on the same version, one wins.
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, conflicts := 0, 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rec
			r.Status = kyb.StatusSubmitted
			_, err := repo.UpdateKYB(ctx, r)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, ErrKYBConflict):
				conflicts++
			default:
				t.Errorf("update: %v", err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 || conflicts != 7 {
		t.Fatalf("want 1 winner / 7 conflicts, got %d / %d", wins, conflicts)
	}
	now := time.Now().UTC().Truncate(time.Second)
	cur, _, _ := repo.GetKYB(ctx, "m_kyb_1")
	cur.Application = kyb.Application{LegalName: "Xochilt", RFC: "CACX7605101P8", TaxRegime: kyb.RegimeRESICOPF,
		Address: kyb.Address{City: "CDMX", PostalCode: "06000"}}
	cur.Status, cur.SubmittedAt, cur.Blocked, cur.BlockedReason, cur.BlockedAt = kyb.StatusInReview, &now, true, "fraud", &now
	cur.List69B, cur.IdentityCheck, cur.AcquirerMerchantID = kyb.List69BClear, kyb.IdentityPassed, "sub_1"
	out, err := repo.UpdateKYB(ctx, cur)
	if err != nil || out.Version != cur.Version+1 || out.Application.Address.City != "CDMX" || out.SubmittedAt == nil ||
		!out.SubmittedAt.Equal(now) || !out.Blocked || out.AcquirerMerchantID != "sub_1" {
		t.Fatalf("round trip: %+v %v", out, err)
	}
	if _, err := repo.UpdateKYB(ctx, cur); !errors.Is(err, ErrKYBConflict) {
		t.Fatalf("stale version: %v", err)
	}
	_, _ = repo.CreateKYB(ctx, KYBRecord{MerchantID: "m_kyb_2"})
	inReview, _ := repo.ListKYB(ctx, KYBQuery{Status: kyb.StatusInReview})
	blocked := true
	onlyBlocked, _ := repo.ListKYB(ctx, KYBQuery{Blocked: &blocked})
	all, _ := repo.ListKYB(ctx, KYBQuery{})
	if len(inReview) != 1 || len(onlyBlocked) != 1 || len(all) != 2 || all[0].MerchantID != "m_kyb_2" {
		t.Fatalf("list filters: review=%d blocked=%d all=%d", len(inReview), len(onlyBlocked), len(all))
	}

	// Documents: a new upload of a type supersedes the previous one.
	for _, d := range []KYBDocument{
		{ID: "kdoc_1", MerchantID: "m_kyb_1", Type: kyb.DocINEFront, ContentType: "image/png", SizeBytes: 10, SHA256: "a", BlobKey: "m_kyb_1/kdoc_1"},
		{ID: "kdoc_2", MerchantID: "m_kyb_1", Type: kyb.DocSelfie, ContentType: "image/png", SizeBytes: 10, SHA256: "b", BlobKey: "m_kyb_1/kdoc_2"},
		{ID: "kdoc_3", MerchantID: "m_kyb_1", Type: kyb.DocINEFront, ContentType: "application/pdf", SizeBytes: 11, SHA256: "c", BlobKey: "m_kyb_1/kdoc_3"},
	} {
		if err := repo.AddKYBDocument(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	active, _ := repo.ListKYBDocuments(ctx, "m_kyb_1", false)
	history, _ := repo.ListKYBDocuments(ctx, "m_kyb_1", true)
	if len(active) != 2 || len(history) != 3 {
		t.Fatalf("documents active=%d history=%d", len(active), len(history))
	}
	if d, found, _ := repo.GetKYBDocument(ctx, "m_kyb_1", "kdoc_1"); !found || d.Status != KYBDocSuperseded {
		t.Fatalf("superseded: %+v", d)
	}
	if _, found, _ := repo.GetKYBDocument(ctx, "m_kyb_2", "kdoc_1"); found {
		t.Fatal("a document is only visible under its merchant")
	}
	if err := blobs.PutBlob(ctx, "m_kyb_1/kdoc_1", []byte{0, 1, 2, 255}); err != nil {
		t.Fatal(err)
	}
	if b, found, err := blobs.GetBlob(ctx, "m_kyb_1/kdoc_1"); err != nil || !found || !bytes.Equal(b, []byte{0, 1, 2, 255}) {
		t.Fatalf("blob: %v %v %v", b, found, err)
	}
	if _, found, _ := blobs.GetBlob(ctx, "nope"); found {
		t.Fatal("missing blob")
	}
}

func TestKYBRepository_MemoryContract(t *testing.T) {
	m := NewMemoryKYBRepository()
	kybRepoContract(t, m, m, func(string) {})
}

// TestPGIntegration_KYBRepositoryContract runs the same contract on PostgreSQL
// (migration 012), plus the FK to merchants.
func TestPGIntegration_KYBRepositoryContract(t *testing.T) {
	db := openMigratedPG(t)
	merchants := NewSQLMerchantRepository(db)
	repo := NewSQLKYBRepository(db)
	kybRepoContract(t, repo, repo, func(id string) {
		if err := merchants.SaveMerchant(context.Background(), MerchantRecord{ID: id, RFC: "CACX7605101P8", Name: id, IsRESICO: true}); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := repo.CreateKYB(context.Background(), KYBRecord{MerchantID: "ghost"}); !errors.Is(err, ErrMerchantNotFound) {
		t.Fatalf("unknown merchant: %v", err)
	}
}
