-- 019_sale_receipts.up.sql (MVP-26)
-- 1. Card details for the sale receipt, as reported by the processor's
--    signed webhooks: brand, last 4 digits (never the full PAN) and the
--    authorization code.
-- 2. sale_receipts: one public receipt per sale. The URL token is
--    base64url(id || expiry || HMAC(RECEIPT_TOKEN_SECRET)); the row maps the
--    random id to the sale (no tenant data in the token). invoice_id is the
--    invoice the end customer requested through the receipt (autofactura).
-- 3. merchant_fiscal_settings.autofactura: SAME_MONTH (default, the sale's
--    calendar month in America/Mexico_City) or DISABLED. See docs/CFDI.md.

ALTER TABLE card_payments ADD COLUMN IF NOT EXISTS card_brand VARCHAR(20) NOT NULL DEFAULT '';
ALTER TABLE card_payments ADD COLUMN IF NOT EXISTS card_last4 VARCHAR(4) NOT NULL DEFAULT ''
    CONSTRAINT card_payments_card_last4_check CHECK (card_last4 = '' OR card_last4 ~ '^[0-9]{4}$');
ALTER TABLE card_payments ADD COLUMN IF NOT EXISTS auth_code VARCHAR(12) NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS sale_receipts (
    id VARCHAR(32) PRIMARY KEY,
    merchant_id VARCHAR(128) NOT NULL,
    sale_id VARCHAR(160) NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    invoice_id VARCHAR(64) NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT sale_receipts_one_per_sale UNIQUE (merchant_id, sale_id)
);

ALTER TABLE merchant_fiscal_settings ADD COLUMN IF NOT EXISTS autofactura VARCHAR(16) NOT NULL DEFAULT 'SAME_MONTH'
    CONSTRAINT merchant_fiscal_settings_autofactura_check CHECK (autofactura IN ('SAME_MONTH', 'DISABLED'));
