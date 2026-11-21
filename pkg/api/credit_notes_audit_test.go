package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"softpos/pkg/auth"
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
