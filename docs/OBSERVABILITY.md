# Observability (MVP-30)

The server exposes in-process metrics in the Prometheus text format
(version 0.0.4) and keeps the structured `slog` logs (the `ALERT …` ERROR
lines stay the source for log-based alerting). Suggested alert rules:
[`deploy/prometheus/kerno-alerts.yml`](../deploy/prometheus/kerno-alerts.yml).

## Endpoint

| Variable | Default | Meaning |
|---|---|---|
| `METRICS_ADDR` | empty (off) | listen address of a **separate** HTTP listener serving `GET /metrics` (e.g. `127.0.0.1:9090`, or the pod IP for an in-cluster Prometheus). Never the public API port: metrics are not routed through the API gateway. |
| `METRICS_TOKEN` | empty | when set, `/metrics` requires `Authorization: Bearer <token>` (constant-time compare). Secret, ≥ 32 bytes; the production guard requires it whenever `METRICS_ADDR` is not a loopback address. Prometheus: `authorization: {type: Bearer, credentials_file: …}`. |
| `METRICS_SAMPLE_INTERVAL` | `30s` | how often the database gauges are sampled (only with `DATABASE_URL`). |

Implementation: `pkg/metrics` (≈ 300 lines, no dependency): counters,
gauges and histograms with label sets, a cap of 2,000 series per family (a
label cardinality bug cannot exhaust memory), text exposition and the token
handler. `prometheus/client_golang` was not added: the repo keeps a minimal
dependency set (money/PCI code), and the few metric types needed here do not
justify ~10 transitive modules. If OpenTelemetry tracing is adopted later,
swapping the registry behind the same metric names is mechanical.

**Labels never carry personal or tenant data**: no merchant / user ids,
phones, RFCs, amounts or payment ids. Routes use the `ServeMux` pattern
(`POST /api/v1/card-payments/{id}/refunds`), `unmatched` for anything else;
unknown webhook sources are `other` (`pkg/api/metrics_test.go` asserts that
a merchant id never reaches the exposition).

Counters are per process (sum them across replicas with `sum(...)`); the
database gauges are global (every replica reports the same value: use
`max(...)`).

## Metrics

### HTTP (middleware, every request)

| Metric | Type | Labels |
|---|---|---|
| `kerno_http_requests_total` | counter | `route`, `method`, `status` |
| `kerno_http_request_duration_seconds` | histogram (5 ms … 10 s) | `route`, `method` |
| `kerno_webhook_failures_total` | counter | `source` (`acquirer`, `codi`, `stp`, `other`), `status` — webhook deliveries answered ≥ 400 (bad signature, out of order, conflict, 5xx) |

### Money flows

| Metric | Type | Labels | Source |
|---|---|---|---|
| `kerno_acquirer_reconciliation_mismatches_total` | counter | `kind` | differences found when a processor payout is reconciled |
| `kerno_acquirer_reconciliation_open_items` | gauge (DB) | `kind` | OPEN items in `acquirer_reconciliation_items` (incl. `NOT_SETTLED` from the settlement watcher) |
| `kerno_payouts_unresolved` | gauge (DB) | `status` (`RESERVED`, `PENDING_RECONCILIATION`) | SPEI payouts not resolved |
| `kerno_payouts_oldest_unresolved_age_seconds` | gauge (DB) | — | stuck payouts |
| `kerno_card_refunds_requested` | gauge (DB) | — | refunds with unknown processor outcome |
| `kerno_card_refunds_oldest_requested_age_seconds` | gauge (DB) | — | |
| `kerno_refund_sweep_total` | counter | `outcome` (`succeeded`, `pending`, `failed`, `expired`, `already_resolved`, `unknown`) | refund sweep (docs/ACQUIRER_INTEGRATION.md §4) |
| `kerno_refund_sweep_alerts_total` | counter | — | refunds unknown after `REFUND_SWEEP_ALERT_ATTEMPTS` |

### Outbox

| Metric | Type | Labels |
|---|---|---|
| `kerno_outbox_events` | gauge (DB) | `status` (`PENDING`, `PROCESSING`, `FAILED` = dead-lettered) |
| `kerno_outbox_oldest_pending_age_seconds` | gauge (DB) | — |

### SAT 69-B

| Metric | Type | Labels |
|---|---|---|
| `kerno_sat69b_loads_total` | counter | `status` (`LOADED`, `UNCHANGED`, `REJECTED_SHRINK`, `REJECTED_CHANGES`, `FAILED`) |
| `kerno_sat69b_retroscreen_total` | counter | `action` (`blocked`, `unblocked`, `excepted`) |
| `kerno_sat69b_list_loaded` / `kerno_sat69b_list_rows` / `kerno_sat69b_list_verified_age_seconds` | gauges (DB) | — |

### Self

| Metric | Type |
|---|---|
| `kerno_ops_sample_errors_total` | counter — the database sample failed |

## Suggested alerts

`deploy/prometheus/kerno-alerts.yml` (12 rules, `severity: page|ticket`):
webhook failures, processor `EVENT_CONFLICT`, open reconciliation items,
payouts stuck > 1 h, refunds stuck / sweep alerts, outbox dead letters and
backlog > 15 min, 69-B list missing / near stale and refused loads, API 5xx
ratio > 2 %, p95 latency > 2 s, failing database sample. Thresholds are
starting points.

## Pending

Routing alerts to a real pager (Alertmanager → PagerDuty / Opsgenie) and the
on-call rotation; distributed tracing (OpenTelemetry) across the server,
outbox publisher and external rails; dashboards; log shipping with
retention.
