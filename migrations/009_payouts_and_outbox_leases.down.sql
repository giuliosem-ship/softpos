-- 009_payouts_and_outbox_leases.down.sql
ALTER TABLE outbox_events DROP COLUMN IF EXISTS lease_owner;
DROP INDEX IF EXISTS idx_payouts_recovery;
DROP TABLE IF EXISTS payouts;
