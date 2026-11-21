-- 017_invoices.up.sql (MVP-22)
-- CFDI invoices issued from sales (individual and factura global), their
-- link to the sales (a sale belongs to at most one active invoice) and the
-- per-merchant folio sequence. See docs/CFDI.md.

CREATE TABLE IF NOT EXISTS invoices (
    id VARCHAR(64) PRIMARY KEY,
    merchant_id VARCHAR(128) NOT NULL REFERENCES merchants(id) ON DELETE RESTRICT,
    kind VARCHAR(16) NOT NULL CHECK (kind IN ('INDIVIDUAL', 'GLOBAL')),
    status VARCHAR(20) NOT NULL CHECK (status IN ('PENDING', 'STAMPED', 'FAILED', 'CANCEL_REQUESTED', 'CANCELED')),
    idempotency_key VARCHAR(255) NOT NULL,
    request_hash VARCHAR(64) NOT NULL,
    serie VARCHAR(25) NOT NULL,
    folio BIGINT NOT NULL CHECK (folio > 0),
    uuid VARCHAR(36),
    subtotal_cents BIGINT NOT NULL CHECK (subtotal_cents >= 0),
    iva_cents BIGINT NOT NULL CHECK (iva_cents >= 0),
    isr_retenido_cents BIGINT NOT NULL DEFAULT 0 CHECK (isr_retenido_cents >= 0),
    total_cents BIGINT NOT NULL CHECK (total_cents >= 0),
    forma_pago VARCHAR(2) NOT NULL DEFAULT '',
    receptor_json JSONB NOT NULL,
    global_period VARCHAR(16) NOT NULL DEFAULT '',
    replaces_invoice_id VARCHAR(64) NOT NULL DEFAULT '',
    xml BYTEA,
    sealed_at TIMESTAMPTZ,
    error_code VARCHAR(64) NOT NULL DEFAULT '',
    error_detail TEXT NOT NULL DEFAULT '',
    stamp_attempts INTEGER NOT NULL DEFAULT 0,
    stamp_lease_until TIMESTAMPTZ,
    cancel_motivo VARCHAR(2) NOT NULL DEFAULT '',
    cancel_folio_sustitucion VARCHAR(36) NOT NULL DEFAULT '',
    cancel_status VARCHAR(16) NOT NULL DEFAULT '' CHECK (cancel_status IN ('', 'REQUESTED', 'ACCEPTED', 'REJECTED')),
    cancel_requested_at TIMESTAMPTZ,
    cancel_resolved_at TIMESTAMPTZ,
    emailed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    stamped_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT invoices_merchant_idempotency UNIQUE (merchant_id, idempotency_key),
    CONSTRAINT invoices_merchant_serie_folio UNIQUE (merchant_id, serie, folio)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_invoices_uuid ON invoices(uuid) WHERE uuid IS NOT NULL;
-- At most one live factura global per merchant and period.
CREATE UNIQUE INDEX IF NOT EXISTS idx_invoices_one_live_global ON invoices(merchant_id, global_period)
    WHERE kind = 'GLOBAL' AND status IN ('PENDING', 'STAMPED', 'CANCEL_REQUESTED');
CREATE INDEX IF NOT EXISTS idx_invoices_page ON invoices(merchant_id, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_invoices_status ON invoices(status, updated_at);

CREATE TABLE IF NOT EXISTS invoice_sales (
    invoice_id VARCHAR(64) NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,
    merchant_id VARCHAR(128) NOT NULL,
    sale_id VARCHAR(160) NOT NULL,
    position INTEGER NOT NULL,
    base_cents BIGINT NOT NULL CHECK (base_cents > 0),
    iva_cents BIGINT NOT NULL CHECK (iva_cents >= 0),
    active BOOLEAN NOT NULL DEFAULT TRUE,
    PRIMARY KEY (invoice_id, sale_id)
);

-- A sale is in at most one active invoice.
CREATE UNIQUE INDEX IF NOT EXISTS idx_invoice_sales_one_active ON invoice_sales(merchant_id, sale_id) WHERE active;

CREATE TABLE IF NOT EXISTS invoice_folios (
    merchant_id VARCHAR(128) NOT NULL,
    serie VARCHAR(25) NOT NULL,
    last_folio BIGINT NOT NULL,
    PRIMARY KEY (merchant_id, serie)
);
