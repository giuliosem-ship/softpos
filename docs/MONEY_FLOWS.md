# Money flows

Every money-moving handler runs inside one **unit of work** (`store.MoneyStore`
/ `store.MoneyTx`, `pkg/store/money.go`):

| Mode | Implementation | Atomicity |
|---|---|---|
| PostgreSQL (`DATABASE_URL`) | `store.SQLMoneyStore`: one `database/sql` transaction | payment status, settlement record, exactly-once key, ledger journal + balances, RESICO accumulator and outbox event commit or roll back together |
| In-memory (dev/test) | `store.MemoryMoneyStore` | units of work serialized by one mutex; writes buffered and applied only after the handler succeeds and every account was validated |

Rules:
- Amounts are `int64` cents; percentages are basis points rounded half-up
  (`bpsHalfUp`). No floats (the settle response exposes `isr_rate_bps`).
- A posting error is never ignored: the transaction rolls back and the API
  answers `500 LEDGER_POSTING_FAILED` (or the mapped 4xx).
- Lock order inside a transaction: payment row → settlement row → operation
  key → monthly-sales row → ledger accounts (sorted by code). Global accounts
  (`ASSET:ACQUIRER_RECEIVABLE`, `ASSET:STP_CONCENTRATOR`, `REVENUE:FEES`) are hot
  rows: postings that touch them serialize.
- Outbox events are written with `SaveEventTx` inside the same transaction, so
  they exist only if the money moved. Delivery is at-least-once (consumers
  deduplicate on `aggregate_id` + `event_type`).

Account codes: `LIABILITY:MERCHANT:<id>:AVAILABLE`, `LIABILITY:MERCHANT:<id>:TAX_ESCROW`,
`ASSET:RBF_LOANS_RECEIVABLE:<id>` (per merchant, created at registration) and the
global `ASSET:ACQUIRER_RECEIVABLE`, `ASSET:STP_CONCENTRATOR`, `REVENUE:FEES`.

## Card settle — `POST /api/v1/payments/settle`

Body: `payment_id` (required), `is_persona_moral`, optional
`gross_amount_cents` (only cross-checked: a different value → `409
AMOUNT_MISMATCH`). Client `platform_fee_cents` / `rbf_sweep_cents` are ignored.

