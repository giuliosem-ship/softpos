-- 018_credit_notes_and_global_parts.down.sql
DROP TABLE IF EXISTS credit_note_jobs;
DROP INDEX IF EXISTS idx_invoices_related;
DELETE FROM invoice_sales WHERE invoice_id IN (SELECT id FROM invoices WHERE kind = 'CREDIT_NOTE');
DELETE FROM invoices WHERE kind = 'CREDIT_NOTE';
ALTER TABLE invoices DROP COLUMN IF EXISTS related_invoice_id;
ALTER TABLE invoices DROP CONSTRAINT IF EXISTS invoices_kind_check;
ALTER TABLE invoices ADD CONSTRAINT invoices_kind_check CHECK (kind IN ('INDIVIDUAL', 'GLOBAL'));
