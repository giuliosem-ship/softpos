package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"softpos/pkg/auth"
	"softpos/pkg/store"
)

// logCapture records slog output (tests in this package do not run in parallel).
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *logCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	c := &logCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(c, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return c
}

func globalsOf(t *testing.T, s *Server) []store.InvoiceRecord {
	t.Helper()
	out, err := s.invoiceRepo().ListInvoices(context.Background(), store.InvoiceQuery{MerchantID: cardMerchant, Kind: store.InvoiceGlobal})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestGlobalInvoices_SplitIntoParts: a period with more sales than fit in
// one CFDI is split into several globals of the same period (no silent
// truncation); a rerun issues nothing new.
func TestGlobalInvoices_SplitIntoParts(t *testing.T) {
	f := setupInvoiceServer(t)
	s := f.s
	s.globalMaxConcepts = 2
	var sales []string
	for _, c := range []string{"SPLIT0001", "SPLIT0002", "SPLIT0003", "SPLIT0004", "SPLIT0005"} {
		sales = append(sales, f.speiSale(t, c, 11600))
	}
	next := time.Now().AddDate(0, 1, 2)
	if n, err := s.RunGlobalInvoices(context.Background(), next); err != nil || n != 3 {
		t.Fatalf("split run: %d %v", n, err)
	}
	gs := globalsOf(t, s)
	covered := map[string]bool{}
	parts := map[int]bool{}
	for _, g := range gs {
		if g.Status != store.InvoiceStamped || len(g.SaleIDs) > 2 || g.GlobalPeriod != gs[0].GlobalPeriod {
			t.Fatalf("part %d: %s %d sales %s", g.GlobalPart, g.Status, len(g.SaleIDs), g.GlobalPeriod)
		}
		parts[g.GlobalPart] = true
		for _, id := range g.SaleIDs {
			covered[id] = true
		}
	}
	if len(gs) != 3 || len(parts) != 3 || !parts[1] || !parts[2] || !parts[3] || len(covered) != len(sales) {
		t.Fatalf("parts %v cover %d of %d sales", parts, len(covered), len(sales))
	}
	if n, _ := s.RunGlobalInvoices(context.Background(), next); n != 0 {
		t.Fatal("rerun must not duplicate")
	}
}

// TestGlobalInvoices_ReconcileLateAndFreedSales: every un-invoiced eligible
// sale within the max age gets into a new part of its period: a capture
// confirmed after the period's global was issued, and sales freed by a
// motivo 04 cancellation months later. Older sales are alerted.
func TestGlobalInvoices_ReconcileLateAndFreedSales(t *testing.T) {
	f := setupInvoiceServer(t)
	s := f.s
	first := f.speiSale(t, "LATE00001", 11600)
	// Intent created this month, captured only after the global was issued.
	code, late, body := createIntent(t, s, cardMerchant, "late-capture", map[string]any{"amount_cents": 5800})
	if code != http.StatusCreated {
		t.Fatalf("intent: %d %s", code, body)
	}
	next := time.Now().AddDate(0, 1, 2)
	if n, err := s.RunGlobalInvoices(context.Background(), next); err != nil || n != 1 {
		t.Fatalf("first run: %d %v", n, err)
	}
	for _, e := range f.mock.SimulateTap(late.ProviderPaymentID, late.CardPaymentID, 5800) {
		f.deliverOK(t, e)
	}
	if n, _ := s.RunGlobalInvoices(context.Background(), next); n != 1 {
		t.Fatal("late capture must get a new part of its period")
	}
	gs := globalsOf(t, s)
	if len(gs) != 2 || gs[0].GlobalPeriod != gs[1].GlobalPeriod || gs[0].GlobalPart+gs[1].GlobalPart != 3 {
		t.Fatalf("two parts of one period: %d", len(gs))
	}
	var part1 store.InvoiceRecord
	for _, g := range gs {
		if g.GlobalPart == 1 {
			part1 = g
		}
	}
	if len(part1.SaleIDs) != 1 || part1.SaleIDs[0] != first {
		t.Fatalf("part 1: %v", part1.SaleIDs)
	}

	// Months later (beyond the old "last two closed periods" lookback) the
	// owner cancels part 1 with motivo 04: its sale is re-issued.
	if code, out, body := cancelInvoice(t, s, auth.RoleOwner, part1.ID, map[string]string{"motivo": "04"}); code != http.StatusOK || out.Status != "CANCELED" {
		t.Fatalf("cancel part 1: %d %s", code, body)
	}
	later := time.Now().AddDate(0, 3, 0)
	s.SetGlobalInvoiceMaxAge(120 * 24 * time.Hour)
	if n, _ := s.RunGlobalInvoices(context.Background(), later); n != 1 {
		t.Fatal("freed sale of an old period must be re-issued")
	}
	holders, _ := s.invoiceRepo().ActiveInvoicesForSales(context.Background(), cardMerchant, []string{first})
	if h := holders[first]; h == "" || h == part1.ID {
		t.Fatalf("re-issued holder: %q", h)
	}

	// Older than the max age: not issued, alerted.
	old := f.speiSale(t, "LATE00002", 2000)
	s.SetGlobalInvoiceMaxAge(30 * 24 * time.Hour)
	logs := captureLogs(t)
	if n, _ := s.RunGlobalInvoices(context.Background(), later); n != 0 {
		t.Fatal("a sale older than the window is not auto-issued")
	}
	out := logs.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "ALERT") || !strings.Contains(out, old) {
		t.Fatalf("missing ALERT for the stale sale: %s", out)
	}
}

// TestGlobalInvoices_FechaUsesLugarExpedicionZone: a merchant in Tijuana
// emits its CFDI Fecha in Pacific time.
func TestGlobalInvoices_FechaUsesLugarExpedicionZone(t *testing.T) {
	f := setupInvoiceServer(t)
	s := f.s
	if err := s.SeedFiscalSettings(context.Background(), cardMerchant, "22000"); err != nil {
		t.Fatal(err)
	}
	fs, _, _ := s.fiscalSettingsRepo().GetFiscalSettings(context.Background(), cardMerchant)
	fs.LugarExpedicion = "22000"
	_ = s.fiscalSettingsRepo().SaveFiscalSettings(context.Background(), fs)
	sale := f.speiSale(t, "TIJ000001", 11600)
	_, inv, body := createInvoice(t, s, auth.RoleOwner, "tij-1", map[string]any{"sale_ids": []string{sale}, "receptor": pmReceptor()})
	if inv.Status != "STAMPED" {
		t.Fatalf("invoice: %s", body)
	}
	rec, _, _ := s.invoiceRepo().GetInvoice(context.Background(), cardMerchant, inv.InvoiceID)
	tij, _ := time.LoadLocation("America/Tijuana")
	want := rec.SealedAt.In(tij).Format("2006-01-02T15:04:05")
	if !strings.Contains(string(rec.XML), `Fecha="`+want+`"`) || !strings.Contains(string(rec.XML), `LugarExpedicion="22000"`) {
		t.Fatalf("Fecha must be Tijuana local time %s", want)
	}
}
