# Acquirer (card processor) integration

Decision (gap analysis, "camino A"): Kerno does **not** authorize cards
itself. Card payments go through a certified processor (Adyen, Mercado Pago,
Clip, ... — not chosen yet). The processor's Tap to Pay SDK runs on the
phone, the processor authorizes and captures, and the processor's **signed
webhooks** are the only source of truth for the final status and for every
ledger movement. The client is never trusted for the outcome.

The backend talks to the processor only through the provider-agnostic port
`acquirer.Provider` (`pkg/acquirer`). Today the only adapter is the
deterministic `MockProvider` (`ACQUIRER_PROVIDER=mock`, tests and DEV_MODE;
rejected in production).

## 1. Port contract (`pkg/acquirer.Provider`)

| Method | Purpose | Idempotency key sent by Kerno |
|---|---|---|
| `Name()` | id in config, webhook path and DB (`card_payments.provider`) | — |
| `CreatePaymentIntent(IntentRequest)` → `Intent{ProviderPaymentID, ClientSecret, Status, ExpiresAt}` | session for the phone SDK | `kerno_intent_<card_payment_id>` |
| `GetPayment(providerPaymentID)` → `PaymentState` | provider view (support / future polling) | — |
| `Capture(CaptureRequest)` | manual-capture providers (Kerno assumes auto-capture today) | caller-defined |
| `Refund(RefundRequest)` → `RefundResult{ProviderRefundID, Status}` | refund request; ledger moves only on the webhook | `kerno_refund_<refund_id>` |
| `GetRefund(RefundLookup{ProviderPaymentID, IdempotencyKey, KernoRefundID})` → `RefundState{ProviderRefundID, Status, AmountCents}` | refund sweep (§4): `pending` / `succeeded` / `failed`; `ErrRefundNotFound` = the provider never received it; `ErrProviderUnavailable` = still unknown | looks up `kerno_refund_<refund_id>` |
| `Cancel(CancelRequest)` | cancel an uncaptured intent | `kerno_cancel_<card_payment_id>` |
| `OnboardMerchant(OnboardRequest)` → `OnboardResult{ProviderMerchantID, Status}` | create the processor sub-merchant when an operator approves the KYB (`docs/KYB.md`); the mock rejects RFCs starting with `REJECT` and times out on `TIMEOUT` | `kerno_onboard_<merchant_id>` |
| `ParseWebhookEvent(body)` → `Event` | translate the provider payload into the normalized `Event` | — |

Error contract: `ErrProviderUnavailable` = outcome unknown (timeout, reset,
5xx) → Kerno never assumes success or failure and retries with the **same**
key; `ErrProviderRejected` = definitive; `ErrPaymentNotFound`;
`ErrRefundNotFound` (only from `GetRefund`, must mean "never received", not
"lookup failed").

Optional `WebhookVerifier` (`VerifyWebhook(header, body, now)`): adapters whose
provider signs webhooks with its own scheme implement it; otherwise the server
verifies the Kerno scheme (§3).

### Mock scenarios (deterministic, by the amount's last two digits)

| Amount ends in | Outcome |
|---|---|
| `51` | tap declined (`payment.declined`, `decline_code=do_not_honor`) |
| `52` | every provider call times out (`ErrProviderUnavailable`); a refund is recorded before timing out, and `GetRefund` keeps timing out |
| other | approved: `payment.authorized` + `payment.captured` (fee 250 bps half-up, settlement T+1) |

Ids derive from the idempotency keys (SHA-256), so retries return the same
intent / refund. `MockProvider.SimulateTap` builds the webhook events of a tap.

## 2. Card payment flow

