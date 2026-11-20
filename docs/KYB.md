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
   → `CLEAR`. An RFC is only `CLEAR` against a **loaded** list: with no 69-B
   record loaded (in memory or in `sat_blacklist`), a failed repository
   lookup or an unrecognized status it stays `UNKNOWN` and the operator
   checks the RFC by hand (the ingestion worker is not wired yet, see the
   gap analysis).
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
