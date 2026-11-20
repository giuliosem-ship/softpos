-- 004_outbox_dispatch.up.sql
-- Retry scheduling for the outbox dispatcher (exponential backoff).

ALTER TABLE outbox_events ADD COLUMN IF NOT EXISTS next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

CREATE INDEX IF NOT EXISTS idx_outbox_events_due ON outbox_events(status, next_attempt_at);
