# Merchant onboarding (KYB / KYC)

A merchant is created by its owner (`POST /api/v1/merchants`, see
`docs/AUTH.md`) and starts with a **DRAFT** onboarding. Until an operator
approves it, the merchant **cannot move money**: card intents and refunds,
CoDi / SPEI QR codes, payouts, RBF loan origination and the legacy EMV
endpoints answer `403
MERCHANT_NOT_APPROVED` (a blocked merchant answers `403 MERCHANT_BLOCKED`,
see `docs/BACKOFFICE.md`). Inbound money that still arrives (STP abonos,
processor webhooks) is recorded; it is never rejected.

Code: `pkg/kyb` (rules, checks, `KYCProvider` port + mock), `pkg/store/kyb.go`
(`merchant_kyb`, `kyb_documents`, `kyb_document_blobs`, migration 012),
`pkg/identity/document_vault.go` (document encryption),
`pkg/identity/rfc_check_digit.go`, `pkg/api/handlers_kyb.go` (HTTP, approval).

## States

```
DRAFT ──submit (checks pass)──► SUBMITTED ──69-B clear──► IN_REVIEW ──operator──► APPROVED
  ▲            │ checks fail: 422, stays DRAFT + reason       │ 69-B listed              └─► REJECTED (reason)
  │            ▼                                              ▼                               │
  └──── owner edits data or uploads a document ◄──────── REJECTED ◄──────────────────────────┘
```

| Status | Meaning | Owner may edit |
|---|---|---|
| `DRAFT` | capturing data and documents; `reason` holds the last failed check | yes |
| `SUBMITTED` | checks passed, automatic verification running (transient) | no |
| `IN_REVIEW` | waiting for an operator (backoffice) | no |
| `APPROVED` | merchant active; virtual CLABE assigned, sub-merchant at the processor | no |
| `REJECTED` | `reason` from the operator or `RFC_LISTED_69B`; editing returns it to `DRAFT` | yes |

Every update is compare-and-set on `merchant_kyb.version` (concurrent
updates → `409 KYB_CONCURRENT_UPDATE`). Merchants that predate migration 012
get a `DRAFT` row on first access. Merchants provisioned by trusted server
code (`Server.RegisterMerchant`: demo seeding, tests) get an `APPROVED` row
with `decided_by = system:provisioning`; the public alta never does.

## Endpoints (tenant from the token, path id must match → else `403`)

| Route | Role | Notes |
|---|---|---|
| `GET /api/v1/merchants/{id}/kyb` | any member | view below; `application` only for the owner |
| `PUT /api/v1/merchants/{id}/kyb` | owner | stores the application (DRAFT / REJECTED → DRAFT); `400 INVALID_KYB_DATA` + `fields`; `409 KYB_NOT_EDITABLE` |
| `POST /api/v1/merchants/{id}/kyb/documents` | owner | multipart `type`, `file` → `201 {"document_id","type","status":"RECEIVED"}` |
| `POST /api/v1/merchants/{id}/kyb/submit` | owner | runs the checks; `200` with the resulting status, `422 KYB_CHECKS_FAILED`, `409 KYB_ALREADY_SUBMITTED` |

PUT body:

```json
{"legal_name":"Xochilt Casas Chavez","rfc":"CACX7605101P8","curp":"CACX760510MDFSHC09",
 "tax_regime":"RESICO_PF","mcc":"5812",
 "address":{"street":"Av. Reforma","ext_number":"10","int_number":"2","neighborhood":"Centro",
            "city":"Ciudad de Mexico","state":"CDMX","postal_code":"06000"},
 "contact_email":"dueno@example.com","contact_phone":"+525512345678"}
```

Shape checks at PUT: `legal_name` ≤ 255, `rfc` 12–13 chars of the SAT
alphabet, `curp` (optional) valid format, `tax_regime` ∈ `RESICO_PF`,
`RESICO_PM`, `ACTIVIDAD_EMPRESARIAL_PF`, `PLATAFORMAS_TECNOLOGICAS_PF`,
`GENERAL_PM`, `OTHER`, `mcc` 4 digits, address fields present (`int_number`
optional), `postal_code` 5 digits, email address, E.164 phone.

View (GET / PUT / submit):

```json
{"status":"IN_REVIEW","reason":"…","checks":{"rfc_valid":true,"list_69b":"CLEAR","documents_complete":true},
 "required_documents":["INE_FRONT","INE_BACK","SELFIE","PROOF_OF_ADDRESS","BANK_STATEMENT","CSF"],
 "uploaded_documents":["INE_FRONT","…"],"submitted_at":"2026-11-20T10:00:00Z","decided_at":"…",
 "application":{…},"error":"KYB_CHECKS_FAILED"}
```

