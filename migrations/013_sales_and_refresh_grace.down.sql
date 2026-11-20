-- 013_sales_and_refresh_grace.down.sql
ALTER TABLE refresh_tokens DROP COLUMN IF EXISTS grace_used_at;
ALTER TABLE refresh_tokens DROP COLUMN IF EXISTS grace_sealed;
DROP INDEX IF EXISTS idx_card_payments_created;
DROP INDEX IF EXISTS idx_card_payments_merchant_page;
DROP TABLE IF EXISTS merchant_sales;