1. **Intent** — `POST /api/v1/card-payments/intents` (any member; header
   `Idempotency-Key` required, ≤ 255 chars). Body:
   `{"amount_cents": 25000, "currency": "MXN", "reference": "mesa 4"}`
   (`currency` optional, only MXN; `reference` ≤ 64 chars; `merchant_id`, if
   sent, must equal the token's tenant; a client `is_persona_moral` is
   ignored — the split input is server-derived, `docs/KYB.md`).
   `amount_cents` 1..100,000,000. The merchant must be KYB-approved and not
   blocked (`403 MERCHANT_NOT_APPROVED` / `MERCHANT_BLOCKED`).
   The row `card_payments` (status `CREATED`) is written first (unique
   `(merchant_id, intent_idempotency_key)`; same key + different body → `409
   IDEMPOTENCY_KEY_REUSED`), then the provider is called outside the
   transaction (detached from the client connection, 30 s), and the
   `provider_payment_id` is stored. Response `201` (`200` +
   `Idempotent-Replayed: true` on a retry):

   ```json
   {"card_payment_id":"cp_…","status":"CREATED","amount_cents":25000,"currency":"MXN",
    "provider":"mock","provider_payment_id":"mock_pi_…","client_secret":"mock_cs_…",
    "expires_at":"2026-11-20T18:15:00Z","created_at":"2026-11-20T18:00:00Z"}
   ```

   `client_secret` is a short-lived single-intent token for the processor SDK
   (never stored by Kerno; a retry with the same key returns the same one
   while the payment is `CREATED`). Errors: `400 MISSING_IDEMPOTENCY_KEY |
   INVALID_AMOUNT | UNSUPPORTED_CURRENCY | REFERENCE_TOO_LONG`, `404
   MERCHANT_NOT_FOUND`, `502 ACQUIRER_UNAVAILABLE` (retry with the same key),
   `502 ACQUIRER_REJECTED`, `503 ACQUIRER_NOT_CONFIGURED`.
2. **Tap** — the app starts the processor SDK with `provider_payment_id` +
   `client_secret`. The SDK result shown on the phone is informative only.
3. **Status** — `GET /api/v1/card-payments/{id}` (tenant-scoped; foreign ids →
   `404 CARD_PAYMENT_NOT_FOUND`). Poll until `final=true`:

   ```json
   {"card_payment_id":"cp_…","merchant_id":"…","provider":"mock","provider_payment_id":"mock_pi_…",
    "status":"CAPTURED","amount_cents":25000,"currency":"MXN","captured_cents":25000,
    "refunded_cents":0,"chargeback_cents":0,"final":true,
    "settlement":{ …same fields as the settle response… },"created_at":"…","updated_at":"…"}
   ```
4. **Cancel** — `POST /api/v1/card-payments/{id}/cancel` (any member, only
   `CREATED`/`AUTHORIZED`). Without a provider intent → `200` `CANCELED` at
   once; otherwise the provider is asked (`202
   {"card_payment":…, "cancel_requested":true}`) and the status moves on the
   `payment.canceled` webhook.

### State machine (`card_payments.status`)

```
CREATED ─authorized→ AUTHORIZED ─captured→ CAPTURED ─refunded→ PARTIALLY_REFUNDED ─refunded→ REFUNDED
CREATED|AUTHORIZED ─declined→ DECLINED (a new tap may still authorize/capture)
CREATED|AUTHORIZED|DECLINED ─canceled→ CANCELED
CAPTURED|PARTIALLY_REFUNDED|CHARGEBACK_REVERSED ─chargeback.opened→ CHARGEBACK ─chargeback.reversed→ CHARGEBACK_REVERSED
```

Out-of-order deliveries: an event that would move the state backwards (e.g.
`authorized` after `captured`) is recorded and **ignored**; `captured` may
arrive before `authorized`. A capture of a `CANCELED` payment or above the
intent amount is a conflict (`ALERT`, not applied).

### Capture → settlement (inside one `MoneyStore` transaction)

Same split as the legacy settle (`postSettlement`): platform fee = merchant
tariff bps half-up, IVA/ISR to `TAX_ESCROW` by RESICO bracket, RBF holdback,
RESICO accumulator, row in `payment_settlements` (PK = card payment id;
migration 010 dropped its FK to `emv_payments`). Plus the processor fee:

