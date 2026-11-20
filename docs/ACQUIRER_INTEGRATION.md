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
| `Cancel(CancelRequest)` | cancel an uncaptured intent | `kerno_cancel_<card_payment_id>` |
| `ParseWebhookEvent(body)` → `Event` | translate the provider payload into the normalized `Event` | — |

Error contract: `ErrProviderUnavailable` = outcome unknown (timeout, reset,
5xx) → Kerno never assumes success or failure and retries with the **same**
key; `ErrProviderRejected` = definitive; `ErrPaymentNotFound`.

Optional `WebhookVerifier` (`VerifyWebhook(header, body, now)`): adapters whose
provider signs webhooks with its own scheme implement it; otherwise the server
verifies the Kerno scheme (§3).

### Mock scenarios (deterministic, by the amount's last two digits)

| Amount ends in | Outcome |
|---|---|
| `51` | tap declined (`payment.declined`, `decline_code=do_not_honor`) |
| `52` | every provider call times out (`ErrProviderUnavailable`) |
| other | approved: `payment.authorized` + `payment.captured` (fee 250 bps half-up, settlement T+1) |

Ids derive from the idempotency keys (SHA-256), so retries return the same
intent / refund. `MockProvider.SimulateTap` builds the webhook events of a tap.

## 2. Card payment flow

1. **Intent** — `POST /api/v1/card-payments/intents` (any member; header
   `Idempotency-Key` required, ≤ 255 chars). Body:
   `{"amount_cents": 25000, "currency": "MXN", "reference": "mesa 4", "is_persona_moral": false}`
   (`currency` optional, only MXN; `reference` ≤ 64 chars; `merchant_id`, if
   sent, must equal the token's tenant). `amount_cents` 1..100,000,000.
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
| `payment.refunded` | `refund_id`, `amount_cents` | proportional refund reversal (same as the EMV refund: available, tax escrow, fee, RESICO month), → `PARTIALLY_REFUNDED`/`REFUNDED`, refund request `SUCCEEDED` | `card_payment.refunded` |
| `refund.failed` | `refund_id` | refund request `FAILED` (hold released) | `card_payment.refund_failed` |
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

## DEV_MODE simulation

`POST /api/v1/dev/card-payments/{id}/simulate` (DEV_MODE + mock acquirer,
own payments; `404` otherwise) with `{"outcome":"tap"}` (default: the mock
scenario of the amount), `{"outcome":"cancel"}` or
`{"outcome":"refund","refund_id":"cr_…"}` signs the corresponding events with
the server secret and runs them through the webhook handler. Response
`{"events":[<webhook results>]}`.
