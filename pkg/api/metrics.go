package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"softpos/pkg/metrics"
	"softpos/pkg/store"
)

// Process metrics (MVP-30, docs/OBSERVABILITY.md), served by cmd/server on
// the separate METRICS_ADDR listener. Labels are bounded: route patterns,
// status codes, sources, outcomes; never tenant ids or personal data.
var (
	httpRequests = metrics.Default.NewCounter("kerno_http_requests_total",
		"HTTP requests by route pattern, method and status code.", "route", "method", "status")
	httpLatency = metrics.Default.NewHistogram("kerno_http_request_duration_seconds",
		"HTTP request latency by route pattern and method.", metrics.DefaultLatencyBuckets, "route", "method")
	webhookFailures = metrics.Default.NewCounter("kerno_webhook_failures_total",
		"Webhook deliveries answered with a 4xx/5xx, by source (acquirer, codi, stp) and status code.", "source", "status")
	acquirerDiscrepancies = metrics.Default.NewCounter("kerno_acquirer_reconciliation_mismatches_total",
		"Processor payout reconciliation differences found, by kind.", "kind")
	refundSweepOutcomes = metrics.Default.NewCounter("kerno_refund_sweep_total",
		"Refund requests handled by the refund sweep, by outcome.", "outcome")
	refundSweepAlerts = metrics.Default.NewCounter("kerno_refund_sweep_alerts_total",
		"Refund requests still unresolved after the alert threshold of attempts.")
	sat69bLoads = metrics.Default.NewCounter("kerno_sat69b_loads_total",
		"SAT 69-B list load attempts by result (LOADED, UNCHANGED, REJECTED_SHRINK, REJECTED_CHANGES, FAILED).", "status")
	sat69bRetroscreen = metrics.Default.NewCounter("kerno_sat69b_retroscreen_total",
		"Merchants blocked / unblocked / excepted by the 69-B retroscreen.", "action")
)

// webhookSources are the known webhook path segments (/api/v1/webhooks/<source>/…).
var webhookSources = map[string]bool{"acquirer": true, "codi": true, "stp": true}

const webhooksPrefix = "/api/v1/webhooks/"

// metricsMiddleware records request counts, latency and webhook failures.
// The route label is the ServeMux pattern ("POST /api/v1/card-payments/{id}/refunds"),
// "unmatched" for paths no route serves.
func (s *Server) metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(sw, r)
		route := "unmatched"
		if _, pattern := s.router.Handler(r); pattern != "" {
			route = pattern
		}
		method := r.Method
		switch method {
		case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
		default:
			method = "OTHER"
		}
		status := strconv.Itoa(sw.statusCode)
		httpRequests.Inc(route, method, status)
		httpLatency.Observe(metrics.Since(start), route, method)
		if sw.statusCode >= 400 && strings.HasPrefix(r.URL.Path, webhooksPrefix) {
			source, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, webhooksPrefix), "/")
			if !webhookSources[source] {
				source = "other"
			}
			webhookFailures.Inc(source, status)
		}
	})
}

// Gauges sampled from the database (store.SampleOps) by cmd/server.
var (
	outboxEvents = metrics.Default.NewGauge("kerno_outbox_events",
		"Outbox events not yet published, by status (FAILED = dead-lettered).", "status")
	outboxOldestPendingAge = metrics.Default.NewGauge("kerno_outbox_oldest_pending_age_seconds",
		"Age of the oldest PENDING/PROCESSING outbox event.")
	payoutsUnresolved = metrics.Default.NewGauge("kerno_payouts_unresolved",
		"SPEI payouts not resolved yet (RESERVED / PENDING_RECONCILIATION).", "status")
	payoutsOldestUnresolvedAge = metrics.Default.NewGauge("kerno_payouts_oldest_unresolved_age_seconds",
		"Seconds since the oldest unresolved payout last changed.")
	reconciliationOpen = metrics.Default.NewGauge("kerno_acquirer_reconciliation_open_items",
		"Open processor reconciliation items, by kind.", "kind")
	refundsRequested = metrics.Default.NewGauge("kerno_card_refunds_requested",
		"Card refund requests in REQUESTED (processor outcome unknown).")
	refundsOldestRequestedAge = metrics.Default.NewGauge("kerno_card_refunds_oldest_requested_age_seconds",
		"Age of the oldest REQUESTED card refund.")
	sat69bListLoaded = metrics.Default.NewGauge("kerno_sat69b_list_loaded", "1 if a SAT 69-B list is loaded.")
	sat69bListRows   = metrics.Default.NewGauge("kerno_sat69b_list_rows", "RFCs in the loaded SAT 69-B list.")
	sat69bListAge    = metrics.Default.NewGauge("kerno_sat69b_list_verified_age_seconds",
		"Seconds since the loaded SAT 69-B list was last verified against the source.")
	opsSampleErrors = metrics.Default.NewCounter("kerno_ops_sample_errors_total",
		"Failures sampling the operational gauges from the database.")
)

// RecordOpsSnapshot publishes a database sample on the gauges.
func RecordOpsSnapshot(snap store.OpsSnapshot) {
	for _, st := range store.OpsOutboxStatuses {
		outboxEvents.Set(float64(snap.OutboxByStatus[st]), st)
	}
	outboxOldestPendingAge.Set(snap.OutboxOldestPendingAge)
	for _, st := range store.OpsPayoutStatuses {
		payoutsUnresolved.Set(float64(snap.PayoutsUnresolved[st]), st)
	}
	payoutsOldestUnresolvedAge.Set(snap.PayoutsOldestUnresolved)
	for _, k := range store.OpsReconKinds {
		reconciliationOpen.Set(float64(snap.ReconciliationOpen[k]), k)
	}
	refundsRequested.Set(float64(snap.RefundsRequested))
	refundsOldestRequestedAge.Set(snap.RefundsOldestRequested)
	loaded := 0.0
	if snap.SAT69BLoaded {
		loaded = 1
	}
	sat69bListLoaded.Set(loaded)
	sat69bListRows.Set(float64(snap.SAT69BRows))
	sat69bListAge.Set(snap.SAT69BVerifiedAge)
}

// RecordOpsSampleError counts a failed database sample.
func RecordOpsSampleError() { opsSampleErrors.Inc() }
