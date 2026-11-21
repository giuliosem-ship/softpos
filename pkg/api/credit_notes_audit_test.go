package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"softpos/pkg/auth"
	"softpos/pkg/pac"
	"softpos/pkg/store"
)

// TestCreditNotes_CancelBlockedWhileCreditNotesLive: a CFDI with a live
// nota de crédito related to it (TipoRelacion 01) cannot be canceled (SAT:
// related vigente CFDI first). Otherwise a motivo 01 replacement built from
// the net amount plus the old egreso still vigente would credit the refund
// twice. Once the credit note is canceled, the invoice can be canceled.
func TestCreditNotes_CancelBlockedWhileCreditNotesLive(t *testing.T) {
	f := setupInvoiceServer(t)
	s := f.s
	intent := capturedCardPayment(t, s, f.mock, "cn-cancel", 11600)
	_, inv, body := createInvoice(t, s, auth.RoleOwner, "cn-cancel", map[string]any{"sale_ids": []string{intent.CardPaymentID}, "receptor": pmReceptor()})
	if inv.Status != "STAMPED" {
		t.Fatalf("invoice: %s", body)
	}
	f.deliverOK(t, refundEvent(intent, "re_cancel", 5800))
	if n, err := s.ProcessCreditNotes(context.Background(), time.Now()); err != nil || n != 1 {
		t.Fatalf("process: %d %v", n, err)
	}
	cn := creditNotesOf(t, s, inv.InvoiceID)[0]
	if code, _, body := cancelInvoice(t, s, auth.RoleOwner, inv.InvoiceID, map[string]string{"motivo": "02"}); code != http.StatusConflict ||
		!strings.Contains(body, "INVOICE_HAS_CREDIT_NOTES") {
		t.Fatalf("cancel with a live credit note: %d %s", code, body)
	}
	if code, out, body := cancelInvoice(t, s, auth.RoleOwner, cn.ID, map[string]string{"motivo": "02"}); code != http.StatusOK || out.Status != store.InvoiceCanceled {
		t.Fatalf("cancel the credit note: %d %s", code, body)
	}
	if code, out, body := cancelInvoice(t, s, auth.RoleOwner, inv.InvoiceID, map[string]string{"motivo": "02"}); code != http.StatusOK || out.Status != store.InvoiceCanceled {
		t.Fatalf("cancel after its credit note: %d %s", code, body)
	}
}

// TestCreditNotes_FailedRetryRefusedOnceCreditedCanceled: a FAILED credit
// note does not block the cancellation of its invoice; retrying it later
// must not stamp an egreso related to a canceled CFDI.
func TestCreditNotes_FailedRetryRefusedOnceCreditedCanceled(t *testing.T) {
	f := setupInvoiceServer(t)
	s := f.s
	intent := capturedCardPayment(t, s, f.mock, "cn-failed", 11600)
	_, inv, _ := createInvoice(t, s, auth.RoleOwner, "cn-failed", map[string]any{"sale_ids": []string{intent.CardPaymentID}, "receptor": pmReceptor()})
	s.SetPACClient(pac.NewMultiPACOrchestrator(pac.NewMockPACClientWithSAT(false, "CFDI40145", f.sat), nil, time.Second))
	f.deliverOK(t, refundEvent(intent, "re_failed", 1160))
	_, _ = s.ProcessCreditNotes(context.Background(), time.Now())
	cn := creditNotesOf(t, s, inv.InvoiceID)[0]
	if cn.Status != store.InvoiceFailed {
		t.Fatalf("credit note: %s", cn.Status)
	}
	s.SetPACClient(pac.NewMultiPACOrchestrator(pac.NewMockPACClientWithSAT(false, "", f.sat), nil, time.Second))
	if code, _, body := cancelInvoice(t, s, auth.RoleOwner, inv.InvoiceID, map[string]string{"motivo": "02"}); code != http.StatusOK {
		t.Fatalf("cancel: %d %s", code, body)
	}
	tok, _ := operatorToken(t, s, "+525500000081")
	_ = boCall(s, tok, http.MethodPost, "/api/v1/backoffice/invoices/"+cn.ID+"/retry", nil)
	if got, _, _ := s.invoiceRepo().GetInvoice(context.Background(), cardMerchant, cn.ID); got.Status == store.InvoiceStamped {
		t.Fatal("an egreso related to a canceled CFDI was stamped")
	}
}

