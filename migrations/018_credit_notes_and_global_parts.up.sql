-- 018_credit_notes_and_global_parts.up.sql (MVP-23, MVP-24)
-- 1. Notas de crédito (CFDI de egreso) are rows of invoices with kind
--    CREDIT_NOTE, related to the credited invoice (related_invoice_id). Their
--    invoice_sales lines are stored inactive: a credit note never holds a
--    sale (the one-active-invoice-per-sale index keeps working).
-- 2. Factura global parts (MVP-24): a period with more sales than fit in one
--    CFDI is split into several globals; each live global of a period has its
--    own part number (unique per merchant, period and part).
-- 3. credit_note_jobs: one durable job per refund / chargeback of a card
--    sale, written in the same money transaction as the ledger movement; the
--    credit-note worker decides whether the sale was invoiced and stamps the
--    egreso through the PAC port. See docs/CFDI.md.

ALTER TABLE invoices DROP CONSTRAINT IF EXISTS invoices_kind_check;
ALTER TABLE invoices ADD CONSTRAINT invoices_kind_check CHECK (kind IN ('INDIVIDUAL', 'GLOBAL', 'CREDIT_NOTE'));
ALTER TABLE invoices ADD COLUMN IF NOT EXISTS related_invoice_id VARCHAR(64) NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_invoices_related ON invoices(related_invoice_id) WHERE related_invoice_id <> '';

CREATE TABLE IF NOT EXISTS credit_note_jobs (
    id BIGSERIAL PRIMARY KEY,
    merchant_id VARCHAR(128) NOT NULL,
    sale_id VARCHAR(160) NOT NULL,
    source_kind VARCHAR(16) NOT NULL CHECK (source_kind IN ('REFUND', 'CHARGEBACK')),
    source_id VARCHAR(160) NOT NULL,
    amount_cents BIGINT NOT NULL CHECK (amount_cents > 0),
    -- Net collected gross of the sale right after this movement (captured -
    -- refunded - charged back): the worker credits only what the active
    -- invoice covers above it, so a refund that landed before the invoice
    -- (already netted out of it) produces no egreso.
    net_after_cents BIGINT NOT NULL CHECK (net_after_cents >= 0),
    status VARCHAR(16) NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING', 'ISSUED', 'NOT_REQUIRED')),
    credit_note_invoice_id VARCHAR(64) NOT NULL DEFAULT '',
    related_invoice_id VARCHAR(64) NOT NULL DEFAULT '',
    credited_cents BIGINT NOT NULL DEFAULT 0 CHECK (credited_cents >= 0),
    -- CHARGEBACK_REVERSED: the chargeback was won after the job was created;
    -- not re-invoiced automatically, flagged for an operator.
    flag VARCHAR(32) NOT NULL DEFAULT '',
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT credit_note_jobs_source UNIQUE (merchant_id, sale_id, source_kind, source_id)
);

CREATE INDEX IF NOT EXISTS idx_credit_note_jobs_due ON credit_note_jobs(next_attempt_at, id) WHERE status = 'PENDING';
CREATE INDEX IF NOT EXISTS idx_credit_note_jobs_flag ON credit_note_jobs(updated_at) WHERE flag <> '';

ALTER TABLE invoices ADD COLUMN IF NOT EXISTS global_part INTEGER NOT NULL DEFAULT 1 CHECK (global_part > 0);
DROP INDEX IF EXISTS idx_invoices_one_live_global;
CREATE UNIQUE INDEX IF NOT EXISTS idx_invoices_one_live_global_part ON invoices(merchant_id, global_period, global_part)
    WHERE kind = 'GLOBAL' AND status IN ('PENDING', 'STAMPED', 'CANCEL_REQUESTED');
