package identity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fixture69B(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParse69BCSV_Latin1HeaderDetectionAndMalformedRows(t *testing.T) {
	res, err := Parse69BCSV(fixture69B(t, "sat69b_latin1.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 6 || res.Malformed != 3 || res.Duplicates != 2 {
		t.Fatalf("rows=%d malformed=%d duplicates=%d lines=%v", len(res.Rows), res.Malformed, res.Duplicates, res.MalformedLines)
	}
	if res.Published != "31 de octubre de 2026" {
		t.Fatalf("published = %q", res.Published)
	}
	by := map[string]List69BRow{}
	for _, r := range res.Rows {
		by[r.RFC] = r
	}
	want := map[string]BlacklistStatus{
		"AAA080808HL8": BlacklistSentenciaFavorable, "AAA091014835": BlacklistDesvirtuado,
		"AAA120730823": BlacklistDefinitivo, "ZURC660122LUA": BlacklistPresunto,
		"ZZT160801AR0": BlacklistDefinitivo, // duplicate (upper-cased RFC): most severe wins
		"ÑAÑ850101AB3": BlacklistPresunto,
	}
	for rfc, st := range want {
		if by[rfc].Status != st {
			t.Errorf("%s: status %q, want %q", rfc, by[rfc].Status, st)
		}
	}
	if by["AAA080808HL8"].Name != "ASESORES EN AVALÚOS Y ACTIVOS, S.A. DE C.V." || by["ZURC660122LUA"].Name != "ZUÑIGA RAMÍREZ CAROLINA" {
		t.Fatalf("Latin-1 names not decoded: %q / %q", by["AAA080808HL8"].Name, by["ZURC660122LUA"].Name)
	}
	if by["AAA080808HL8"].Oficio != "500-05-2018-16632" {
		t.Fatalf("oficio = %q", by["AAA080808HL8"].Oficio)
	}
	if len(res.SHA256) != 64 {
		t.Fatal("sha256 missing")
	}
}

func TestParse69BCSV_UTF8BOMAndCRLF(t *testing.T) {
	res, err := Parse69BCSV(fixture69B(t, "sat69b_utf8_bom_small.csv"))
	if err != nil || len(res.Rows) != 2 || res.Malformed != 0 {
		t.Fatalf("rows=%d malformed=%d err=%v", len(res.Rows), res.Malformed, err)
	}
	if res.Rows[1].Name != "ZUÑIGA RAMÍREZ CAROLINA" {
		t.Fatalf("utf-8 name = %q", res.Rows[1].Name)
	}
}

func TestParse69BCSV_RejectsFilesWithoutHeaderOrRows(t *testing.T) {
	if _, err := Parse69BCSV(fixture69B(t, "sat69b_no_header.csv")); !errors.Is(err, ErrList69BNoHeader) {
		t.Fatalf("no header: %v", err)
	}
	if _, err := Parse69BCSV([]byte("No,RFC,Nombre,Situación del contribuyente\n1,BAD,X,Presunto\n")); !errors.Is(err, ErrList69BEmpty) {
		t.Fatalf("no rows: %v", err)
	}
}

func TestReplaceGuard(t *testing.T) {
	g := ReplaceGuard{MaxShrinkPct: 10}
	cases := []struct {
		old, new int
		ok       bool
	}{{0, 5, true}, {100, 90, true}, {100, 89, false}, {100, 150, true}}
	for _, c := range cases {
		if g.Allows(c.old, c.new) != c.ok {
			t.Errorf("Allows(%d,%d) != %v", c.old, c.new, c.ok)
		}
	}
	if !(ReplaceGuard{MaxShrinkPct: 10, Override: true}).Allows(100, 1) {
		t.Fatal("override must allow")
	}
}

// TestList69BSyncer_LoadSwapShrinkAndStaleness covers the whole ingestion
// contract over the memory store: load, unchanged re-check, refused shrink,
// operator override and Lookup mapping / staleness.
func TestList69BSyncer_LoadSwapShrinkAndStaleness(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryList69BStore()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	sync := &List69BSyncer{Store: st, MaxShrinkPct: 50, Now: clock}
	svc := NewBlacklistService()
	svc.SetRepository(st)
	svc.SetClock(clock)
	svc.SetMaxAge(72 * time.Hour)

	if _, err := svc.Lookup(ctx, "AAA120730823"); !errors.Is(err, ErrBlacklistNotLoaded) {
		t.Fatalf("before any load: %v", err)
	}
	full := fixture69B(t, "sat69b_latin1.csv")
	res, err := sync.Load(ctx, full, "file:latin1", "test", false)
	if err != nil || res.Load.Status != Load69BLoaded || res.Load.RowCount != 6 || res.Malformed != 3 {
		t.Fatalf("load: %+v %v", res, err)
	}
	for rfc, want := range map[string]BlacklistStatus{
		"AAA120730823": BlacklistDefinitivo, "ZURC660122LUA": BlacklistPresunto,
		"AAA091014835": BlacklistDesvirtuado, "AAA080808HL8": BlacklistSentenciaFavorable,
		"GODE561231GR8": BlacklistClean, // not listed, list loaded
	} {
		if got, err := svc.Lookup(ctx, rfc); err != nil || got != want {
			t.Errorf("Lookup(%s) = %q %v, want %q", rfc, got, err, want)
		}
	}

	// Same file again: only the verification time moves.
	now = now.Add(48 * time.Hour)
	res, err = sync.Load(ctx, full, "file:latin1", "test", false)
	if err != nil || res.Load.Status != Load69BUnchanged {
		t.Fatalf("unchanged: %+v %v", res, err)
	}
	meta, _, _ := st.List69BMeta(ctx)
	if !meta.VerifiedAt.Equal(now) || meta.RowCount != 6 {
		t.Fatalf("meta after verification: %+v", meta)
	}

	// 6 -> 2 rows is a 66% shrink: refused, the loaded list stays.
	small := fixture69B(t, "sat69b_utf8_bom_small.csv")
	if _, err := sync.Load(ctx, small, "file:small", "test", false); !errors.Is(err, ErrList69BShrank) {
		t.Fatalf("shrink must be refused: %v", err)
	}
	if got, _ := svc.Lookup(ctx, "AAA120730823"); got != BlacklistDefinitivo {
		t.Fatal("refused load must not touch the list")
	}
	loads, _ := st.ListLoads(ctx, 10)
	if len(loads) != 3 || loads[0].Status != Load69BRejectedShrink || loads[0].PreviousCount != 6 {
		t.Fatalf("history: %+v", loads)
	}
	// Operator override swaps the whole list (no leftovers of the old one).
	if res, err := sync.Load(ctx, small, "file:small", "test", true); err != nil || res.Load.Status != Load69BLoaded || res.Load.RowCount != 2 {
		t.Fatalf("override: %+v %v", res, err)
	}
	if got, _ := svc.Lookup(ctx, "AAA120730823"); got != BlacklistClean {
		t.Fatalf("swap must drop rows not in the new list, got %q", got)
	}

	// No verification for > max age: UNKNOWN (stale), never CLEAN.
	now = now.Add(73 * time.Hour)
	if _, err := svc.Lookup(ctx, "GODE561231GR8"); !errors.Is(err, ErrBlacklistStale) {
		t.Fatalf("stale list: %v", err)
	}

	// A broken file is recorded as FAILED and changes nothing.
	if _, err := sync.Load(ctx, fixture69B(t, "sat69b_no_header.csv"), "file:bad", "test", false); !errors.Is(err, ErrList69BNoHeader) {
		t.Fatalf("bad file: %v", err)
	}
	loads, _ = st.ListLoads(ctx, 1)
	if loads[0].Status != Load69BFailed {
		t.Fatalf("failure not recorded: %+v", loads[0])
	}
}

// TestReplaceGuard_AllowsChanges (MVP-27): the change protection compares the
// situations of the RFCs present in both lists.
func TestReplaceGuard_AllowsChanges(t *testing.T) {
	g := ReplaceGuard{MaxChangePct: 10}
	cases := []struct {
		common, changed int
		want            bool
	}{{0, 0, true}, {100, 10, true}, {100, 11, false}, {1000, 500, false}, {3, 0, true}}
	for _, c := range cases {
		if got := g.AllowsChanges(c.common, c.changed); got != c.want {
			t.Errorf("AllowsChanges(%d,%d)=%v want %v", c.common, c.changed, got, c.want)
		}
	}
	if !(ReplaceGuard{MaxChangePct: 10, OverrideChanges: true}).AllowsChanges(100, 100) || !(ReplaceGuard{MaxChangePct: 100}).AllowsChanges(100, 100) {
		t.Fatal("override / 100% must allow")
	}
	old := map[string]BlacklistStatus{"A": BlacklistPresunto, "B": BlacklistDefinitivo, "C": BlacklistPresunto}
	common, changed := ChangedSituations(old, []List69BRow{{RFC: "A", Status: BlacklistPresunto}, {RFC: "B", Status: BlacklistDesvirtuado}, {RFC: "D", Status: BlacklistPresunto}})
	if common != 2 || changed != 1 {
		t.Fatalf("ChangedSituations: %d %d", common, changed)
	}
}

// TestList69BSyncer_ChangeGuard: same RFCs with changed situations are
// refused (REJECTED_CHANGES recorded, list kept) unless overridden.
func TestList69BSyncer_ChangeGuard(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryList69BStore()
	sync := &List69BSyncer{Store: st}
	csv := func(sit string) []byte {
		return []byte("No,RFC,Nombre,Situación del contribuyente\n1,ZURC660122LUA,A," + sit + "\n2,AAA120730823,B," + sit + "\n")
	}
	if _, err := sync.Load(ctx, csv("Presunto"), "f1", "t", false); err != nil {
		t.Fatal(err)
	}
	res, err := sync.Load(ctx, csv("Desvirtuado"), "f2", "t", false)
	if !errors.Is(err, ErrList69BChanged) || res.Load.Status != Load69BRejectedChanges {
		t.Fatalf("want change rejection, got %+v %v", res.Load, err)
	}
	if e, _, _ := st.GetEntry(ctx, "ZURC660122LUA"); e.Status != BlacklistPresunto {
		t.Fatalf("list must be kept: %+v", e)
	}
	if res, err := sync.LoadWithOptions(ctx, csv("Desvirtuado"), "f2", "t", Load69BOptions{OverrideChanges: true}); err != nil || !res.Load.OverrideChanges {
		t.Fatalf("override: %+v %v", res.Load, err)
	}
	if e, _, _ := st.GetEntry(ctx, "ZURC660122LUA"); e.Status != BlacklistDesvirtuado {
		t.Fatalf("override must swap: %+v", e)
	}
}
