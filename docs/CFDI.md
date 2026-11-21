# CFDI 4.0 (facturación)

Kerno issues CFDI 4.0 for its merchants (RESICO persona física at MVP):
individual invoices requested for one or more sales, and the periodic
**factura global** for sales to the general public. Stamping goes through
the `pac.PACClient` port (only a mock exists until a PAC is contracted).
Refunds and chargebacks after invoicing produce notas de crédito (CFDI de
egreso, MVP-23). Fiscal rules here still need validation by an accountant
(see the gap analysis, F7/F8/F12).

## Cadena original and sello

- `fiscal.BuildCFDI40XML` serializes `fiscal.CFDIComprobante` (XSD element
  and attribute order; optional attributes omitted when empty). Strings with
  characters not allowed in XML 1.0 are refused (Go would replace them and
  the sello would stop matching).
- `fiscal.BuildCadenaOriginal` is a Go port of the SAT XSLT
  `cadenaoriginal_4_0.xslt` + `utilerias.xslt` (Go has no XSLT engine):
  required attributes always emit `|value` (an empty one emits `|`),
  optional ones only when present, every value through XPath
  `normalize-space` (only space/tab/CR/LF collapse; e.g. U+00A0 is kept).
  Covered nodes: comprobante, InformacionGlobal, CfdiRelacionados, Emisor,
  Receptor, Conceptos with their Traslados/Retenciones (Exento without
  TasaOCuota/Importe), comprobante Impuestos. Complements are not supported
  (Kerno emits none; the TimbreFiscalDigital has its own cadena).
- **Golden tests** (`pkg/fiscal/cadena_xslt_test.go`): the official XSLT,
  vendored byte-identical in `pkg/fiscal/testdata/sat/` (source URLs,
  download date and SHA-256 in its `README.md`), runs through `xsltproc
  --nonet` over 12 variants (receptor PM/PF, factura global with many
  conceptos, IVA 16 % / 0 % / exento, ISR and IVA retenciones, special
  characters `& < > " '` and whitespace collapsing, ObjetoImp 01,
  CfdiRelacionados, empty required / blank optional attributes, foreign
  currency) and must match the Go cadena byte for byte; a second test seals
  each variant and verifies the sello over the cadena **the XSLT computes**.
  The tests skip with a message when `xsltproc` is missing; CI installs it.
  Bugs fixed against the XSLT: inner whitespace was not collapsed, empty
  required attributes were dropped, concept-level taxes / Exento /
  CfdiRelacionados were missing, and `SignCFDI` signed a hash of the XML
  instead of the cadena original.
