package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"softpos/pkg/identity"
	"softpos/pkg/store"
)

// list69B builds a minimal SAT 69-B CSV from "RFC=Situación" pairs.
func list69B(pairs ...string) []byte {
	var b strings.Builder
	b.WriteString("No,RFC,Nombre del Contribuyente,Situación del contribuyente\n")
	for i, p := range pairs {
		rfc, sit, _ := strings.Cut(p, "=")
		b.WriteString(strings.Join([]string{string(rune('1' + i)), rfc, "N", sit}, ","))
		b.WriteString("\n")
	}
	return []byte(b.String())
}

// TestSAT69B_BlockLifecycle (MVP-27): a 69-B block emits an outbox event and
// is lifted automatically when the RFC is DESVIRTUADO; an operator block is
// never lifted by a load; an operator unblock of a listed merchant needs a
// reason and records an exception the next load respects, until the
// situation changes (PRESUNTO -> DEFINITIVO re-blocks).
func TestSAT69B_BlockLifecycle(t *testing.T) {
	s, _ := setupTestServer(t)
	st := identity.NewMemoryList69BStore()
	s.blacklist.SetRepository(st)
	s.SetSAT69BMaxChangePct(100) // tiny lists: every situation change is > 10 %
	ctx := context.Background()
	kybMem := store.NewMemoryKYBRepository()
	s.SetKYBRepository(kybMem, kybMem)
	for id, rfc := range map[string]string{"m_auto": "ZURC660122LUA", "m_manual": "AAA120730823", "m_exc": "ZZT160801AR0"} {
		if err := s.RegisterMerchant(id, rfc, "Comercio "+id, true); err != nil {
			t.Fatal(err)
		}
	}
	tok, _ := operatorToken(t, s, "+525500000270")
	load := func(raw []byte) store.Retroscreen69BResult {
		t.Helper()
		_, retro, err := s.LoadSAT69B(ctx, raw, "file", "test", true)
		if err != nil {
			t.Fatal(err)
		}
		return retro
	}
	get := func(id string) store.KYBRecord {
		rec, err := s.loadKYB(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}

	// 1. Every RFC listed: all three blocked by the 69-B process, one event each.
	retro := load(list69B("ZURC660122LUA=Presunto", "AAA120730823=Definitivo", "ZZT160801AR0=Presunto", "GODE561231GR8=Desvirtuado"))
	if len(retro.Blocked) != 3 {
		t.Fatalf("blocked: %+v", retro)
	}
	if rec := get("m_auto"); !rec.Blocked || rec.BlockedSource != store.BlockSourceSAT69B {
		t.Fatalf("69-B block source: %+v", rec)
	}
	if ev := kybMem.Events(); len(ev) != 3 || ev[0].Type != store.EventMerchantBlocked69B {
		t.Fatalf("block events: %+v", ev)
	}

	// 2. m_manual: an operator converts it into a manual block (unblock with
	// reason -> exception, then an operator block).
	if rec := boCall(s, tok, http.MethodPost, "/api/v1/backoffice/merchants/m_manual/unblock", map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("unblocking a listed merchant needs a reason: %d %s", rec.Code, rec.Body.String())
	}
	rec := boCall(s, tok, http.MethodPost, "/api/v1/backoffice/merchants/m_manual/unblock", map[string]any{"reason": "amparo vigente, expediente 123"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"sat69b_exception":true`) {
		t.Fatalf("unblock with exception: %d %s", rec.Code, rec.Body.String())
	}
	if rec := boCall(s, tok, http.MethodPost, "/api/v1/backoffice/merchants/m_manual/block", map[string]any{"reason": "fraude reportado"}); rec.Code != http.StatusOK {
		t.Fatalf("operator block: %d", rec.Code)
	}
	if got := get("m_manual"); got.BlockedSource != store.BlockSourceOperator {
		t.Fatalf("operator block source: %+v", got)
	}

	// 3. m_exc: unblocked with a documented exception for PRESUNTO.
	if rec := boCall(s, tok, http.MethodPost, "/api/v1/backoffice/merchants/m_exc/unblock", map[string]any{"reason": "desvirtuado ante el SAT, pendiente de publicación"}); rec.Code != http.StatusOK {
		t.Fatalf("unblock m_exc: %d %s", rec.Code, rec.Body.String())
	}
	if got := auditActions(t, s, "m_exc"); got[len(got)-1] != "sat69b.exception" {
		t.Fatalf("exception audit: %v", got)
	}

	// 4. Next load: ZURC is DESVIRTUADO (auto-unblock), AAA leaves the list
	// (operator block stays), ZZT still PRESUNTO (exception respected).
	retro = load(list69B("ZURC660122LUA=Desvirtuado", "ZZT160801AR0=Presunto", "GODE561231GR8=Desvirtuado"))
	if len(retro.Unblocked) != 1 || retro.Unblocked[0] != "m_auto" || len(retro.Blocked) != 0 || len(retro.Excepted) != 1 {
		t.Fatalf("second load: %+v", retro)
	}
	if got := get("m_auto"); got.Blocked || got.BlockedSource != "" || got.List69B != "CLEAR" {
		t.Fatalf("auto-unblock: %+v", got)
	}
	if got := get("m_manual"); !got.Blocked || got.BlockedSource != store.BlockSourceOperator {
		t.Fatalf("an operator block is never lifted by a load: %+v", got)
	}
	if got := get("m_exc"); got.Blocked {
		t.Fatalf("exception not respected: %+v", got)
	}
	ev := kybMem.Events()
	if ev[len(ev)-1].Type != store.EventMerchantUnblocked69B || ev[len(ev)-1].AggregateID != "m_auto" {
		t.Fatalf("unblock event: %+v", ev[len(ev)-1])
	}
	if got := auditActions(t, s, "m_auto"); got[len(got)-1] != "kyb.retroscreen_69b_unblock" {
		t.Fatalf("unblock audit: %v", got)
	}

	// 5. Situation change PRESUNTO -> DEFINITIVO: the exception no longer applies.
	retro = load(list69B("ZURC660122LUA=Desvirtuado", "ZZT160801AR0=Definitivo", "GODE561231GR8=Desvirtuado"))
	if len(retro.Blocked) != 1 || retro.Blocked[0] != "m_exc" {
		t.Fatalf("situation change must re-block: %+v", retro)
	}
	// ... and the old PRESUNTO exception was revoked.
	if ok, _ := kybMem.ActiveSat69BException(ctx, "m_exc", "ZZT160801AR0", "PRESUNTO"); ok {
		t.Fatal("exception must be revoked on re-block")
	}

	// 6. Fail closed: with a stale list a listed merchant cannot be unblocked.
	s.blacklist.SetMaxAge(1)
	rec = boCall(s, tok, http.MethodPost, "/api/v1/backoffice/merchants/m_exc/unblock", map[string]any{"reason": "x"})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "SAT69B_STATUS_UNKNOWN") {
		t.Fatalf("stale list unblock: %d %s", rec.Code, rec.Body.String())
	}
}

// TestSAT69B_ChangeGuard (MVP-27): a file with the same RFCs but massively
// changed situations is refused (409 LIST_CHANGED) and the list stays; the
// override needs a reason.
func TestSAT69B_ChangeGuard(t *testing.T) {
	s, _ := setupTestServer(t)
	st := identity.NewMemoryList69BStore()
	s.blacklist.SetRepository(st)
	file := list69B("ZURC660122LUA=Presunto", "AAA120730823=Definitivo", "ZZT160801AR0=Presunto")
	s.SetSAT69BSource("https://sat.example/69b.csv", func(context.Context) ([]byte, error) { return file, nil }, 0)
	tok, _ := operatorToken(t, s, "+525500000271")
	if rec := boCall(s, tok, http.MethodPost, "/api/v1/backoffice/sat69b/sync", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("first load: %d %s", rec.Code, rec.Body.String())
	}
	file = list69B("ZURC660122LUA=Desvirtuado", "AAA120730823=Desvirtuado", "ZZT160801AR0=Desvirtuado")
	rec := boCall(s, tok, http.MethodPost, "/api/v1/backoffice/sat69b/sync", map[string]any{})
	var out struct {
		Error string       `json:"error"`
		Load  BOSAT69BLoad `json:"load"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusConflict || out.Error != "LIST_CHANGED" || out.Load.Status != identity.Load69BRejectedChanges {
		t.Fatalf("change guard: %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := s.blacklist.Lookup(context.Background(), "ZURC660122LUA"); got != identity.BlacklistPresunto {
		t.Fatalf("refused load must keep the list: %q", got)
	}
	if rec := boCall(s, tok, http.MethodPost, "/api/v1/backoffice/sat69b/sync", map[string]any{"override_changes": true}); rec.Code != http.StatusBadRequest {
		t.Fatalf("override without reason: %d", rec.Code)
	}
	rec = boCall(s, tok, http.MethodPost, "/api/v1/backoffice/sat69b/sync", map[string]any{"override_changes": true, "reason": "SAT resolvió en bloque (verificado)"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"override_changes":true`) {
		t.Fatalf("override: %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := s.blacklist.Lookup(context.Background(), "ZURC660122LUA"); got != identity.BlacklistDesvirtuado {
		t.Fatalf("override must swap: %q", got)
	}
}