| Journal | Dr | Cr |
|---|---|---|
| `settle_<cp>` | `ASSET:ACQUIRER_RECEIVABLE` gross | `REVENUE:FEES`, `TAX_ESCROW`, `RBF_LOANS_RECEIVABLE`, `AVAILABLE` |
| `acq_fee_<cp>` | `EXPENSE:ACQUIRER_FEES` provider fee | `ASSET:ACQUIRER_RECEIVABLE` provider fee |

So `ACQUIRER_RECEIVABLE` holds exactly what the processor owes Kerno. Outbox
`card_payment.captured` (also `.authorized`, `.declined`, `.canceled`).

### Legacy EMV endpoints

`POST /payments/emv/authorize`, `/payments/settle` and `/payments/emv/reverse`
"approve" a payment from a TLV the phone sends: they conflict with "never
trust the client". **Decision:** `cmd/server` enables them only with
`DEV_MODE=true` (Web POS demo); otherwise they answer `410
LEGACY_CARD_FLOW_DISABLED`. `api.NewServer` keeps them on for library users
and unit tests (`SetLegacyCardFlowEnabled`).

## 3. Processor webhooks — `POST /api/v1/webhooks/acquirer/{provider}`

Public route (no JWT), `{provider}` must equal the configured adapter
(`404 UNKNOWN_ACQUIRER`). Signature (Kerno scheme, same as CoDi):

```
X-Kerno-Timestamp: <unix seconds>
X-Kerno-Signature: v1=<hex(HMAC-SHA256(ACQUIRER_WEBHOOK_SECRET, timestamp + "." + raw_body))>
```

±5 min window (`401 STALE_TIMESTAMP`), constant-time comparison, raw body ≤ 1
MiB (`401 INVALID_SIGNATURE`; `503 WEBHOOK_NOT_CONFIGURED` without a secret).
An adapter whose processor signs differently implements `WebhookVerifier`
instead. The body is translated by `ParseWebhookEvent` into the normalized
`acquirer.Event` (the mock / DEV wire format):

```json
{"id":"evt_…","type":"payment.captured","provider_payment_id":"…","kerno_payment_id":"cp_…",
 "amount_cents":11600,"fee_cents":290,"settlement_date":"2026-11-21","occurred_at":"…"}
```

| Type | Required fields | Effect (one `MoneyStore` transaction) | Outbox |
|---|---|---|---|
| `payment.authorized` | — | `CREATED`/`DECLINED` → `AUTHORIZED` | `card_payment.authorized` |
| `payment.declined` | `decline_code` | `CREATED`/`AUTHORIZED` → `DECLINED` | `card_payment.declined` |
| `payment.canceled` | — | → `CANCELED` (ignored + `ALERT` if captured) | `card_payment.canceled` |
| `payment.captured` | `amount_cents`, `fee_cents`, `settlement_date` | settle split + processor fee (§2), → `CAPTURED` | `card_payment.captured` |
| `payment.refunded` | `refund_id`, `amount_cents` (`kerno_refund_id` optional) | bounded by captured − refunded − chargebacks (else `EVENT_CONFLICT`); proportional refund reversal (same as the EMV refund: available, tax escrow, fee, RESICO month), → `PARTIALLY_REFUNDED`/`REFUNDED`, refund request `SUCCEEDED` | `card_payment.refunded` |
| `refund.failed` | `refund_id` (`kerno_refund_id` optional) | refund request `FAILED` (hold released) | `card_payment.refund_failed` |
| `chargeback.opened` | `chargeback_id`, `amount_cents` | journal `chargeback_<cp>_<cb>`: Dr merchant `AVAILABLE` (may go negative → payouts blocked), Cr `ASSET:ACQUIRER_RECEIVABLE`; → `CHARGEBACK` | `card_payment.chargeback_opened` (notifies the merchant, includes `available_after_cents`) |
| `chargeback.reversed` | `chargeback_id`, `amount_cents` | reverse journal `chargeback_rev_…`; → `CHARGEBACK_REVERSED` | `card_payment.chargeback_reversed` |
| `payout.settled` | `payout` | processor payout reconciliation (§5) | `acquirer.payout_settled` |

