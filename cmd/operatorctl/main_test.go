package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"softpos/pkg/auth"
	"softpos/pkg/identity"
	"softpos/pkg/kyb"
	"softpos/pkg/store"
)

func TestRun_AddCheckRemove(t *testing.T) {
	ctx := context.Background()
	repo := auth.NewMemoryRepository()
	if err := run(ctx, repo, "check", "+525512340001", "t"); err == nil {
		t.Fatal("check of an unknown user must fail")
	}
	if err := run(ctx, repo, "add", "+525512340001", "t"); err != nil {
		t.Fatal(err)
	}
	u, found, _ := repo.GetUserByPhone(ctx, "+525512340001")
	if ok, _ := repo.IsPlatformOperator(ctx, u.ID); !found || !ok {
		t.Fatal("add must create the user and grant the operator")
	}
	if err := run(ctx, repo, "add", "+525512340001", "t"); err != nil {
		t.Fatalf("add is idempotent: %v", err)
	}
	if err := run(ctx, repo, "remove", "+525512340001", "t"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := repo.IsPlatformOperator(ctx, u.ID); ok {
		t.Fatal("remove must revoke the operator")
	}
	if err := run(ctx, repo, "drop", "+525512340001", "t"); !errors.Is(err, errUsage) {
		t.Fatalf("unknown command: %v", err)
	}
}

func TestRunLoad69B_LoadsAndRetroscreens(t *testing.T) {
	ctx := context.Background()
	raw, err := os.ReadFile(filepath.Join("..", "..", "pkg", "identity", "testdata", "sat69b_latin1.csv"))
	if err != nil {
		t.Fatal(err)
	}
	repo := identity.NewMemoryList69BStore()
	kybRepo := store.NewMemoryKYBRepository()
	audit := store.NewMemoryAuditRepository()
	if _, err := kybRepo.CreateKYB(ctx, store.KYBRecord{MerchantID: "m1", Status: kyb.StatusApproved,
		Application: kyb.Application{RFC: "ZURC660122LUA"}}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runLoad69B(ctx, repo, kybRepo, audit, raw, "file:x.csv", "operatorctl:test", false, &out); err != nil {
		t.Fatalf("load: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "LOADED rows=6") || !strings.Contains(out.String(), "1 blocked [m1]") {
		t.Fatalf("output: %s", out.String())
	}
	rec, _, _ := kybRepo.GetKYB(ctx, "m1")
	if !rec.Blocked || rec.List69B != kyb.List69BListed {
		t.Fatalf("retroscreen: %+v", rec)
	}
	// A much smaller file is refused without --override-shrink.
	small, _ := os.ReadFile(filepath.Join("..", "..", "pkg", "identity", "testdata", "sat69b_utf8_bom_small.csv"))
	out.Reset()
	if err := runLoad69B(ctx, repo, kybRepo, audit, small, "file:small.csv", "operatorctl:test", false, &out); !errors.Is(err, identity.ErrList69BShrank) {
		t.Fatalf("shrink: %v", err)
	}
	if err := runLoad69B(ctx, repo, kybRepo, audit, small, "file:small.csv", "operatorctl:test", true, &out); err != nil {
		t.Fatalf("override: %v", err)
	}
}
