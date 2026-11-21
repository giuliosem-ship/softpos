-- 020_sat69b_lifecycle_refund_sweep.down.sql
DROP INDEX IF EXISTS idx_card_payment_refunds_requested;
ALTER TABLE card_payment_refunds DROP COLUMN IF EXISTS last_swept_at;
ALTER TABLE card_payment_refunds DROP COLUMN IF EXISTS sweep_attempts;
DELETE FROM sat_blacklist_loads WHERE status = 'REJECTED_CHANGES';
ALTER TABLE sat_blacklist_loads DROP CONSTRAINT IF EXISTS sat_blacklist_loads_status_check;
ALTER TABLE sat_blacklist_loads ADD CONSTRAINT sat_blacklist_loads_status_check
    CHECK (status IN ('LOADED', 'UNCHANGED', 'REJECTED_SHRINK', 'FAILED'));
ALTER TABLE sat_blacklist_loads DROP COLUMN IF EXISTS override_changes;
DROP TABLE IF EXISTS sat69b_exceptions;
ALTER TABLE merchant_kyb DROP CONSTRAINT IF EXISTS merchant_kyb_blocked_source_check;
ALTER TABLE merchant_kyb DROP COLUMN IF EXISTS blocked_source;
