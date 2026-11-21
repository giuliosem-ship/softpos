-- 018_credit_notes_and_global_parts.down.sql
-- Going back to one live global per period fails if a period already has
-- several live parts (cancel the extra parts first).
DROP INDEX IF EXISTS idx_invoices_one_live_global_part;
CREATE UNIQUE INDEX IF NOT EXISTS idx_invoices_one_live_global ON invoices(merchant_id, global_period)
    WHERE kind = 'GLOBAL' AND status IN ('PENDING', 'STAMPED', 'CANCEL_REQUESTED');
ALTER TABLE invoices DROP COLUMN IF EXISTS global_part;
DROP TABLE IF EXISTS credit_note_jobs;
DROP INDEX IF EXISTS idx_invoices_related;
DELETE FROM invoice_sales WHERE invoice_id IN (SELECT id FROM invoices WHERE kind = 'CREDIT_NOTE');
DELETE FROM invoices WHERE kind = 'CREDIT_NOTE';
ALTER TABLE invoices DROP COLUMN IF EXISTS related_invoice_id;
ALTER TABLE invoices DROP CONSTRAINT IF EXISTS invoices_kind_check;
ALTER TABLE invoices ADD CONSTRAINT invoices_kind_check CHECK (kind IN ('INDIVIDUAL', 'GLOBAL'));
