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
