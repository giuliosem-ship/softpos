-- 011_reconciliation_resolved_by.down.sql
ALTER TABLE acquirer_reconciliation_items DROP COLUMN IF EXISTS resolved_by;