Exactly-once: `money_operations` key `acq:<provider>:evt:<event id>` claimed
in the posting transaction (a concurrent duplicate waits for the first and
replays its stored result), plus secondary keys per refund id
(`acq:<p>:refund:<cp>:<refund_id>`) and chargeback id, so the same refund
notified under two event ids moves money once. Responses:

| Code | Meaning | Provider should |
|---|---|---|
| `200 {"event_id","event_type","result":"applied","card_payment_id","status"}` | processed (`result` is `ignored` for a regression / duplicate) | stop |
| `409 EVENT_OUT_OF_ORDER` | depends on an event not received yet (refund / chargeback before capture, reversal before opening); nothing consumed | retry later |
| `404 CARD_PAYMENT_NOT_FOUND` | unknown `provider_payment_id` (and `kerno_payment_id`) | retry later |
| `422 EVENT_CONFLICT` | contradicts the record (capture of a canceled payment, capture above the intent, refund above the remainder); `ERROR` log `ALERT` | escalate |
| `400 INVALID_EVENT` | unparseable / missing fields | — |

## 4. Refunds through the processor

`POST /api/v1/card-payments/{id}/refunds` (owner; cashiers too with
`ALLOW_CASHIER_REFUNDS=true`), header `Idempotency-Key` required, body
`{"amount_cents": 5000}` (0 or omitted = the whole refundable remainder).
Payment must be `CAPTURED`, `PARTIALLY_REFUNDED` or `CHARGEBACK_REVERSED`.
The request row (`card_payment_refunds`, `REQUESTED`) holds its amount
against `captured − refunded − chargeback − open requests` (`400
REFUND_EXCEEDS_REMAINING`), then the provider is called (`kerno_refund_<id>`):

- acknowledged → `PENDING` + `provider_refund_id`, `202`
  `{"refund_id":"cr_…","card_payment_id":"cp_…","amount_cents":5000,"status":"PENDING","provider_refund_id":"…"}`;
- unknown outcome → stays `REQUESTED` (amount still held), `502
  ACQUIRER_UNAVAILABLE`; retry with the same key re-sends the same request;
- rejected → `FAILED` (hold released), `502 ACQUIRER_REJECTED`.

The ledger moves only on `payment.refunded`. A retry with the same key
replays the stored request (`Idempotent-Replayed: true`; another amount →
`409 IDEMPOTENCY_KEY_REUSED`). Known limit: a `payment.refunded` that arrives
before the provider refund id was stored leaves the request `PENDING` (the
refund itself is applied); an operator closes it.

### Refund sweep (MVP-29)

A request can stay `REQUESTED` forever when the provider call's outcome is
unknown (timeout, crash between the commit and the call) and the merchant
never retries; it holds its amount. `Server.SweepStaleRefunds` (worker in
`cmd/server`, every `REFUND_SWEEP_INTERVAL`, default 5 min, only with a
processor configured) handles them:

1. **Claim:** in one short transaction, `REQUESTED` rows untouched for
   `REFUND_SWEEP_AFTER` (default 15 min, min 2 min, well beyond the 30 s
   provider call) are selected `FOR UPDATE SKIP LOCKED` (concurrent replicas
   and webhooks holding a row are skipped), `sweep_attempts` is incremented
   and `updated_at` moved to now (the lease: the next attempt comes one
   threshold later). Card payment rows are only read, so the webhook's lock
   order (card payment → refund) is never inverted (migration 020).
