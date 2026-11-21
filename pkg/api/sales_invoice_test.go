package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"softpos/pkg/auth"
)

// TestSales_ExposeInvoiceAndCreditNotes: each sale shows its active
// invoice (id, kind, status; null when not invoiced) and its credit notes,
// in the listing and in the sale detail (additive fields only).
func TestSales_ExposeInvoiceAndCreditNotes(t *testing.T) {
	f := setupInvoiceServer(t)
	s := f.s
	card := capturedCardPayment(t, s, f.mock, "sx-card", 11600)
	inGlobal := f.speiSale(t, "SALEXP001", 5800)
	if _, inv, body := createInvoice(t, s, auth.RoleOwner, "sx-1", map[string]any{"sale_ids": []string{card.CardPaymentID}, "receptor": pmReceptor()}); inv.Status != "STAMPED" {
		t.Fatalf("invoice: %s", body)
	}
	if n, _ := s.RunGlobalInvoices(context.Background(), time.Now().AddDate(0, 1, 2)); n != 1 {
		t.Fatal("global")
	}
	f.deliverOK(t, refundEvent(card, "re_sx", 1160))
	if n, _ := s.ProcessCreditNotes(context.Background(), time.Now()); n != 1 {
		t.Fatal("credit note")
	}
	free := f.speiSale(t, "SALEXP002", 1000)

	code, page, body := getSales(t, s, cardMerchant, "")
	if code != http.StatusOK {
		t.Fatalf("sales: %d %s", code, body)
	}
	byID := map[string]SaleItem{}
	for _, it := range page.Items {
		byID[it.ID] = it
	}
	c := byID[card.CardPaymentID]
	if c.InvoiceID == nil || c.InvoiceKind == nil || *c.InvoiceKind != "INDIVIDUAL" || c.InvoiceStatus == nil || *c.InvoiceStatus != "STAMPED" {
		t.Fatalf("card sale invoice: %s", body)
	}
	if len(c.CreditNotes) != 1 || c.CreditNotes[0].Status != "STAMPED" || c.CreditNotes[0].TotalCents == 0 || c.CreditNotes[0].UUID == "" {
		t.Fatalf("card sale credit notes: %s", body)
	}
	if g := byID[inGlobal]; g.InvoiceKind == nil || *g.InvoiceKind != "GLOBAL" {
		t.Fatalf("sale in the global: %s", body)
	}
	if !strings.Contains(body, `"id":"`+free+`"`) || byID[free].InvoiceID != nil {
		t.Fatalf("un-invoiced sale: %s", body)
	}
	raw := map[string][]map[string]any{}
	_ = json.Unmarshal([]byte(body), &raw)
	for _, it := range raw["items"] {
		if it["id"] == free {
			if v, ok := it["invoice_id"]; !ok || v != nil {
				t.Fatalf("invoice_id must be an explicit null: %v", it)
			}
		}
	}

	// Detail: same fields; tenant isolation; unknown sale.
	rec := serve(s, asRole(t, s, cardMerchant, auth.RoleCashier, jsonReq(http.MethodGet, "/api/v1/merchants/"+cardMerchant+"/sales/"+card.CardPaymentID, nil)))
	var d SaleDetail
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	if rec.Code != http.StatusOK || d.ID != card.CardPaymentID || d.InvoiceID == nil || *d.InvoiceID != *c.InvoiceID || len(d.CreditNotes) != 1 {
		t.Fatalf("detail: %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(s, asTenant(t, s, cardMerchant, jsonReq(http.MethodGet, "/api/v1/merchants/"+cardMerchant+"/sales/nope", nil))); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown sale: %d", rec.Code)
	}
	if rec := serve(s, asTenant(t, s, "merch_other", jsonReq(http.MethodGet, "/api/v1/merchants/"+cardMerchant+"/sales/"+card.CardPaymentID, nil))); rec.Code != http.StatusForbidden {
		t.Fatalf("other tenant: %d", rec.Code)
	}
}

// TestAuth_MembershipsExposeMerchantName: the token pair's memberships
// (merchant picker) carry the merchant's business name.
func TestAuth_MembershipsExposeMerchantName(t *testing.T) {
	s, _ := setupTestServer(t)
	sender := withTestAuth(t, s)
	tp := httpLogin(t, s, sender, "+525512340001", "")
	req := withBearer(jsonReq(http.MethodPost, "/api/v1/merchants", map[string]any{"rfc": "GARM850101ABC", "name": "Tacos Don Memo", "is_resico": true}), tp.AccessToken)
	rec := serve(s, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	rec = postJSON(t, s.Router(), "/api/v1/auth/refresh", map[string]string{"refresh_token": tp.RefreshToken})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"merchant_name":"Tacos Don Memo"`) {
		t.Fatalf("refresh memberships: %d %s", rec.Code, rec.Body.String())
	}
	tp2 := httpLogin(t, s, sender, "+525512340001", "")
	if len(tp2.Memberships) != 1 || tp2.Memberships[0].MerchantName != "Tacos Don Memo" {
		t.Fatalf("login memberships: %+v", tp2.Memberships)
	}
}