1. Lock the payment. Unknown id or another tenant's payment → `404
   PAYMENT_NOT_FOUND`; `VOIDED` → `409 INVALID_PAYMENT_STATE`.
2. Already `SETTLED`/`REFUNDED` → return the stored settlement (same body,
   nothing posted). Concurrent settles serialize on the payment row; exactly
   one posts.
3. `gross` = authorized amount. Lock the month's RESICO accumulator, compute
   `fiscal.CalculateResicoTaxSplit`. Platform fee = `gross × tariff_bps`
   (half-up); tariff = `merchants.platform_fee_bps`, or
   `DEFAULT_PLATFORM_FEE_BPS` (default 200 = 2 %) when NULL. RBF sweep =
   in-memory RBF engine holdback (0 without an active loan).
4. Journal `settle_<payment_id>`:

   | Account | Dr | Cr |
   |---|---|---|
   | `ASSET:ACQUIRER_RECEIVABLE` | gross | |
   | `REVENUE:FEES` | | fee |
   | `TAX_ESCROW` | | IVA + ISR (IVA only when the buyer is a persona moral that withholds ISR) |
   | `ASSET:RBF_LOANS_RECEIVABLE:<id>` | | rbf sweep |
   | `AVAILABLE` | | remainder |

5. Payment `AUTHORIZED → SETTLED`, accumulator `+= base`, row in
   `payment_settlements` (PK = payment id), outbox `payment.settled`.

The RBF loan state lives in process memory (phase 2): it is updated after the
commit, and settles of a merchant with an active loan are serialized
in-process.

## CoDi confirmation — `POST /api/v1/webhooks/codi/confirm`

HMAC-signed (see `docs/AUTH.md`). Exactly-once key: `codi:confirm:<tracking_key>`
in `money_operations`, claimed with `INSERT … ON CONFLICT DO NOTHING` inside the
posting transaction. A concurrent duplicate blocks on the key until the first
transaction ends, then replays its stored response; if the first rolled back,
the duplicate posts. Posting: journal `codi_<tracking_key>` with the same split
as a card settle but fee 0; accumulator `+= base`; outbox `codi.confirmed`.

## STP abono — `POST /api/v1/webhooks/stp/abono`

Signed by STP (RSA or HMAC). Exactly-once key: `stp:abono:<claveRastreo>`
(required). Journal `spei_in_<claveRastreo>`: Dr `ASSET:STP_CONCENTRATOR`, Cr
`AVAILABLE` (no tax split yet); outbox `spei.abono_received`.

## Refund / void — `POST /api/v1/payments/emv/reverse`

Body: `payment_id`, optional `refund_cents` (0 = whole remaining amount);
optional `Idempotency-Key` header (key `m:<tenant>:reverse:<payment>:<key>` in
`money_operations`; a retry replays the first response).

- `AUTHORIZED` → `VOIDED` (nothing was posted; partial voids → 400). Any member
  may void. Outbox `payment.voided`.
- `SETTLED` → refund, **owner only** unless `ALLOW_CASHIER_REFUNDS=true`
  (decision: refunds move money out of the merchant balance, so they follow
  the payout rule; voids of an un-settled sale stay open to cashiers).
  `refund_cents` must be ≤ `gross − refunded_gross` (`400
  REFUND_EXCEEDS_REMAINING`). The settlement record is reversed
  **proportionally with cumulative targets**: for each component *x* (base,
  tax escrow, fee) `target = x × refunded_total / gross` (half-up; exact *x*
  on the last refund) and the delta to the already refunded value is posted,
  so partial refunds always add up to the full reversal. Journal
  `refund_<payment>_<refunded_total>`:

  | Account | Dr | Cr |
  |---|---|---|
  | `AVAILABLE` | refund − Δtax − Δfee | |
  | `TAX_ESCROW` | Δtax (IVA + ISR set-aside) | |
  | `REVENUE:FEES` | Δfee | |
  | `ASSET:ACQUIRER_RECEIVABLE` | | refund |

  The RESICO accumulator of the **settlement month** decreases by Δbase
  (floored at 0); the settlement's refund columns are updated; the payment
  becomes `REFUNDED` when fully refunded (stays `SETTLED` while partially
  refunded). Outbox `payment.refunded`. The RBF holdback is not reversed (the
  loan repayment stands, the merchant's available balance bears it). The
  available balance may go negative if the merchant already withdrew the
  funds (blocks further payouts).

## Settlement account and payouts

The merchant's own bank account (settlement CLABE, `merchants.settlement_clabe`
+ `settlement_clabe_status` NONE/PENDING/VERIFIED/REJECTED, migration 008) is
distinct from the server-assigned virtual collection CLABE.

- `PUT /api/v1/merchants/{id}/settlement-account {"clabe"}` (owner): valid
  checksum, not any merchant's collection CLABE. Every change resets the
  status through `SettlementAccountVerifier` (stub `PendingAccountVerifier` →
  `PENDING`; a real ownership check holder-RFC = merchant RFC is pending).
- `POST …/settlement-account/verify {"clabe","status":"VERIFIED|REJECTED"}`
  (operator; owner in `DEV_MODE`): applies only if `clabe` is still the
  registered one. `GET …/settlement-account` shows it. With `DEV_MODE` the
  demo merchant is seeded with a verified account.

`POST /api/v1/disbursements {"amount_cents","concept"}` (owner): destination is
always the registered account; `destination_clabe`, if sent, must equal it
(`403 DESTINATION_CLABE_NOT_ALLOWED`); not registered / not `VERIFIED` → `409`.

1. One transaction: lock `AVAILABLE` and `ASSET:STP_CONCENTRATOR` (sorted),
   check balance ≥ amount (`400 INSUFFICIENT_FUNDS`), post `disb_<clave>` (Dr
   `AVAILABLE`, Cr `ASSET:STP_CONCENTRATOR`), outbox `disbursement.requested`.
   Concurrent payouts serialize on the account lock and cannot overdraw.
2. `registraOrden` to STP outside the transaction, detached from the client
   connection (30 s timeout): a disconnect cannot abort an order in flight.
3. Definitive failure (STP rejected the order, or it never left: validation /
   signing) → compensating journal `disb_rev_<clave>` (reverse entries) +
   outbox `disbursement.failed` in a new transaction, `502`. If the
   compensation itself fails it is logged for manual reconciliation.
4. Unknown outcome (`stp.ErrSTPNetworkError`: timeout, reset, 5xx, unreadable
   response) → **no compensation** (STP may have paid; returning the funds
   would allow a double withdrawal): the reservation stays, outbox
   `disbursement.dispatch_unknown`, the payout is tracked for CEP
   reconciliation, `202 PENDING_RECONCILIATION`.
5. A crash between the debit commit and the STP call leaves the funds reserved
   with only `disbursement.requested` in the outbox; there is no dispatcher
   that re-sends or releases it yet (manual reconciliation).

## Not covered yet

- Real processor/STP confirmations of payouts and refunds (webhooks), daily
  reconciliation, STP returns.
- RBF loans persisted in the database (and reversed on refunds).
- Real settlement-account ownership verification (STP/bank titularidad or KYB
  provider) and 2FA on account changes.
