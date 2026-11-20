package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"softpos/pkg/acquirer"
	"softpos/pkg/auth"
)

func getSales(t *testing.T, s *Server, merchant, query string) (int, SalesPage, string) {
	t.Helper()
	rec := serve(s, asRole(t, s, merchant, auth.RoleCashier, jsonReq(http.MethodGet, "/api/v1/merchants/"+merchant+"/sales"+query, nil)))
	var page SalesPage
	_ = json.Unmarshal(rec.Body.Bytes(), &page)
	return rec.Code, page, rec.Body.String()
}

// TestSales_UnifiedNewestFirstWithCursor: card payments, a SPEI abono and a
// CoDi confirmation of one merchant, paginated without gaps or duplicates.
func TestSales_UnifiedNewestFirstWithCursor(t *testing.T) {
	s, _ := setupTestServer(t)
	const m, clabe = "merch_sales", "646180123400000007"
	if err := s.RegisterMerchantCLABE(clabe, m, "GARM850101ABC"); err != nil {
		t.Fatal(err)
	}
	_ = s.RegisterMerchant("merch_sales_other", "GARM850101ABC", "Other", true)
	s.SetAcquirerProvider(acquirer.NewMockProvider(acquirer.MockConfig{}))
	s.SetCoDiWebhookSecret(testCoDiWebhookSecret)

	for i := 0; i < 3; i++ {
		if code, _, body := createIntent(t, s, m, fmt.Sprintf("sale-%d", i), map[string]any{"amount_cents": 1000 + i}); code != http.StatusCreated {
			t.Fatalf("intent: %d %s", code, body)
		}
	}
	createIntent(t, s, "merch_sales_other", "foreign", map[string]any{"amount_cents": 999})
	if rec := serve(s, jsonReq(http.MethodPost, "/api/v1/webhooks/stp/abono", signedAbono(t, s, "SPEISALES01", clabe, 50000))); rec.Code != http.StatusOK {
		t.Fatalf("abono: %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(s, signedCoDiRequest(testCoDiWebhookSecret, time.Now(), codiPayload("BNXSALES001"))); rec.Code != http.StatusOK {
		t.Fatalf("codi: %d %s", rec.Code, rec.Body.String())
	}

	var all []SaleItem
	cursor := ""
	for pages := 0; pages < 5; pages++ {
		q := "?limit=2"
		if cursor != "" {
			q += "&before=" + cursor
		}
		code, page, body := getSales(t, s, m, q)
		if code != http.StatusOK {
			t.Fatalf("sales: %d %s", code, body)
		}
		all = append(all, page.Items...)
		if page.NextCursor == nil {
			break
		}
		if len(page.Items) != 2 {
			t.Fatalf("a page with next_cursor must be full: %s", body)
		}
		cursor = *page.NextCursor
	}
	if len(all) != 5 {
		t.Fatalf("want 5 sales (3 card, 1 spei, 1 codi), got %d: %+v", len(all), all)
	}
	kinds := map[string]int{}
	seen := map[string]bool{}
	for i, it := range all {
		kinds[it.Kind]++
		if seen[it.ID] {
			t.Fatalf("duplicate %s across pages", it.ID)
		}
		seen[it.ID] = true
		if i > 0 && all[i-1].CreatedAt < it.CreatedAt {
			t.Fatalf("not newest first: %s before %s", all[i-1].CreatedAt, it.CreatedAt)
		}
		switch it.Kind {
		case "card":
			if it.CardPaymentID != it.ID || it.Status != "CREATED" {
				t.Errorf("card item: %+v", it)
			}
		case "spei":
			if it.AmountCents != 50000 || it.Status != "RECEIVED" || it.CardPaymentID != "" {
				t.Errorf("spei item: %+v", it)
			}
		case "codi":
			if it.AmountCents != 11600 || it.Status != "CONFIRMED" {
				t.Errorf("codi item: %+v", it)
			}
		}
	}
	if kinds["card"] != 3 || kinds["spei"] != 1 || kinds["codi"] != 1 {
		t.Fatalf("kinds = %v", kinds)
	}
	// Contract: next_cursor is present (null) on the last page.
	_, _, body := getSales(t, s, m, "")
	var raw map[string]json.RawMessage
	_ = json.Unmarshal([]byte(body), &raw)
	if string(raw["next_cursor"]) != "null" {
		t.Fatalf("last page next_cursor must be null: %s", body)
	}
}

func TestSales_TenantAndValidation(t *testing.T) {
	s, _ := setupTestServer(t)
	_ = s.RegisterMerchant("merch_sales_v", "GARM850101ABC", "V", true)
	if rec := serve(s, asTenant(t, s, "merch_sales_other", jsonReq(http.MethodGet, "/api/v1/merchants/merch_sales_v/sales", nil))); rec.Code != http.StatusForbidden {
		t.Fatalf("foreign merchant: want 403, got %d", rec.Code)
	}
	if rec := serve(s, jsonReq(http.MethodGet, "/api/v1/merchants/merch_sales_v/sales", nil)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: want 401, got %d", rec.Code)
	}
	for q, want := range map[string]string{"?limit=0": "INVALID_LIMIT", "?limit=101": "INVALID_LIMIT", "?before=***": "INVALID_CURSOR", "?before=bm9waXBl": "INVALID_CURSOR"} {
		if code, _, body := getSales(t, s, "merch_sales_v", q); code != http.StatusBadRequest || !contains(body, want) {
			t.Errorf("%s: want 400 %s, got %d %s", q, want, code, body)
		}
	}
	if code, page, body := getSales(t, s, "merch_sales_v", ""); code != http.StatusOK || page.Items == nil || len(page.Items) != 0 || page.NextCursor != nil {
		t.Fatalf("empty: %d %s", code, body)
	}
}

// TestRefresh_GraceWindowOverHTTP: the app re-sends the previous refresh token
// (response lost) and gets the same refresh token back.
func TestRefresh_GraceWindowOverHTTP(t *testing.T) {
	s, _ := setupTestServer(t)
	sender := withTestAuth(t, s)
	tp := httpLogin(t, s, sender, "+525510009999", "")
	first := postJSON(t, s.Router(), "/api/v1/auth/refresh", map[string]string{"refresh_token": tp.RefreshToken})
	second := postJSON(t, s.Router(), "/api/v1/auth/refresh", map[string]string{"refresh_token": tp.RefreshToken})
	var a, b auth.TokenPair
	_ = json.Unmarshal(first.Body.Bytes(), &a)
	_ = json.Unmarshal(second.Body.Bytes(), &b)
	if first.Code != http.StatusOK || second.Code != http.StatusOK || a.RefreshToken == "" || a.RefreshToken != b.RefreshToken {
		t.Fatalf("grace: %d %s / %d %s", first.Code, first.Body.String(), second.Code, second.Body.String())
	}
	third := postJSON(t, s.Router(), "/api/v1/auth/refresh", map[string]string{"refresh_token": tp.RefreshToken})
	if third.Code != http.StatusUnauthorized || !contains(third.Body.String(), "REFRESH_TOKEN_REUSED") {
		t.Fatalf("second replay: want 401 REFRESH_TOKEN_REUSED, got %d %s", third.Code, third.Body.String())
	}
}
