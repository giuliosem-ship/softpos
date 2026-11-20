-- 007_money_posting.down.sql
DROP TABLE IF EXISTS money_operations;
DROP TABLE IF EXISTS payment_settlements;
ALTER TABLE merchants DROP COLUMN IF EXISTS platform_fee_bps;