`uploaded_documents`, `application` (owner only) and `error` (422 only) are
additions to the shared contract. While editable, `rfc_valid` and
`documents_complete` are computed live; `list_69b` is `UNKNOWN` until a
submission looks it up.

## Documents

| Type | Content |
|---|---|
| `INE_FRONT`, `INE_BACK` | voter ID (both sides) |
| `SELFIE` | liveness selfie |
| `PROOF_OF_ADDRESS` | comprobante de domicilio |
| `BANK_STATEMENT` | estado de cuenta (supports the settlement CLABE) |
| `CSF` | Constancia de Situación Fiscal |

- Limits: ≤ 5 MiB per file (`413 DOCUMENT_TOO_LARGE`); the content type is
  **sniffed** (`http.DetectContentType`), not taken from the client: JPEG,
  PNG or PDF only (`415 UNSUPPORTED_DOCUMENT_FORMAT`); unknown `type` → `400
  INVALID_DOCUMENT_TYPE`; uploads only while editable (`409`).
- Encryption: `identity.DocumentVault` seals each file with a fresh
  AES-256-GCM DEK (AAD = `<merchant>/<document id>`, so a blob cannot be
  swapped between documents) and wraps the DEK with the vault
  `KeyProvider` (`VAULT_MASTER_KEY_HEX` / KMS, same rotation as the CSD
  vault). The plaintext buffer is zeroed after sealing.
- Storage: metadata in `kyb_documents` (type, sniffed content type, size,
  SHA-256 of the plaintext, uploader, status `RECEIVED` / `SUPERSEDED` — a
  new upload of a type supersedes the previous one); envelopes behind
  `store.DocumentBlobStore` (`kyb_document_blobs` bytea today; an S3
  implementation can replace it without touching the handlers).
- Operators download documents through the backoffice (audit-logged).

## Submission checks (`POST …/kyb/submit`)

Deterministic checks first (`kyb.Precheck`); a failure answers `422
KYB_CHECKS_FAILED`, keeps `DRAFT` and stores `reason`:

| Order | Check | Reason |
|---|---|---|
| 1 | application complete and well formed | `APPLICATION_INCOMPLETE` |
| 2 | RFC format + SAT verification digit (`identity.ValidateRFCWithCheckDigit`; generic RFCs rejected) | `RFC_INVALID` |
| 3 | persona física — **derived from the RFC length** (12 = persona moral), never from a client flag | `PERSONA_MORAL_NOT_SUPPORTED` |
| 4 | regime `RESICO_PF` | `UNSUPPORTED_TAX_REGIME` |
| 5 | CURP (if sent) starts with the RFC's first 10 characters | `CURP_RFC_MISMATCH` |
| 6 | every required document uploaded | `DOCUMENTS_INCOMPLETE` |

Then the status becomes `SUBMITTED` (`submitted_at`) and:

1. **SAT 69-B** lookup (`identity.BlacklistService.Lookup`): `PRESUNTO` /
   `DEFINITIVO` → `LISTED` → automatic `REJECTED` (`RFC_LISTED_69B`,
   `decided_by = system:69b`); `CLEAN` / `DESVIRTUADO` / `SENTENCIA_FAVORABLE`
   → `CLEAR`. An RFC is only `CLEAR` against a **loaded, current** list: with
   no 69-B list loaded, a **stale** list (not re-verified for
   `SAT_69B_MAX_AGE`), a failed repository lookup or an unrecognized status
   it stays `UNKNOWN` and the operator checks the RFC by hand. The list is
   ingested as described in "SAT 69-B list ingestion" below.
2. **Identity verification** through `kyb.KYCProvider` (liveness + INE +
   face match) over the latest `INE_FRONT`, `INE_BACK` and `SELFIE`
   (decrypted in memory only, 20 s timeout). The result (`PASSED`,
   `FAILED`, `REVIEW`, `UNKNOWN` when no provider / provider unavailable)
   and the provider reference are stored for the operator; it never
   approves or rejects on its own.
3. `IN_REVIEW`.

**Non-RESICO merchants:** the MVP supports only RESICO persona física (the
tax set-aside of `docs/MONEY_FLOWS.md` is RESICO PF). Other regimes and
personas morales are **rejected explicitly at submission** (reasons 3–4)
instead of running a "no set-aside" mode; supporting them is a later product
decision (F7).

