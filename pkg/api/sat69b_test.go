package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"softpos/pkg/auth"
	"softpos/pkg/identity"
)

func read69BFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "identity", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestSAT69B_BackofficeSyncRetroscreenAndShrinkGuard: an operator triggers
// the download; the list is swapped in, an approved merchant whose RFC is
// now listed is blocked (audited), the status endpoint shows the load, and a
// list that shrank too much is refused unless overridden with a reason.
func TestSAT69B_BackofficeSyncRetroscreenAndShrinkGuard(t *testing.T) {
	s, _ := setupTestServer(t)
	st := identity.NewMemoryList69BStore()
	s.blacklist.SetRepository(st)
	file := read69BFixture(t, "sat69b_latin1.csv")
	fetchErr := error(nil)
	s.SetSAT69BSource("https://sat.example/69b.csv", func(context.Context) ([]byte, error) { return file, fetchErr }, 50)

	// Approved merchants: one with a listed RFC (DEFINITIVO), one clean.
	if err := s.RegisterMerchant("m_listed", "AAA120730823", "Asesores Agricolas", false); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterMerchant("m_clean", "GODE561231GR8", "Comercio Limpio", true); err != nil {
		t.Fatal(err)
	}
	tok, _ := operatorToken(t, s, "+525500000069")
	if rec := serve(s, asTenant(t, s, "m_clean", jsonReq(http.MethodPost, "/api/v1/backoffice/sat69b/sync", map[string]any{}))); rec.Code != http.StatusForbidden {
		t.Fatalf("merchant token must not trigger the sync: %d", rec.Code)
	}

	rec := boCall(s, tok, http.MethodPost, "/api/v1/backoffice/sat69b/sync", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("sync: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Load        BOSAT69BLoad `json:"load"`
		Malformed   int          `json:"malformed"`
		Retroscreen struct {
			Blocked []string `json:"blocked"`
		} `json:"retroscreen"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Load.Status != identity.Load69BLoaded || out.Load.RowCount != 6 || out.Malformed != 3 || len(out.Retroscreen.Blocked) != 1 || out.Retroscreen.Blocked[0] != "m_listed" {
		t.Fatalf("sync result: %s", rec.Body.String())
	}
	listed, _ := s.loadKYB(context.Background(), "m_listed")
	clean, _ := s.loadKYB(context.Background(), "m_clean")
	if !listed.Blocked || listed.List69B != "LISTED" || clean.Blocked || clean.List69B != "CLEAR" {
		t.Fatalf("retroscreen: listed=%+v clean=%+v", listed, clean)
	}
	if got := auditActions(t, s, "m_listed"); len(got) != 1 || got[0] != "kyb.retroscreen_69b_block" {
		t.Fatalf("retroscreen audit: %v", got)
	}
	// Money routes now refuse the blocked merchant.
	if rec := serve(s, asTenant(t, s, "m_listed", jsonReq(http.MethodPost, "/api/v1/payments/codi/qr", map[string]any{"merchant_id": "m_listed", "amount_cents": 1000}))); rec.Code != http.StatusForbidden {
		t.Fatalf("blocked merchant money route: %d %s", rec.Code, rec.Body.String())
	}

	// Status endpoint.
	rec = boCall(s, tok, http.MethodGet, "/api/v1/backoffice/sat69b", nil)
	var status BOSAT69BStatus
	_ = json.Unmarshal(rec.Body.Bytes(), &status)
	if rec.Code != http.StatusOK || !status.Loaded || status.Stale || status.RowCount != 6 || status.Published != "31 de octubre de 2026" || len(status.Loads) != 1 || !status.SourceConfigured {
		t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
	}

	// Shrunk file: 409, the list stays; override needs a reason.
	file = read69BFixture(t, "sat69b_utf8_bom_small.csv")
	if rec := boCall(s, tok, http.MethodPost, "/api/v1/backoffice/sat69b/sync", map[string]any{}); rec.Code != http.StatusConflict {
		t.Fatalf("shrink: %d %s", rec.Code, rec.Body.String())
	}
	if rec := boCall(s, tok, http.MethodPost, "/api/v1/backoffice/sat69b/sync", map[string]any{"override_shrink": true}); rec.Code != http.StatusBadRequest {
		t.Fatalf("override without reason: %d", rec.Code)
	}
	if rec := boCall(s, tok, http.MethodPost, "/api/v1/backoffice/sat69b/sync", map[string]any{"override_shrink": true, "reason": "SAT published a reduced list (verified)"}); rec.Code != http.StatusOK {
		t.Fatalf("override: %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := s.blacklist.Lookup(context.Background(), "AAA120730823"); got != identity.BlacklistClean {
		t.Fatalf("override must swap the list: %q", got)
	}

	// Download failure: 502 and a FAILED row; the list stays.
	fetchErr = errors.New("connection reset")
	if rec := boCall(s, tok, http.MethodPost, "/api/v1/backoffice/sat69b/sync", map[string]any{}); rec.Code != http.StatusBadGateway {
		t.Fatalf("fetch failure: %d", rec.Code)
	}
	loads, _ := st.ListLoads(context.Background(), 1)
	if loads[0].Status != identity.Load69BFailed {
		t.Fatalf("fetch failure not recorded: %+v", loads[0])
	}
	syncs := 0
	for _, a := range auditActions(t, s, "") {
		if a == "sat69b.sync" {
			syncs++
		}
	}
	if syncs != 4 {
		t.Fatalf("every sync attempt must be audited, got %d", syncs)
	}
}

// TestSAT69B_KYBSubmitUsesLoadedList: a submission against a loaded list
// yields LISTED (automatic rejection) or CLEAR.
func TestSAT69B_KYBSubmitUsesLoadedList(t *testing.T) {
	s, _ := setupTestServer(t)
	st := identity.NewMemoryList69BStore()
	s.blacklist.SetRepository(st)
	if _, _, err := s.LoadSAT69B(context.Background(), read69BFixture(t, "sat69b_latin1.csv"), "file", "test", false); err != nil {
		t.Fatal(err)
	}
	draftMerchant(t, s, "m_sub")
	base := "/api/v1/merchants/m_sub/kyb"
	kybCall(t, s, "m_sub", auth.RoleOwner, http.MethodPut, base, kybApplication())
	uploadAllDocs(t, s, "m_sub", nil)
	code, _, body := kybCall(t, s, "m_sub", auth.RoleOwner, http.MethodPost, base+"/submit", nil)
	if code != http.StatusOK {
		t.Fatalf("submit: %d %s", code, body)
	}
	rec, _ := s.loadKYB(context.Background(), "m_sub")
	if rec.List69B != "CLEAR" {
		t.Fatalf("list_69b with a loaded list = %s", rec.List69B)
	}
}
