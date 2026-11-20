-- 011_reconciliation_resolved_by.up.sql (MVP-16 audit): record which operator
-- resolved a processor reconciliation difference (audit trail in the report,
-- not only in the logs).
ALTER TABLE acquirer_reconciliation_items ADD COLUMN IF NOT EXISTS resolved_by VARCHAR(128) NOT NULL DEFAULT '';
