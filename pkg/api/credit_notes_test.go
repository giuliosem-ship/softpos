package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"softpos/pkg/acquirer"
	"softpos/pkg/auth"
	"softpos/pkg/fiscal"
	"softpos/pkg/pac"
	"softpos/pkg/store"
)

func (f invoiceFixture) deliverOK(t *testing.T, e acquirer.Event) {
	t.Helper()
	if code, body := deliver(t, f.s, e); code != http.StatusOK {
		t.Fatalf("deliver %s: %d %s", e.Type, code, body)
	}
}

func refundEvent(intent CardPaymentIntentResponse, id string, cents int64) acquirer.Event {
	return acquirer.Event{ID: "evt_" + id, Type: acquirer.EventRefunded, ProviderPaymentID: intent.ProviderPaymentID, RefundID: id, AmountCents: cents}
}

func chargebackEvent(intent CardPaymentIntentResponse, typ acquirer.EventType, id string, cents int64) acquirer.Event {
	return acquirer.Event{ID: "evt_" + id + "_" + string(typ), Type: typ, ProviderPaymentID: intent.ProviderPaymentID, ChargebackID: id, AmountCents: cents}
}

func creditJobs(t *testing.T, s *Server, q store.CreditNoteJobQuery) []store.CreditNoteJob {
	t.Helper()
	repo, ok := s.creditJobRepo()
	if !ok {
		t.Fatal("no credit-note job repository")
	}
	jobs, err := repo.ListCreditNoteJobs(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	return jobs
}

func creditNotesOf(t *testing.T, s *Server, invoiceID string) []store.InvoiceRecord {
	t.Helper()
	out, err := s.invoiceRepo().ListInvoices(context.Background(), store.InvoiceQuery{MerchantID: cardMerchant, Kind: store.InvoiceCreditNote, RelatedInvoiceID: invoiceID})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestCreditNotes_RefundAfterIndividualInvoice: a refund after the sale was
// invoiced to a persona moral produces a stamped CFDI de egreso related
// with TipoRelacion 01, proportional IVA and ISR retention; a second refund
// credits exactly what is left, so the credit notes add up to the invoice.
func TestCreditNotes_RefundAfterIndividualInvoice(t *testing.T) {
	f := setupInvoiceServer(t)
	s := f.s
	intent := capturedCardPayment(t, s, f.mock, "cn-card-1", 11600)
	_, inv, body := createInvoice(t, s, auth.RoleOwner, "cn-1", map[string]any{"sale_ids": []string{intent.CardPaymentID}, "receptor": pmReceptor()})
	if inv.Status != "STAMPED" || inv.ISRRetenidoCents != 125 {
		t.Fatalf("invoice: %s", body)
	}
	f.deliverOK(t, refundEvent(intent, "re_cn_1", 5800))
	f.deliverOK(t, refundEvent(intent, "re_cn_1", 5800)) // duplicate delivery: one job
	jobs := creditJobs(t, s, store.CreditNoteJobQuery{})
	if len(jobs) != 1 || jobs[0].Status != store.CreditJobPending || jobs[0].SourceKind != store.CreditSourceRefund ||
		jobs[0].AmountCents != 5800 || jobs[0].NetAfterCents != 5800 || jobs[0].SaleID != intent.CardPaymentID {
		t.Fatalf("job enqueued with the refund: %+v", jobs)
	}

	now := time.Now()
	if n, err := s.ProcessCreditNotes(context.Background(), now); err != nil || n != 1 {
		t.Fatalf("process: %d %v", n, err)
	}
	notes := creditNotesOf(t, s, inv.InvoiceID)
	if len(notes) != 1 {
		t.Fatalf("credit notes: %d", len(notes))
	}
	cn := notes[0]
	// 58.00 of 116.00 -> base 50.00, IVA 8.00, ISR 1.25 % of 50.00 = 0.625 -> 0.63.
	if cn.Status != store.InvoiceStamped || cn.Serie != "E" || cn.UUID == "" || cn.SubtotalCents != 5000 || cn.IVACents != 800 ||
		cn.ISRRetenidoCents != 63 || cn.TotalCents != 5800-63 || cn.RelatedInvoiceID != inv.InvoiceID {
		t.Fatalf("credit note: %s %s %d+%d-%d=%d rel=%s", cn.Status, cn.Serie, cn.SubtotalCents, cn.IVACents, cn.ISRRetenidoCents, cn.TotalCents, cn.RelatedInvoiceID)
	}
	x := string(cn.XML)
	for _, want := range []string{`TipoDeComprobante="E"`, `TipoRelacion="01"`, `UUID="` + inv.UUID + `"`, `UsoCFDI="G02"`,
		`ClaveProdServ="84111506"`, `ClaveUnidad="ACT"`, `Descripcion="Devolución de venta"`, `NoIdentificacion="` + intent.CardPaymentID + `"`,
		`Rfc="EKU9003173C9"`, `TotalImpuestosRetenidos="0.63"`, `Total="57.37"`, `MetodoPago="PUE"`, `FormaPago="04"`} {
		if !strings.Contains(x, want) {
			t.Errorf("egreso XML misses %s", want)
		}
	}
	if jobs := creditJobs(t, s, store.CreditNoteJobQuery{}); jobs[0].Status != store.CreditJobIssued || jobs[0].CreditNoteInvoiceID != cn.ID || jobs[0].CreditedCents != 5800 {
		t.Fatalf("job after issue: %+v", jobs[0])
	}
	if len(f.mailer.sent) != 2 || !strings.Contains(f.mailer.sent[1].Subject, cn.UUID) {
		t.Fatalf("the receptor gets the credit note by email: %+v", f.mailer.sent)
	}
	if n, _ := s.ProcessCreditNotes(context.Background(), now); n != 0 {
		t.Fatal("a job is issued once")
	}
	// The credit note never holds the sale; the invoice keeps it.
	if holders, _ := s.invoiceRepo().ActiveInvoicesForSales(context.Background(), cardMerchant, []string{intent.CardPaymentID}); holders[intent.CardPaymentID] != inv.InvoiceID {
		t.Fatalf("holders: %v", holders)
	}
	// Its PDF / XML are served like any invoice.
	rec := serve(s, asTenant(t, s, cardMerchant, jsonReq(http.MethodGet, "/api/v1/merchants/"+cardMerchant+"/invoices/"+cn.ID+"/pdf", nil)))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Body.String(), "%PDF") {
		t.Fatalf("credit note pdf: %d", rec.Code)
	}

	// Rest of the sale: the last credit takes exactly the remaining base / IVA.
	f.deliverOK(t, refundEvent(intent, "re_cn_2", 5800))
	if n, _ := s.ProcessCreditNotes(context.Background(), time.Now()); n != 1 {
		t.Fatal("second refund credited")
	}
	notes = creditNotesOf(t, s, inv.InvoiceID)
	var base, iva int64
	for _, n := range notes {
		base, iva = base+n.SubtotalCents, iva+n.IVACents
	}
	if len(notes) != 2 || base != inv.SubtotalCents || iva != inv.IVACents {
		t.Fatalf("credit notes must add up to the invoice: base %d iva %d of %+v", base, iva, inv)
	}
}

// TestCreditNotes_NotRequiredWhenNotInvoicedAbove: a refund before the
// invoice (the invoice is already net) or of a sale never invoiced creates
// no egreso; the latter waits the grace period first.
func TestCreditNotes_NotRequiredWhenNotInvoicedAbove(t *testing.T) {
	f := setupInvoiceServer(t)
	s := f.s
	netted := capturedCardPayment(t, s, f.mock, "cn-net", 11600)
	f.deliverOK(t, refundEvent(netted, "re_net", 5800))
	if _, inv, body := createInvoice(t, s, auth.RoleOwner, "cn-net", map[string]any{"sale_ids": []string{netted.CardPaymentID}, "receptor": pmReceptor()}); inv.TotalCents != 5800-63 {
		t.Fatalf("invoice of the net amount: %s", body)
	}
	never := capturedCardPayment(t, s, f.mock, "cn-never", 2000)
	f.deliverOK(t, refundEvent(never, "re_never", 500))

	now := time.Now()
	if n, err := s.ProcessCreditNotes(context.Background(), now); err != nil || n != 0 {
		t.Fatalf("process: %d %v", n, err)
	}
	byID := map[string]store.CreditNoteJob{}
	for _, j := range creditJobs(t, s, store.CreditNoteJobQuery{}) {
		byID[j.SaleID] = j
	}
	if j := byID[netted.CardPaymentID]; j.Status != store.CreditJobNotRequired {
		t.Fatalf("refund before the invoice: %+v", j)
	}
	if j := byID[never.CardPaymentID]; j.Status != store.CreditJobPending || j.LastError != "NO_INVOICE_YET" || j.Attempts != 1 {
		t.Fatalf("not invoiced yet: waits the grace period: %+v", j)
	}
	if n, _ := s.ProcessCreditNotes(context.Background(), now.Add(creditNoteGrace+time.Minute)); n != 0 {
		t.Fatal("no egreso for a sale never invoiced")
	}
	if j := creditJobs(t, s, store.CreditNoteJobQuery{Statuses: []string{store.CreditJobNotRequired}}); len(j) != 2 {
		t.Fatalf("both not required: %+v", j)
	}
	if n := len(creditNotesOf(t, s, "")); n != 0 {
		t.Fatalf("no credit notes expected, got %d", n)
	}
}

// TestCreditNotes_ChargebackAfterGlobalAndReversal: a chargeback of a sale
// in the factura global credits it with an egreso to the general public
// (InformacionGlobal of the period); a later reversal is flagged for an
// operator, never re-invoiced; a chargeback reversed before its egreso was
// issued needs none.
func TestCreditNotes_ChargebackAfterGlobalAndReversal(t *testing.T) {
	f := setupInvoiceServer(t)
	s := f.s
	intent := capturedCardPayment(t, s, f.mock, "cn-glob", 23200)
	next := time.Now().AddDate(0, 1, 2)
	if n, err := s.RunGlobalInvoices(context.Background(), next); err != nil || n != 1 {
		t.Fatalf("global: %d %v", n, err)
	}
	globals, _ := s.invoiceRepo().ListInvoices(context.Background(), store.InvoiceQuery{MerchantID: cardMerchant, Kind: store.InvoiceGlobal})
	g := globals[0]

	f.deliverOK(t, chargebackEvent(intent, acquirer.EventChargebackOpened, "cb_1", 11600))
	if n, err := s.ProcessCreditNotes(context.Background(), next); err != nil || n != 1 {
		t.Fatalf("process: %d %v", n, err)
	}
	notes := creditNotesOf(t, s, g.ID)
	if len(notes) != 1 || notes[0].Status != store.InvoiceStamped || notes[0].TotalCents != 11600 || notes[0].ISRRetenidoCents != 0 {
		t.Fatalf("credit note of the global: %d", len(notes))
	}
	month := fiscal.ResicoYearMonth(time.Now())
	x := string(notes[0].XML)
	for _, want := range []string{`TipoDeComprobante="E"`, `Rfc="XAXX010101000"`, `UsoCFDI="S01"`, `Periodicidad="04"`, `Meses="` + month[5:] + `"`,
		`TipoRelacion="01"`, `UUID="` + g.UUID + `"`, `Descripcion="Contracargo de venta"`} {
		if !strings.Contains(x, want) {
			t.Errorf("egreso of a global misses %s", want)
		}
	}

	// Chargeback won: flagged, not re-invoiced.
	f.deliverOK(t, chargebackEvent(intent, acquirer.EventChargebackReversed, "cb_1", 11600))
	flagged := creditJobs(t, s, store.CreditNoteJobQuery{Flagged: true})
	if len(flagged) != 1 || flagged[0].Flag != store.CreditFlagChargebackReversed || flagged[0].Status != store.CreditJobIssued {
		t.Fatalf("flagged: %+v", flagged)
	}
	if n, _ := s.ProcessCreditNotes(context.Background(), next); n != 0 || len(creditNotesOf(t, s, g.ID)) != 1 {
		t.Fatal("a reversal is never re-invoiced automatically")
	}
	tok, _ := operatorToken(t, s, "+525500000078")
	rec := boCall(s, tok, http.MethodGet, "/api/v1/backoffice/credit-notes?flagged=true", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"flag":"CHARGEBACK_REVERSED"`) || !strings.Contains(rec.Body.String(), notes[0].ID) {
		t.Fatalf("backoffice flagged list: %d %s", rec.Code, rec.Body.String())
	}

	// Opened and reversed before the worker ran: no egreso.
	f.deliverOK(t, chargebackEvent(intent, acquirer.EventChargebackOpened, "cb_2", 5000))
	f.deliverOK(t, chargebackEvent(intent, acquirer.EventChargebackReversed, "cb_2", 5000))
	if n, _ := s.ProcessCreditNotes(context.Background(), next); n != 0 {
		t.Fatal("reversed before issue")
	}
	for _, j := range creditJobs(t, s, store.CreditNoteJobQuery{}) {
		if j.SourceID == "cb_2" && (j.Status != store.CreditJobNotRequired || j.Flag != store.CreditFlagChargebackReversed) {
			t.Fatalf("cb_2: %+v", j)
		}
	}
}

// TestCreditNotes_StampingFailuresRetried: PAC outage -> the credit note
// stays PENDING and the worker retries it; a SAT rejection -> FAILED,
// retried from the backoffice.
func TestCreditNotes_StampingFailuresRetried(t *testing.T) {
	f := setupInvoiceServer(t)
	s := f.s
	intent := capturedCardPayment(t, s, f.mock, "cn-pac", 11600)
	_, inv, _ := createInvoice(t, s, auth.RoleOwner, "cn-pac", map[string]any{"sale_ids": []string{intent.CardPaymentID}, "receptor": pmReceptor()})
	s.SetPACClient(pac.NewMultiPACOrchestrator(pac.NewMockPACClientWithSAT(true, "", f.sat), nil, 100*time.Millisecond))
	f.deliverOK(t, refundEvent(intent, "re_pac", 1160))
	now := time.Now()
	if n, _ := s.ProcessCreditNotes(context.Background(), now); n != 1 {
		t.Fatal("credit note created")
	}
	cn := creditNotesOf(t, s, inv.InvoiceID)[0]
	if cn.Status != store.InvoicePending || cn.ErrorCode != "PAC_UNAVAILABLE" {
		t.Fatalf("pac down: %s %s", cn.Status, cn.ErrorCode)
	}
	s.SetPACClient(pac.NewMultiPACOrchestrator(pac.NewMockPACClientWithSAT(false, "", f.sat), nil, time.Second))
	if n, _ := s.ProcessCreditNotes(context.Background(), now); n != 1 {
		t.Fatal("pending credit note retried by the worker")
	}
	if cn = creditNotesOf(t, s, inv.InvoiceID)[0]; cn.Status != store.InvoiceStamped {
		t.Fatalf("retried: %s %s", cn.Status, cn.ErrorCode)
	}

	s.SetPACClient(pac.NewMultiPACOrchestrator(pac.NewMockPACClientWithSAT(false, "CFDI40145", f.sat), nil, time.Second))
	f.deliverOK(t, refundEvent(intent, "re_sat", 1160))
	_, _ = s.ProcessCreditNotes(context.Background(), time.Now())
	var failed store.InvoiceRecord
	for _, n := range creditNotesOf(t, s, inv.InvoiceID) {
		if n.Status == store.InvoiceFailed {
			failed = n
		}
	}
	if failed.ID == "" || failed.ErrorCode != "CFDI40145" {
		t.Fatalf("sat rejection: %d credit notes, failed=%q %s", len(creditNotesOf(t, s, inv.InvoiceID)), failed.ID, failed.ErrorCode)
	}
	s.SetPACClient(pac.NewMultiPACOrchestrator(pac.NewMockPACClientWithSAT(false, "", f.sat), nil, time.Second))
	tok, _ := operatorToken(t, s, "+525500000079")
	if rec := boCall(s, tok, http.MethodPost, "/api/v1/backoffice/invoices/"+failed.ID+"/retry", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"STAMPED"`) {
		t.Fatalf("backoffice retry of a credit note: %d %s", rec.Code, rec.Body.String())
	}
	if holders, _ := s.invoiceRepo().ActiveInvoicesForSales(context.Background(), cardMerchant, []string{intent.CardPaymentID}); holders[intent.CardPaymentID] != inv.InvoiceID {
		t.Fatalf("a retried credit note must not take the sale: %v", holders)
	}
}

func TestCreditSplit_ProportionalHalfUpAndExactRemainder(t *testing.T) {
	cases := []struct{ credit, lb, li, pb, pi, wb, wi int64 }{
		{5800, 10000, 1600, 0, 0, 5000, 800},
		{1, 10000, 1600, 0, 0, 1, 0},
		{3333, 8621, 1379, 0, 0, 2873, 460},
		{5800, 10000, 1600, 5000, 800, 5000, 800}, // remainder: exact
	}
	for _, c := range cases {
		b, i := creditSplit(c.credit, c.lb, c.li, c.pb, c.pi)
		if b != c.wb || i != c.wi || b+i != c.credit || !fiscal.IVAWithinSATLimits(b, i) {
			t.Errorf("creditSplit(%d of %d+%d, prev %d+%d) = %d+%d, want %d+%d", c.credit, c.lb, c.li, c.pb, c.pi, b, i, c.wb, c.wi)
		}
	}
}