- Sello: RSA PKCS#1 v1.5 over SHA-256 of the UTF-8 cadena, base64
  (`fiscal.CSDSigner.Seal`); `NoCertificado` is the SAT 20-digit number
  (the certificate serial's ASCII digits, `fiscal.CertificateNumber`).

## Merchant CSD (Certificado de Sello Digital)

`PUT /api/v1/merchants/{id}/csd` (owner, `multipart/form-data`: `cer` file,
`key` file, `password` field; each part ≤ 64 KB) → `200
{"serial","rfc","valid_from","valid_to","status":"ACTIVE"}`.
`GET …/csd` (any member) → same shape (`status` `EXPIRED` once past
`valid_to`) or `404 CSD_NOT_FOUND`.

Validation (`fiscal.ValidateCSD`) → `422 {"error":"CSD_INVALID","reason"}`:

| reason | rule |
|---|---|
| `CER_UNREADABLE` | `.cer` is not an X.509 certificate (DER or PEM) |
| `KEY_UNREADABLE` / `KEY_ENCRYPTION_UNSUPPORTED` | `.key` is not a PKCS#8 EncryptedPrivateKeyInfo with PBES2/PBKDF2 (HMAC-SHA1/SHA-256) and DES-EDE3-CBC or AES-CBC |
| `WRONG_PASSWORD` | decryption padding / PKCS#8 parse fails |
| `KEY_CERT_MISMATCH` | the key's public key is not the certificate's |
| `NOT_RSA` | non-RSA key |
| `FIEL_NOT_CSD` | e.firma: key usage keyAgreement/dataEncipherment/keyEncipherment or EKU emailProtection/clientAuth (a CSD only signs) |
| `RFC_MISMATCH` | first token of the subject `x500UniqueIdentifier` (2.5.4.45) ≠ the merchant's KYB RFC |
| `EXPIRED` / `NOT_YET_VALID` | outside `NotBefore..NotAfter` |
| `MISSING_CER` / `MISSING_KEY` / `MISSING_PASSWORD` | part absent |

The password is read from the multipart stream as bytes (never a Go
string), the `.cer`, the **original encrypted `.key`** and the password are
sealed in the vault (`identity.CSDVaultService.StoreCSDBytes`, per-CSD DEK
wrapped by the KMS/env key provider) into `merchants.encrypted_csd_json`,
and the buffers are zeroed. At signing time the envelope is opened, the key
decrypted in memory, and `DecryptedCSD.Wipe` + `clear` zero it afterwards
(the PBKDF2 stdlib API still takes the password as a string internally).
Metadata (`GET`) is read back from the stored certificate. Not done yet:
verifying the chain against the SAT root CAs and the SAT revocation list
(LCO); the PAC rejects revoked CSDs. The DEV_MODE demo merchant keeps a
self-signed, SAT-shaped CSD (`fiscaltest.NewCSD`, plaintext key, password
marker `identity.CSDPlaintextKeyMarker`).

## SAT catalogs

A versioned subset of the CFDI 4.0 catalogs is embedded in the binary
(`pkg/fiscal/catalogs/sat_catalogs.json`, `version` e.g. `2026.11-1`):
`c_RegimenFiscal` (persona física / moral), `c_UsoCFDI` (persona type and
allowed receptor regimes), the `c_ClaveProdServ` / `c_ClaveUnidad` codes
merchants may pick, `c_MotivoCancelacion` and `c_Periodicidad`. Change the
JSON and bump `version` to update.

`GET /api/v1/catalogs/sat?names=regimen_fiscal,uso_cfdi` (any authenticated
user; no `names` = all; `400 UNKNOWN_CATALOG`) →
`{"regimen_fiscal":[{"code","description","persona_fisica","persona_moral"}],
"uso_cfdi":[{"code","description","persona_fisica","persona_moral","regimenes":[…]}], …}`
with header `X-SAT-Catalog-Version`.

Receptor validation (`fiscal.ValidateReceptor`, used by the invoice API):
RFC format + check digit (12 = persona moral, 13 = persona física; the
generic `XAXX010101000` / `XEXX010101000` are only for the factura global),
régimen in the catalog and allowed for the persona type, uso CFDI in the
catalog, allowed for the persona type and compatible with the régimen,
5-digit CP (existence is checked by the PAC), name 1–254 characters (not
`PUBLICO EN GENERAL`), optional email.

## Merchant fiscal settings

`GET /api/v1/merchants/{id}/fiscal-settings` (any member) / `PUT` (owner)
`{"clave_prod_serv","clave_unidad","descripcion","lugar_expedicion","global_periodicity":"MONTHLY"|"DAILY"}`
(`merchant_fiscal_settings`, migration 016). Defaults: `01010101`, `H87`,
`Venta`, the KYB address postal code, `MONTHLY`. Codes must be in the
embedded subset (`422 FISCAL_SETTINGS_INVALID` with `fields`). The emisor
of every CFDI is the KYB `legal_name` (upper case), RFC and régimen
(`RESICO_PF` → 626, `ACTIVIDAD_EMPRESARIAL_PF` → 612,
`PLATAFORMAS_TECNOLOGICAS_PF` → 625, `GENERAL_PM` → 601) and the
`lugar_expedicion`.

## Invoices from sales (MVP-22)

`POST /api/v1/merchants/{id}/invoices` (owner or cashier; approved, not
blocked merchant; header `Idempotency-Key` ≤ 200 chars)

```json
{"sale_ids":["<card payment id | spei_… | codi_…>"],
 "receptor":{"rfc","nombre","regimen_fiscal","codigo_postal","uso_cfdi","email"?},
 "replaces_invoice_id":"inv_…"?}
```

→ `201` invoice:

```json
{"invoice_id","kind":"INDIVIDUAL"|"GLOBAL","status":"STAMPED"|"PENDING"|"FAILED"|"CANCEL_REQUESTED"|"CANCELED",
 "uuid"?,"serie"?,"folio"?,"total_cents","subtotal_cents","iva_cents","isr_retenido_cents",
 "receptor":{…},"sale_ids":[…],"created_at","stamped_at"?,"error"?,
 "global_period"?,"replaces_invoice_id"?,"cancellation"?:{"motivo","folio_sustitucion"?,"status","requested_at","resolved_at"?}}
```

- **Amounts come from what was posted, never from the client:** a card sale
  (`CAPTURED` / `PARTIALLY_REFUNDED`) uses its settlement record
  (`payment_settlements` base and IVA minus refunds; when that net IVA falls
  outside the SAT limits for its base, possible after a partial refund, the
  net gross is re-split with `fiscal.IVAFromGross`: same total, base at most
  one cent from the ledger); SPEI abonos
  (`RECEIVED`) and CoDi confirmations (`CONFIRMED`) use the 16 %
  IVA-inclusive split of the RESICO posting (`fiscal.IVAFromGross`, same
  formula as `CalculateResicoTaxSplit`). One concepto per sale
  (`NoIdentificacion` = sale id, the merchant's ClaveProdServ / ClaveUnidad /
  descripción), IVA 16 % trasladado per concepto and in total, int64 cents.
  `FormaPago` is the one of the largest amount (04 card, 03 transfer),
  `MetodoPago` PUE, `Fecha` in America/Mexico_City, serie `K` (individual) /
  `G` (global) with a per-merchant folio sequence.
- **ISR retention:** a persona moral receptor (12-char RFC) of a RESICO
  persona física emisor gets the Art. 113-J retention (1.25 % of the base,
  `isr_retenido_cents`; `total = subtotal + iva − isr`). The ledger split of
  the original sale is **not** re-posted (pending, accountant review).
- **Errors:** `400 MISSING_IDEMPOTENCY_KEY` / `INVALID_SALE_IDS` (1–100
  unique ids), `422 RECEPTOR_INVALID {"fields":{"<field>":"<REASON>"}}`
  (reasons in "SAT catalogs" above), `422 SALE_NOT_FOUND` /
  `SALE_NOT_INVOICEABLE {"sale_id"}`, `409 SALE_ALREADY_INVOICED
  {"sale_id","invoice_id"}` (a sale is in at most one active invoice, unique
  partial index `invoice_sales(merchant_id, sale_id) WHERE active`), `409
  IDEMPOTENCY_KEY_REUSED`, `422 FISCAL_PROFILE_INCOMPLETE {"missing"}`, `422
  CSD_NOT_FOUND` / `CSD_EXPIRED`, `422 REPLACEMENT_INVALID`, `403
  MERCHANT_NOT_APPROVED` / `MERCHANT_BLOCKED`. Same key + same body replays
  the invoice (`201`).
- **Stamping** (`pac.PACClient` via the multi-PAC orchestrator): `STAMPED`
  (TimbreFiscalDigital injected, receptor emailed); a deterministic SAT/PAC
  rejection → `FAILED` with `error` = PAC code and the sales released; PAC
  unavailable → stays `PENDING` (`error: PAC_UNAVAILABLE`) holding its
  sales. A retry resends the **same sealed XML** while its `Fecha` is < 70 h
  old (so a PAC that did stamp it returns the existing timbre) and reseals
  with a new Fecha after that. A stamping lease (`stamp_lease_until`)
  prevents concurrent stamping of one invoice.
- `GET /api/v1/merchants/{id}/invoices?limit=50&before=<cursor>` →
  `{"items":[…],"next_cursor"}` (newest first, same cursor format as sales);
  `GET …/invoices/{invoice_id}`; `GET …/{invoice_id}/xml`
  (`application/xml`) and `GET …/{invoice_id}/pdf` (`application/pdf`); both
  `409 INVOICE_NOT_STAMPED` before stamping. Any member of the merchant.

### Representación impresa (PDF) and email

`pkg/cfdipdf` renders the PDF from the stamped XML in pure Go (no CGO): a
minimal PDF 1.4 writer with the standard Helvetica fonts (WinAnsi), emisor /
receptor, conceptos (paginated), subtotal / IVA / ISR retenido / total,
UUID, certification date, certificate numbers, sello del CFDI, sello del SAT,
cadena original del TFD, and the SAT verification QR
(`https://verificacfdi.facturaelectronica.sat.gob.mx/default.aspx?id=<UUID>&re=<RFC emisor>&rr=<RFC receptor>&tt=<10.6 total>&fe=<last 8 of the sello>`)
drawn as vector modules with `rsc.io/qr` (pure Go). When the receptor has an
`email`, PDF + XML are sent through the `api.Mailer` interface
(`LogMailer` only logs until an email provider is contracted).

### Cancellation

`POST …/invoices/{invoice_id}/cancel` (owner) `{"motivo":"01"|"02"|"03"|"04","folio_sustitucion"?}`:

- Only `STAMPED` invoices (`409 INVOICE_NOT_CANCELABLE`). `422
  CANCEL_INVALID {"fields"}` for an unknown motivo, `01` without
  `folio_sustitucion`, `folio_sustitucion` with another motivo, or `04` on an
  individual invoice.
- **Motivo 01** (errores con relación): first issue the replacement with
  `replaces_invoice_id` (it carries `CfdiRelacionados` TipoRelacion 04 with
  the original UUID and takes over the sales), then cancel the original with
  `folio_sustitucion` = the replacement's UUID (or invoice id), which must be
  `STAMPED` and replace this invoice.
- The invoice is claimed (`STAMPED → CANCEL_REQUESTED`) before calling
  `PACClient.Cancel` with the CSD, so concurrent requests never both reach
  the PAC. PAC answer: canceled at once (SAT: ≤ $1,000, público en general,
  …) → `200` `CANCELED` (sales released); waiting for the receptor →
  `202` `CANCEL_REQUESTED` (`cancellation.status` `REQUESTED`); rejected /
  not cancelable → `422 CANCEL_REJECTED` and the invoice stays `STAMPED`;
  PAC unavailable → `502 PAC_UNAVAILABLE` (claim reverted).
- **Receptor acceptance:** the cancellation poller (`CFDI_CANCEL_POLL_INTERVAL`,
  default 15 m) queries `PACClient.Status`: `Cancelado` (accepted, or the 72 h
  "plazo vencido") → `CANCELED` + sales released; `Solicitud rechazada` →
  back to `STAMPED` with `cancellation.status` `REJECTED` (the merchant may
  request again).

### Factura global automática

Worker (`CFDI_GLOBAL_INVOICE_INTERVAL`, default 1 h; `Server.RunGlobalInvoices`):
for every approved, unblocked merchant with a CSD and a complete fiscal
profile, for the last two **closed** periods (`MONTHLY` default or `DAILY`,
America/Mexico_City boundaries) it issues one CFDI to `XAXX010101000 /
PUBLICO EN GENERAL / 616 / S01` with `DomicilioFiscalReceptor` =
`LugarExpedicion` and `InformacionGlobal` (Periodicidad 04 or 01, Meses,
Año), one concepto per sale (`01010101`, `ACT`, "Venta", `NoIdentificacion` =
sale id) for every invoiceable sale of the period **not in an active
invoice**. A unique partial index allows one live global per merchant and
period; the idempotency key is derived from the period and the sale set, so
reruns never duplicate it; `PENDING` globals are retried by the worker,
`FAILED` ones from the backoffice. A late run (> 72 h after the period end)
still issues it and logs a warning.

**Sales already in a global (documented choice):** an individual invoice for
a sale included in an active factura global is **rejected** (`409
SALE_ALREADY_INVOICED` with the global's `invoice_id`). With the default
monthly periodicity customers can ask for their invoice during the whole
month of the purchase, which matches the SAT rule that a nominative CFDI for
a sale must be issued within the same month; afterwards the merchant can
cancel the global with **motivo 04** (frees its sales; the next worker run
re-issues the global for the sales not invoiced meanwhile) and then invoice
the sale individually. Accountant validation pending.

### Notas de crédito (CFDI de egreso, MVP-23)

A refund (`payment.refunded`) or chargeback (`chargeback.opened`) of a card
sale **after** the sale was invoiced (individually or in a factura global)
produces a CFDI de egreso automatically:

- **Durable job, same transaction as the money:** the webhook transaction
  that posts the ledger reversal also inserts a `credit_note_jobs` row
  (migration 018; unique per merchant, sale, source kind `REFUND` /
  `CHARGEBACK` and provider refund / chargeback id), recording the movement
  amount and the sale's net collected gross right after it (`captured −
  refunded − charged back`). No PAC call happens inside the webhook.
- **Worker** (`Server.ProcessCreditNotes`, `CFDI_CREDIT_NOTE_INTERVAL`,
  default 1 m) takes PENDING jobs oldest first (a sale's jobs strictly in
  order) and looks at the sale's active invoice:
  - none → after a 5 min grace (an invoice being created with amounts read
    just before the movement) `NOT_REQUIRED`: the sale is invoiced later by
    its net amount (refund before invoice);
  - not `STAMPED` yet (`PENDING`, `CANCEL_REQUESTED`) → retried with backoff
    (1 m doubling up to 1 h);
  - `STAMPED` → credit = `min(movement, invoiced line gross − earlier credit
    notes of that line − net after the movement)`; ≤ 0 → `NOT_REQUIRED` (the
    invoice was already net of it). Otherwise a `CREDIT_NOTE` invoice is
    created (idempotency key `cn:<kind>:<sale>:<source id>`: one credit note
    per refund / chargeback, even with concurrent workers) and stamped
    through `pac.PACClient`; the job becomes `ISSUED`.
- **The egreso:** `TipoDeComprobante="E"`, serie `E` (own folio sequence),
  `CfdiRelacionados TipoRelacion="01"` with the credited invoice's UUID,
  `MetodoPago` PUE, `FormaPago` 04, one concepto `84111506` / `ACT`
  ("Devolución de venta" / "Contracargo de venta", `NoIdentificacion` = sale
  id), IVA 16 % trasladado. Amounts in int64 cents: base = credit × line base
  / line gross rounded half-up, IVA = credit − base (re-split with the RESICO
  16 % formula if outside the SAT limits); the last credit of a line takes
  exactly its remaining base and IVA, so the credit notes of a line add up to
  it to the cent. ISR retention 1.25 % (half-up) when the credited invoice
  withheld it. Receptor: the credited invoice's, with `UsoCFDI` G02
  ("Devoluciones, descuentos o bonificaciones") when the catalog allows it
  for the receptor's régimen (otherwise its original uso); for a factura
  global, `XAXX010101000` / `PUBLICO EN GENERAL` / S01 with the global's
  `InformacionGlobal`. Its invoice lines are stored **inactive** (a credit
  note never holds a sale) and the invoice API returns it with
  `"kind":"CREDIT_NOTE"` and `related_invoice_id`. XML / PDF / email as any
  invoice. Golden tests: two egreso variants plus the built ones match the
  official XSLT.
- **Failures:** PAC unavailable → the credit note stays `PENDING` and the
  worker retries it; SAT rejection → `FAILED`, retried from the backoffice
  (`POST /api/v1/backoffice/invoices/{id}/retry`, same as failed stamps).
- **Chargeback reversed (`chargeback.reversed`) — decision:** Kerno does
  **not** re-invoice automatically. A job still PENDING becomes
  `NOT_REQUIRED` (funds came back before any egreso); an `ISSUED` one is
  flagged `CHARGEBACK_REVERSED`, an `ALERT` is logged and operators see it in
  `GET /api/v1/backoffice/credit-notes?flagged=true` (also `status=`,
  `merchant_id=`, `limit=`) to issue a new CFDI de ingreso manually with the
  accountant's guidance. If the reversal races a worker issuing the egreso,
  the job ends `ISSUED` and still flagged.
- **Limitations (accountant to confirm):** egreso vs. cancelación con
  sustitución; clave 84111506 / ACT and InformacionGlobal on the egreso of a
  global; G02; the egreso of a credited invoice later replaced with motivo 01
  stays related to the canceled UUID (the replacement is built from the net
  amount, so it is not credited twice, but the old egreso is not canceled).

### Backoffice

`GET /api/v1/backoffice/invoices?status=FAILED,PENDING&merchant_id=&limit=`
lists stampings to look at (with `stamp_attempts`, `error_detail`);
`POST /api/v1/backoffice/invoices/{invoice}/retry` re-stamps a `PENDING` /
`FAILED` invoice (a `FAILED` one first re-links its sales: `409
SALE_ALREADY_INVOICED` if one was invoiced meanwhile; `409
INVOICE_NOT_RETRYABLE` otherwise), audit action `invoice.retry`.

## PAC adapter contract

`pac.PACClient` (`pkg/pac/client.go`) — what a real PAC adapter
(Finkok, SW Sapien, Diverza, …) must implement, sandbox first:

| Method | Contract |
|---|---|
| `Stamp(ctx, sealedXML)` | Send the sealed CFDI 4.0; return the `TimbreFiscalDigital` 1.1 (UUID, FechaTimbrado, RfcProvCertif, SelloCFD = the comprobante sello, NoCertificadoSAT, SelloSAT). **Idempotent on the sello**: a CFDI already stamped (SAT/PAC error 307) must return the existing timbre, never a second UUID. Map SAT/PAC validation codes (CFDI40xxx, 30x) to `ErrSATValidationError` with `PACResponse.ErrorCode`/`ErrorMessage` (no retry, no failover); timeouts, 5xx and network errors to `ErrPACUnavailable` (retry / failover). |
| `Cancel(ctx, CancelRequest)` | Cancel with the emisor's CSD (certificate + plaintext key, wiped by the caller; or the PAC's stored CSD/PFX), motivo 01–04 and `FolioSustitucion` for 01. Return `CANCELED` (no acceptance needed), `IN_PROCESS` (receptor acceptance pending) or `REJECTED` with `ErrSATValidationError` (not cancelable, wrong RFC, …); transient errors → `ErrPACUnavailable`. |
| `Status(ctx, StatusRequest)` | SAT ConsultaCFDIService (by UUID, emisor/receptor RFC and total): `Estado` (`Vigente`, `Cancelado`, `No Encontrado`), `EsCancelable`, `EstatusCancelacion` (`En proceso`, `Solicitud rechazada`, `Cancelado con/sin aceptación`, `Plazo vencido`). |

Also required before production: contract and credentials per environment
(secrets in the vault / env, added to `ServerEnvVars` and the production
guard), the PAC's own error catalogue mapped to the codes above, CSD
registration with the PAC if it signs cancellations, timeouts below the
orchestrator's (1.5 s per attempt today; stamping is retried from the
backoffice / worker), and the SAT validator on real CFDIs of the sandbox.
The mock (`pac.MockPACClient` + `pac.MockSAT`) simulates the SAT rules used
by the tests (cancellation without acceptance ≤ $1,000 or to the general
public, 72 h plazo vencido).
