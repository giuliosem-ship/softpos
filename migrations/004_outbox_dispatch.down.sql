-- 004_outbox_dispatch.down.sql
DROP INDEX IF EXISTS idx_outbox_events_due;
ALTER TABLE outbox_events DROP COLUMN IF EXISTS next_attempt_at;
