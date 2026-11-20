-- 007_money_posting.up.sql (MVP-07): server-derived settlement amounts and
-- atomic, idempotent money posting.

-- Per-merchant platform tariff in basis points; NULL = server default
-- (DEFAULT_PLATFORM_FEE_BPS).
ALTER TABLE merchants ADD COLUMN IF NOT EXISTS platform_fee_bps INTEGER
    CHECK (platform_fee_bps IS NULL OR (platform_fee_bps >= 0 AND platform_fee_bps <= 10000));

-- One row per settled card payment: the server-computed split, written in the
-- same transaction as the payment state change, the ledger journal and the
-- RESICO monthly accumulator. The primary key makes a double settle impossible.
CREATE TABLE IF NOT EXISTS payment_settlements (
    payment_id VARCHAR(128) PRIMARY KEY REFERENCES emv_payments(id) ON DELETE RESTRICT,
    merchant_id VARCHAR(128) NOT NULL,
    gross_cents BIGINT NOT NULL CHECK (gross_cents > 0),
    base_cents BIGINT NOT NULL CHECK (base_cents >= 0),
    iva_cents BIGINT NOT NULL CHECK (iva_cents >= 0),
    isr_cents BIGINT NOT NULL CHECK (isr_cents >= 0),
    isr_rate_bps BIGINT NOT NULL CHECK (isr_rate_bps >= 0),
    tax_escrow_cents BIGINT NOT NULL CHECK (tax_escrow_cents >= 0),
    platform_fee_bps BIGINT NOT NULL CHECK (platform_fee_bps >= 0),
    platform_fee_cents BIGINT NOT NULL CHECK (platform_fee_cents >= 0),
    rbf_sweep_cents BIGINT NOT NULL CHECK (rbf_sweep_cents >= 0),
    net_available_cents BIGINT NOT NULL,
    persona_moral BOOLEAN NOT NULL DEFAULT FALSE,
    year_month VARCHAR(7) NOT NULL,
    journal_id VARCHAR(128) NOT NULL,
    refunded_gross_cents BIGINT NOT NULL DEFAULT 0 CHECK (refunded_gross_cents >= 0 AND refunded_gross_cents <= gross_cents),
    refunded_base_cents BIGINT NOT NULL DEFAULT 0 CHECK (refunded_base_cents >= 0),
    refunded_tax_escrow_cents BIGINT NOT NULL DEFAULT 0 CHECK (refunded_tax_escrow_cents >= 0),
    refunded_fee_cents BIGINT NOT NULL DEFAULT 0 CHECK (refunded_fee_cents >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_payment_settlements_merchant ON payment_settlements(merchant_id);

-- Exactly-once keys for money-moving webhooks and operations
-- (codi:confirm:<tracking_key>, stp:abono:<claveRastreo>, refund keys). The
-- key is inserted with ON CONFLICT DO NOTHING inside the posting transaction;
-- a duplicate waits for the first transaction and then replays its response.
CREATE TABLE IF NOT EXISTS money_operations (
    op_key VARCHAR(255) PRIMARY KEY,
    status_code INTEGER NOT NULL DEFAULT 0,
    response_body BYTEA,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