`KYC_PROVIDER`: `mock` (`kyb.MockKYCProvider`: passes unless the selfie
content starts with `FAIL` / `REVIEW`, `TIMEOUT` = unavailable; DEV_MODE
default, rejected by the production guard) or empty (identity check
`UNKNOWN`, operators verify the documents manually). A real provider
(Truora, Metamap, …) implements `kyb.KYCProvider`.

## SAT 69-B list ingestion (MVP-20)

The SAT publishes the complete Article 69-B list as one CSV
(`http://omawww.sat.gob.mx/cifras_sat/Documents/Listado_Completo_69-B.csv`,
~14k rows, Windows-1252/Latin-1, two banner rows — "Información actualizada
al …" — then the header `No,RFC,Nombre del Contribuyente,Situación del
contribuyente,…`).

- **Parser** (`identity.Parse69BCSV`): UTF-8 (with or without BOM) is kept,
  anything else is decoded as Windows-1252; the header row is found by its
  `RFC` and `Situación…` columns (banner rows ignored); data rows with a bad
  RFC (e.g. the SAT's redacted `XXXXXXXXXXXX` rows), an unknown situation or
  too few columns are **counted and logged** as malformed, never fatal; an
  RFC listed twice keeps its most severe situation. A file without header or
  without any valid row is refused (`FAILED`). Validated against the real
  file of 2026-11-20 (published "31 de diciembre de 2025"): 14,054 RFCs
  loaded, 91 malformed (redacted) rows, 89 duplicates, ~0.1 s into Postgres.
- **Mapping** (`kyb.Classify69B`): `Presunto` / `Definitivo` → `LISTED`;
  `Desvirtuado` / `Sentencia Favorable` / not in a current list → `CLEAR`.
- **Atomic swap** (`identity.List69BStore.ReplaceList`, migration 015): in
  one transaction (serialized by a lock on `sat_blacklist_meta`) the rows go
  to `sat_blacklist_staging`, `sat_blacklist` is replaced from it and
  `sat_blacklist_meta` records `list_version` (SHA-256 of the file),
  `source`, `published`, `row_count`, `malformed_count`, `fetched_at`,
  `verified_at`. Lookups (MVCC) see either the old or the new list, never a
  half one. Every attempt is a `sat_blacklist_loads` row (`LOADED`,
  `UNCHANGED`, `REJECTED_SHRINK`, `REJECTED_CHANGES`, `FAILED`).
- **Shrink protection:** a list with more than `SAT_69B_MAX_SHRINK_PCT`
  (default 10 %) fewer rows than the loaded one is refused
  (`REJECTED_SHRINK`, ERROR log) and the previous list stays, until an
  operator overrides it (backoffice with a reason, or
  `operatorctl load-69b --override-shrink`).
- **Change protection (MVP-27, migration 020):** the row count alone does not
  catch a wrong file of the same size (column shift, another list). Inside
  the swap transaction the new rows are compared with the loaded list: if
  more than `SAT_69B_MAX_CHANGE_PCT` (default 10 %) of the RFCs of the
  loaded list changed situation **or were replaced** by other RFCs (removed
  beyond the net shrink, `identity.GuardedChanges`), the load is refused
  (`REJECTED_CHANGES`, `ALERT` ERROR log, `409 LIST_CHANGED` in the
  backoffice) and the previous list stays, until an operator overrides it
  (`override_changes` + reason in the backoffice, or
  `operatorctl load-69b --override-changes`; recorded in
  `sat_blacklist_loads.override_changes`). Removed RFCs count because the retroscreen lifts the
  69-B block of an RFC absent from a current list: a same-size file with
  other RFCs would pass the shrink protection and unblock every listed
  merchant (audit of Lote L); the net shrink stays the shrink protection's.
  Additions only block more merchants (fail-closed) and are not changes. A real SAT update changes the
  situation of a few hundred of ~14k RFCs (< 5 %). `operatorctl` uses the
  default thresholds.
- **Staleness:** the same file again only moves `verified_at`. A list not
  verified for `SAT_69B_MAX_AGE` (default 7 days) makes every lookup
  `UNKNOWN` (`identity.ErrBlacklistStale`).
- **Retroscreen:** after each load (or unchanged verification) every
  `APPROVED` merchant is re-checked (`store.Retroscreen69B`); see the block
  lifecycle below. `list_69b` is refreshed for every merchant whose lookup
  is conclusive.
- **Ways to load:** worker in `cmd/server` (`SAT_69B_URL`, at boot and every
  `SAT_69B_REFRESH_INTERVAL`, default 24 h; no URL = no worker); backoffice
  `POST /api/v1/backoffice/sat69b/sync` and `GET /api/v1/backoffice/sat69b`
  (`docs/BACKOFFICE.md`); `DATABASE_URL=… go run ./cmd/operatorctl load-69b
  <file.csv> [--override-shrink] [--override-changes]` (audited as
  `sat69b.load_file`). Without `DATABASE_URL` the list is kept in memory
  (`identity.MemoryList69BStore`).

### 69-B block lifecycle (MVP-27)

`merchant_kyb.blocked_source` (migration 020) records who placed the current
block: `OPERATOR` (backoffice `POST …/block`) or `SAT_69B` (retroscreen).
Existing blocks were backfilled (`blocked_reason = RFC_LISTED_69B` →
`SAT_69B`, any other → `OPERATOR`).

| Retroscreen sees | Merchant state | Result |
|---|---|---|
| `PRESUNTO` / `DEFINITIVO` | not blocked, no exception for this RFC + situation | **block** (`SAT_69B`, `blocked_reason = RFC_LISTED_69B`, money routes `403 MERCHANT_BLOCKED`), outbox event `merchant.blocked_69b` **in the same transaction** as the update, audit `kyb.retroscreen_69b_block` by `system:69b`, `ALERT` ERROR log; older exceptions of the merchant are revoked |
| `PRESUNTO` / `DEFINITIVO` | not blocked, active exception for this RFC + situation | nothing (`excepted` in the result) |
| `DESVIRTUADO` / `SENTENCIA FAVORABLE` / absent from a current list | blocked by `SAT_69B` | **auto-unblock**, outbox `merchant.unblocked_69b` in the same transaction, audit `kyb.retroscreen_69b_unblock`; exceptions revoked |
| same | blocked by `OPERATOR` | stays blocked: a manual block is never lifted by a load (an operator block on a `SAT_69B`-blocked merchant takes the block over and makes it `OPERATOR`) |
| lookup not conclusive (no list, stale list, DB error) | any | nothing (fail closed: blocks stay) |

- **Notification:** the outbox events (`payload`: `merchant_id`, `situation`,
  `blocked_reason` / dates) are published by the outbox dispatcher (log
  publisher today); a real notification channel (SMS/e-mail/push) plugs in
  as an `outbox.Publisher` (see the pending list).
- **Documented exception:** an operator unblock of a merchant whose RFC is
  still `PRESUNTO`/`DEFINITIVO` requires a `reason` (`400 REASON_REQUIRED`)
  and records a `sat69b_exceptions` row (merchant, RFC, situation, reason,
  operator) plus the audit rows `merchant.unblock` and `sat69b.exception`.
  The next loads respect it while the situation stays the same; a situation
  change (e.g. `PRESUNTO → DEFINITIVO`) re-blocks and revokes it, and so does
  the RFC leaving the list. If the list cannot be consulted (stale /
  unavailable) a merchant recorded as `LISTED` is not unblocked
  (`409 SAT69B_STATUS_UNKNOWN`).
- The process itself (who reviews, how long an exception may last, what the
  merchant is told) must still be agreed with compliance.

## Operator decision (backoffice)

`Server.ApproveMerchantKYB` (from `IN_REVIEW` or a stuck `SUBMITTED`; never
when `list_69b` is `LISTED`, `ErrKYBListed69B`):

1. assigns the virtual collection CLABE if the merchant has none
   (`AssignVirtualCLABE`; the alta no longer assigns it);
2. onboards the merchant at the processor: `acquirer.Provider.OnboardMerchant`
   with key `kerno_onboard_<merchant_id>` (idempotent; an error aborts the
   approval and leaves `IN_REVIEW` for a retry); the sub-merchant id is
   stored in `acquirer_merchant_id`;
3. copies the verified `legal_name` / `rfc` into the merchant profile;
4. `APPROVED` with `decided_at` / `decided_by` (operator user id).

`Server.RejectMerchantKYB` stores `REJECTED` with the operator's reason. The
HTTP endpoints, audit log and blocking are in `docs/BACKOFFICE.md`.

## Tax split input (`is_persona_moral`)

The RESICO split's persona-moral flag means "the **buyer** is a persona moral
that withholds 1.25 % ISR" (Art. 113-J LISR). It is no longer read from any
request body (card intents, legacy settle; CoDi never had it): the server
decides it (`Server.customerPersonaMoral`). Since only RESICO PF merchants are
approved and there is no validated CFDI receptor yet (F8), every sale is
treated as a sale to the general public (flag `false`). A client
`is_persona_moral` is ignored.

## Pending (see `docs/KERNO_MVP_GAP_ANALYSIS.md`)

Real KYC provider and its contract; real processor onboarding API; STP
registration of the virtual CLABE range; PLD risk scoring; document
retention / deletion policy (LFPDPPP); S3 blob store.
