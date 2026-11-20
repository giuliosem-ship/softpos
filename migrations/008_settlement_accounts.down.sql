-- 008_settlement_accounts.down.sql
ALTER TABLE merchants DROP COLUMN IF EXISTS settlement_clabe_updated_at;
ALTER TABLE merchants DROP COLUMN IF EXISTS settlement_clabe_status;
ALTER TABLE merchants DROP COLUMN IF EXISTS settlement_clabe;