2. **Ask the provider** (`GetRefund`, outside any transaction), then:
   - `succeeded` → a synthetic `payment.refunded` (`id = sweep_<refund>_succeeded`,
     the provider refund id) goes through `ApplyAcquirerEvent`, the **same
     path as the webhook**: the refund is posted exactly once per provider
     refund id (`claimSub`), so the real webhook, earlier, concurrent or
     later, never posts it again;
   - `failed` → synthetic `refund.failed` (or directly `FAILED` without a
     provider id), hold released, `card_payment.refund_failed`;
   - `pending` → `PENDING` + `provider_refund_id` (the webhook finishes it);
   - `ErrRefundNotFound` → `FAILED` with `last_error = EXPIRED: …`, hold
     released, outbox `card_payment.refund_expired`; a retry with the same
     `Idempotency-Key` replays the expired request (no new provider call; the
     merchant requests a new refund with a new key);
   - unavailable / anything else → stays `REQUESTED`; after
     `REFUND_SWEEP_ALERT_ATTEMPTS` (default 5) attempts every further one
     logs `ALERT refund request unresolved` (ERROR).
   Direct status changes re-lock card payment then refund and only act if
   the refund is still `REQUESTED` **and still under this sweep's lease**
   (`updated_at` unchanged since the claim; `already_resolved` otherwise). A
   merchant retry with the same `Idempotency-Key` that re-sends a
   `REQUESTED` refund touches `updated_at` before the provider call, so a
   sweep whose lookup predates that re-send never expires it (audit of
   Lote L).

`cmd/reconciler` does not run the sweep: it needs the processor adapter and
the webhook application path of `api.Server` (it only has the database).

**Chargebacks and refunds in flight.** A `chargeback.opened` does not net
`REQUESTED`/`PENDING` refunds: the processor already took the disputed funds
and Kerno cannot cancel a refund the processor may still complete. If the
refunds in flight exceed what remains after the chargeback, an `ALERT` is
logged and the outbox event carries `open_refund_cents`, so an operator can
cancel the refund at the processor; a refund that still completes beyond the
remainder arrives as `422 EVENT_CONFLICT` (never posted, operator review) —
netting it automatically would require knowing which of the two the
processor actually honored.

## 5. Processor settlement reconciliation (Kerno ← processor, T+n)

**Payout lines** arrive in the `payout.settled` webhook:

```json
{"id":"evt_…","type":"payout.settled","payout":{"payout_id":"po_…","amount_cents":29250,
 "settlement_date":"2026-11-21","items":[
   {"provider_payment_id":"…","kind":"capture","amount_cents":10000,"fee_cents":250},
   {"provider_payment_id":"…","kind":"refund","amount_cents":500}]}}
```

Item kinds and their contribution to the deposit: `capture` (+amount − fee),
`refund` (−amount), `chargeback` (−amount), `chargeback_reversal` (+amount).
One transaction (exactly-once by event id **and** by payout id):

1. Each line is matched to its card payment (rows locked in provider-id
   order). Differences become rows of `acquirer_reconciliation_items`
   (dedupe key per payout/line/kind):

   | Kind | When |
   |---|---|
   | `MISSING_PAYMENT` | line for a payment Kerno does not know or did not capture (or a chargeback line without a recorded chargeback) |
   | `AMOUNT_MISMATCH` | capture amount ≠ `captured_cents`; refund line above the refunds recorded |
   | `FEE_MISMATCH` | line fee ≠ the fee of the `payment.captured` event |
   | `DUPLICATE_SETTLEMENT` | capture already settled in another payout (or twice in this one) |
   | `PAYOUT_TOTAL_MISMATCH` | Σ line contributions ≠ deposited `amount_cents` |
   | `NOT_SETTLED` | (watcher) captured payment with no payout one day after its `settlement_date` |

2. Matched captures get `provider_payout_id` + `provider_settled_at`.
3. Journal `acq_payout_<provider>_<payout_id>`: Dr `ASSET:BANK_OPERATING`,
   Cr `ASSET:ACQUIRER_RECEIVABLE` for the **deposited** amount (what reached
   the bank); any difference stays in the receivable and is explained by the
   report items.
4. Row in `acquirer_payouts` (`RECONCILED` / `DISCREPANCY`, expected vs
   actual), outbox `acquirer.payout_settled`; every new item is logged as
   `ERROR` `ALERT acquirer reconciliation discrepancy`.

**Overdue settlements**: `pkg/payouts.AcquirerSettlementWatcher` runs in
`cmd/server` every `ACQUIRER_RECONCILE_INTERVAL` (default 1h, with a
database) and as a one-shot job (`go run ./cmd/reconciler`, replaces the old
stub; needs `DATABASE_URL`). Settlement dates are Mexico City calendar days.

