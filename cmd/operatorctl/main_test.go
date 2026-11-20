package main

import (
	"context"
	"errors"
	"testing"

	"softpos/pkg/auth"
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
