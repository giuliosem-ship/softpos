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

- `AUTHORIZED` → `VOIDED` (nothing was posted); outbox `payment.voided`.
- `SETTLED` → reversal journal and `REFUNDED`, in one transaction; a posting
  failure answers 500 and the payment stays `SETTLED`.

## Not covered yet

- Real processor/STP confirmations of payouts and refunds (webhooks), daily
  reconciliation, STP returns.
- RBF loans persisted in the database (and reversed on refunds).
