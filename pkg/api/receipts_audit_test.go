package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestReceipts_TokenNotInRequestLogs: the receipt token is the credential
// of /r/{token} (90 days, autofactura); the request log must not carry it.
func TestReceipts_TokenNotInRequestLogs(t *testing.T) {
	logs := captureLogs(t)
	h := LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	const tok = "AAAAAAAAAAAAAAAAsecretTOKENsecretTOKENsecret"
	for _, p := range []string{"/r/" + tok, "/r/" + tok + "/invoice", "/r/" + tok + "/invoice/pdf"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, p, nil))
	}
	out := logs.String()
	if strings.Contains(out, tok) || !strings.Contains(out, "/r/{token}/invoice/pdf") {
		t.Fatalf("request log: %s", out)
	}
}

// TestReceipts_AutofacturaFormBodyBounded: the public form is parsed with
// the same 8 KiB bound as the JSON body (ParseForm alone reads up to 10 MB).
func TestReceipts_AutofacturaFormBodyBounded(t *testing.T) {
	f := setupReceiptServer(t)
	s := f.s
	_, d, body := saleDetail(t, s, f.speiSale(t, "RCPTBIG001", 11600))
	if d.ReceiptURL == "" {
		t.Fatalf("receipt: %s", body)
	}
	big := "rfc=EKU9003173C9&nombre=" + strings.Repeat("A", 64<<10)
	req := httptest.NewRequest(http.MethodPost, d.ReceiptURL+"/invoice", strings.NewReader(big))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if rec := serve(s, req); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "INVALID_FORM") {
		t.Fatalf("oversized form: %d %s", rec.Code, rec.Body.String())
	}
}
