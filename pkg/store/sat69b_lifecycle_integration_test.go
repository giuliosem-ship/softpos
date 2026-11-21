package store

import (
	"context"
	"errors"
	"testing"

	"softpos/pkg/identity"
	"softpos/pkg/kyb"
)

// TestPGIntegration_SAT69BLifecycle (MVP-27): on PostgreSQL the 69-B block
// writes its outbox event in the same transaction, the SAT_69B block is
// lifted when the RFC is DESVIRTUADO, an exception is respected for the same
// situation and revoked on a situation change, and the change guard records
// REJECTED_CHANGES while keeping the list.
func TestPGIntegration_SAT69BLifecycle(t *testing.T) {
	db := openMigratedPG(t)
	ctx := context.Background()
	merchants := NewSQLMerchantRepository(db)
	repo := NewSQLKYBRepository(db)
	audit := NewSQLAuditRepository(db)
	bl := NewSQLBlacklistRepository(db)
	for id, rfc := range map[string]string{"m_a": "ZURC660122LUA", "m_b": "ZZT160801AR0"} {
		if err := merchants.SaveMerchant(ctx, MerchantRecord{ID: id, RFC: rfc, Name: id, IsRESICO: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.CreateKYB(ctx, KYBRecord{MerchantID: id, Status: kyb.StatusApproved, Application: kyb.Application{RFC: rfc}}); err != nil {
			t.Fatal(err)
		}
	}
	svc := identity.NewBlacklistService()
	svc.SetRepository(bl)
	syncer := &identity.List69BSyncer{Store: bl, MaxChangePct: 100}
	load := func(csv string) Retroscreen69BResult {
		t.Helper()
		if _, err := syncer.Load(ctx, []byte("No,RFC,Nombre,Situación del contribuyente\n"+csv), "it", "it", true); err != nil {
			t.Fatal(err)
		}
		res, err := Retroscreen69B(ctx, repo, audit, svc.Lookup, "system:69b")
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	countEvents := func(typ string) int {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox_events WHERE event_type = $1`, typ).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	res := load("1,ZURC660122LUA,A,Presunto\n2,ZZT160801AR0,B,Presunto\n")
	if len(res.Blocked) != 2 || countEvents(EventMerchantBlocked69B) != 2 {
		t.Fatalf("block + events: %+v events=%d", res, countEvents(EventMerchantBlocked69B))
	}
	rec, _, _ := repo.GetKYB(ctx, "m_a")
	if rec.BlockedSource != BlockSourceSAT69B {
		t.Fatalf("source: %+v", rec)
	}
	// Operator unblocks m_b with a documented exception for PRESUNTO.
	b, _, _ := repo.GetKYB(ctx, "m_b")
	if err := repo.AddSat69BException(ctx, Sat69BException{MerchantID: "m_b", RFC: "ZZT160801AR0", Situation: "PRESUNTO", Reason: "amparo", OperatorID: "op"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.AddSat69BException(ctx, Sat69BException{MerchantID: "m_b", RFC: "ZZT160801AR0", Situation: "PRESUNTO", Reason: "dup", OperatorID: "op"}); err != nil {
		t.Fatalf("a duplicate active exception is kept, not an error: %v", err)
	}
	b.Blocked, b.BlockedReason, b.BlockedAt, b.BlockedSource = false, "", nil, ""
	if _, err := repo.UpdateKYB(ctx, b); err != nil {
		t.Fatal(err)
	}

	res = load("1,ZURC660122LUA,A,Desvirtuado\n2,ZZT160801AR0,B,Presunto\n")
	if len(res.Unblocked) != 1 || res.Unblocked[0] != "m_a" || len(res.Excepted) != 1 || countEvents(EventMerchantUnblocked69B) != 1 {
		t.Fatalf("auto-unblock / exception: %+v", res)
	}
	if rec, _, _ := repo.GetKYB(ctx, "m_a"); rec.Blocked || rec.BlockedSource != "" {
		t.Fatalf("m_a unblocked: %+v", rec)
	}
	res = load("1,ZURC660122LUA,A,Desvirtuado\n2,ZZT160801AR0,B,Definitivo\n")
	if len(res.Blocked) != 1 || res.Blocked[0] != "m_b" {
		t.Fatalf("situation change re-blocks: %+v", res)
	}
	if ok, err := repo.ActiveSat69BException(ctx, "m_b", "ZZT160801AR0", "PRESUNTO"); err != nil || ok {
		t.Fatalf("exception revoked: %v %v", ok, err)
	}

	// A failed update writes no event (same transaction).
	stale, _, _ := repo.GetKYB(ctx, "m_a")
	stale.Version--
	if _, err := repo.UpdateKYBWithEvent(ctx, stale, KYBEvent{Type: "test.event", AggregateID: "m_a", Payload: map[string]any{}}); !errors.Is(err, ErrKYBConflict) {
		t.Fatalf("conflict: %v", err)
	}
	if countEvents("test.event") != 0 {
		t.Fatal("an event must not survive a failed update")
	}

	// Change guard on PostgreSQL.
	guarded := &identity.List69BSyncer{Store: bl}
	_, err := guarded.Load(ctx, []byte("No,RFC,Nombre,Situación del contribuyente\n1,ZURC660122LUA,A,Presunto\n2,ZZT160801AR0,B,Presunto\n"), "it", "it", false)
	if !errors.Is(err, identity.ErrList69BChanged) {
		t.Fatalf("change guard: %v", err)
	}
	loads, _ := bl.ListLoads(ctx, 1)
	if loads[0].Status != identity.Load69BRejectedChanges {
		t.Fatalf("history: %+v", loads[0])
	}
	if st, _ := svc.Lookup(ctx, "ZZT160801AR0"); st != identity.BlacklistDefinitivo {
		t.Fatalf("list kept: %q", st)
	}
	// Same size, every RFC replaced: refused too (removals would auto-unblock).
	if _, err := guarded.Load(ctx, []byte("No,RFC,Nombre,Situación del contribuyente\n1,AAA080808HL8,C,Presunto\n2,AAA120730823,D,Presunto\n"), "it", "it", false); !errors.Is(err, identity.ErrList69BChanged) {
		t.Fatalf("replaced RFCs must trip the change guard: %v", err)
	}
	if st, _ := svc.Lookup(ctx, "ZZT160801AR0"); st != identity.BlacklistDefinitivo {
		t.Fatalf("list kept after replaced-RFC file: %q", st)
	}
	var staged int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sat_blacklist_staging`).Scan(&staged)
	if staged != 0 {
		t.Fatalf("staging left with %d rows", staged)
	}
}
