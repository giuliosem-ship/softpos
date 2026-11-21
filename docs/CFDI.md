# CFDI 4.0 (facturación)

Kerno issues CFDI 4.0 for its merchants (RESICO persona física at MVP):
individual invoices requested for one or more sales, and the periodic
**factura global** for sales to the general public. Stamping goes through
the `pac.PACClient` port (only a mock exists until a PAC is contracted).
Fiscal rules here still need validation by an accountant (see the gap
analysis, F8/F12).

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
