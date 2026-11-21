package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"softpos/pkg/metrics"
)

// TestMetricsMiddleware (MVP-30): requests are counted by route pattern (no
// ids from the path), webhook failures by source, and the exposition never
// contains a merchant id.
func TestMetricsMiddleware(t *testing.T) {
	s, mock := setupWebhookCardServer(t)
	_ = mock
	route := "GET /api/v1/card-payments/{id}"
	before := httpRequests.Value(route, "GET", "404")
	rec := serve(s, asTenant(t, s, cardMerchant, jsonReq(http.MethodGet, "/api/v1/card-payments/cp_does_not_exist", nil)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get: %d", rec.Code)
	}
	if got := httpRequests.Value(route, "GET", "404"); got != before+1 {
		t.Fatalf("route counter: %v -> %v", before, got)
	}
	if httpLatency.Count(route, "GET") == 0 {
		t.Fatal("latency not observed")
	}
	// Unsigned acquirer webhook: 401, counted as a webhook failure.
	wb := webhookFailures.Value("acquirer", "401")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/acquirer/mock", strings.NewReader(`{}`))
	if rec := serve(s, req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned webhook: %d", rec.Code)
	}
	if webhookFailures.Value("acquirer", "401") != wb+1 {
		t.Fatal("webhook failure not counted")
	}
	// Unknown paths share one label.
	serve(s, httptest.NewRequest(http.MethodGet, "/api/v1/webhooks/"+cardMerchant+"/x", nil))
	var b bytes.Buffer
	if err := metrics.Default.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), cardMerchant) || strings.Contains(b.String(), "cp_does_not_exist") {
		t.Fatalf("metrics must not carry tenant / payment ids:\n%s", b.String())
	}
	if !strings.Contains(b.String(), `kerno_webhook_failures_total{source="other"`) {
		t.Fatalf("unknown webhook source must be 'other':\n%s", b.String())
	}
}