**Operator report** (role `operator`; others `403 ROLE_FORBIDDEN`):

- `GET /api/v1/acquirer/reconciliation?status=OPEN|RESOLVED|ALL&limit=N` →
  `{"items":[{"id","kind","provider","payout_id","card_payment_id","provider_payment_id","expected_cents","actual_cents","detail","status","resolution_note","resolved_by","created_at","resolved_at"}],"payouts":[{"provider","payout_id","amount_cents","expected_cents","settlement_date","item_count","discrepancy_count","status","created_at"}]}`
- `POST /api/v1/acquirer/reconciliation/{item}/resolve {"note":"…"}` → `RESOLVED`
  with `resolved_by` = the operator's user id (migration 011; `404
  OPEN_ITEM_NOT_FOUND` if not open). Resolving moves no money. Correcting journals, if any, are
  posted by backoffice tooling (next batch).

## 6. Plugging a real processor (checklist)

1. Contract the processor (certified Tap to Pay on Android in Mexico:
   Adyen, Mercado Pago Point/Tap, Clip, Stripe Terminal if available) and get
   sandbox credentials.
2. Add `pkg/acquirer/<name>.go` implementing `Provider` (and `WebhookVerifier`
   if it signs webhooks with its own scheme) and register it in
   `acquirer.New`. Map its statuses to `Status` and its webhook types to the
   normalized `EventType`s (§3); put Kerno's `card_payment_id` in the intent
   metadata so webhooks can carry `kerno_payment_id`, and the refund's
   `RefundRequest.KernoRefundID` in the refund metadata so refund webhooks can
   carry `kerno_refund_id` (matches a refund whose provider id was never
   stored because the call timed out, releasing its hold).
3. Idempotency: pass Kerno's keys (`kerno_intent_…`, `kerno_refund_…`,
   `kerno_cancel_…`, `kerno_onboard_…`; `OnboardMerchant` maps to the
   processor's sub-merchant / account onboarding API) as the provider's idempotency keys; return
   `ErrProviderUnavailable` only for unknown outcomes.
4. Webhooks: configure the endpoint
   `https://<host>/api/v1/webhooks/acquirer/<name>` and the secret in
   `ACQUIRER_WEBHOOK_SECRET`; make sure the provider retries non-2xx
   responses (out-of-order handling relies on it). Capture mode: automatic
   (or call `Capture` after `payment.authorized` for manual-capture
   providers). Fees and expected settlement date must come with the capture
   (or a fee report) for `FEE_MISMATCH` to work.
5. Payouts: translate the provider's settlement report / payout webhook
   into `payout.settled` lines (capture, refund, chargeback, reversal).
6. Android: replace the mock SDK step with the provider SDK (session =
   `provider_payment_id` + `client_secret`), then poll `GET
   /api/v1/card-payments/{id}` until `final=true`.
7. Config: `ACQUIRER_PROVIDER=<name>` (the production guard rejects `mock`
   and requires a ≥ 32-byte `ACQUIRER_WEBHOOK_SECRET`); keep `DEV_MODE` off
   so the legacy EMV endpoints stay disabled.
8. Refund lookup: implement `GetRefund` by the refund idempotency key or
   the `kerno_refund_id` metadata; return `ErrRefundNotFound` only when the
   provider confirms it has no such refund (the sweep then releases the
   hold), `ErrProviderUnavailable` for anything inconclusive.
9. Tests: an adapter contract test against the provider sandbox (create /
   refund / cancel idempotency, refund lookup, webhook parsing fixtures,
   signature).

## DEV_MODE simulation

`POST /api/v1/dev/card-payments/{id}/simulate` (DEV_MODE + mock acquirer,
own payments; `404` otherwise) with `{"outcome":"tap"}` (default: the mock
scenario of the amount), `{"outcome":"cancel"}` or
`{"outcome":"refund","refund_id":"cr_…"}` signs the corresponding events with
the server secret and runs them through the webhook handler. Response
`{"events":[<webhook results>]}`.