// TestGlobalInvoices_RefreshesLinesBeforeIssuing: the reconciliation reads
// every un-invoiced sale of the window before issuing parts one by one
// (each stamped through the PAC), so a refund can land long after a sale's
// amounts were read. The part must use the sale's amounts as of its
// creation; otherwise the refund (whose credit-note job resolved
// NOT_REQUIRED after the grace period) would never be credited.
func TestGlobalInvoices_RefreshesLinesBeforeIssuing(t *testing.T) {
	f := setupInvoiceServer(t)
	s := f.s
	intent := capturedCardPayment(t, s, f.mock, "gl-stale", 11600)
	ctx := context.Background()
	next := time.Now().AddDate(0, 1, 2)
	emisor, err := s.emisorProfile(ctx, cardMerchant)
	if err != nil {
		t.Fatal(err)
	}
	var lines []saleLine
	if err := s.eachUninvoicedSale(ctx, cardMerchant, time.Time{}, next, 0, func(l saleLine) { lines = append(lines, l) }); err != nil || len(lines) != 1 {
		t.Fatalf("lines: %v %d", err, len(lines))
	}
	f.deliverOK(t, refundEvent(intent, "re_stale", 5800)) // lands while earlier parts are being issued
	out, err := s.issueGlobalPart(ctx, cardMerchant, emisor, globalPeriodOf(emisor.Settings.GlobalPeriodicity, time.Now()), lines, next)
	if err != nil || out == nil {
		t.Fatalf("part: %v", err)
	}
	if out.TotalCents != 5800 {
		t.Fatalf("the part must invoice the net amount at creation: total %d", out.TotalCents)
	}
}

// TestCreditNotes_ISRRemainderAddsUpToTheInvoice: two half refunds of a
// persona moral invoice (ISR 1.25 % of 100.00 = 1.25) credit 0.63 + 0.62,
// not 0.63 + 0.63: the credit notes of a line add up to it to the cent,
// retention and total included (0.62 is within the SAT limits of 50.00).
func TestCreditNotes_ISRRemainderAddsUpToTheInvoice(t *testing.T) {
	f := setupInvoiceServer(t)
	s := f.s
	intent := capturedCardPayment(t, s, f.mock, "cn-isr", 11600)
	_, inv, _ := createInvoice(t, s, auth.RoleOwner, "cn-isr", map[string]any{"sale_ids": []string{intent.CardPaymentID}, "receptor": pmReceptor()})
	for _, id := range []string{"re_isr_1", "re_isr_2"} {
		f.deliverOK(t, refundEvent(intent, id, 5800))
		if n, err := s.ProcessCreditNotes(context.Background(), time.Now()); err != nil || n != 1 {
			t.Fatalf("process %s: %d %v", id, n, err)
		}
	}
	var isr, total int64
	for _, n := range creditNotesOf(t, s, inv.InvoiceID) {
		if n.Status != store.InvoiceStamped {
			t.Fatalf("credit note %s: %s %s", n.ID, n.Status, n.ErrorCode)
		}
		isr, total = isr+n.ISRRetenidoCents, total+n.TotalCents
	}
	if isr != inv.ISRRetenidoCents || total != inv.TotalCents {
		t.Fatalf("credit notes ISR %d total %d, invoice ISR %d total %d", isr, total, inv.ISRRetenidoCents, inv.TotalCents)
	}
}
