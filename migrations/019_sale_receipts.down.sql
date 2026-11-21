-- 019_sale_receipts.down.sql
ALTER TABLE merchant_fiscal_settings DROP COLUMN IF EXISTS autofactura;
DROP TABLE IF EXISTS sale_receipts;
ALTER TABLE card_payments DROP COLUMN IF EXISTS auth_code;
ALTER TABLE card_payments DROP COLUMN IF EXISTS card_last4;
ALTER TABLE card_payments DROP COLUMN IF EXISTS card_brand;
