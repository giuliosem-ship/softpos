-- 002_rbf_and_payments.up.sql
-- SoftPOS Hybrid Fiscal & Payment Platform (Mexico)
-- Persistence for RBF Loans, EMV Contactless Payments, and Transactional Outbox Events

CREATE TABLE IF NOT EXISTS rbf_loans (
    merchant_id VARCHAR(128) PRIMARY KEY,
    principal_cents BIGINT NOT NULL CHECK (principal_cents > 0),
    fee_cents BIGINT NOT NULL CHECK (fee_cents >= 0),
    repayment_cap_cents BIGINT NOT NULL CHECK (repayment_cap_cents >= principal_cents),
    holdback_bps BIGINT NOT NULL CHECK (holdback_bps >= 0 AND holdback_bps <= 10000),
    repaid_cents BIGINT NOT NULL DEFAULT 0 CHECK (repaid_cents >= 0),
    remaining_cents BIGINT NOT NULL CHECK (remaining_cents >= 0),
    status VARCHAR(32) NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'PAID_OFF', 'DEFAULTED')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_rbf_loans_status ON rbf_loans(status);

CREATE TABLE IF NOT EXISTS emv_payments (
    id VARCHAR(128) PRIMARY KEY,
    merchant_id VARCHAR(128) NOT NULL,
    amount_cents BIGINT NOT NULL CHECK (amount_cents > 0),
    currency VARCHAR(3) NOT NULL DEFAULT 'MXN',
    status VARCHAR(32) NOT NULL CHECK (status IN ('AUTHORIZED', 'SETTLED', 'VOIDED', 'REFUNDED')),
    masked_pan VARCHAR(32) NOT NULL,
    cryptogram_type VARCHAR(16) NOT NULL,
    cryptogram_hex VARCHAR(64) NOT NULL,
    atc INT NOT NULL,
    tlv_payload_hex TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_emv_payments_merchant_id ON emv_payments(merchant_id);
CREATE INDEX IF NOT EXISTS idx_emv_payments_status ON emv_payments(status);

CREATE TABLE IF NOT EXISTS outbox_events (
    id BIGSERIAL PRIMARY KEY,
    event_type VARCHAR(64) NOT NULL,
    aggregate_id VARCHAR(128) NOT NULL,
    payload_json JSONB NOT NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING', 'PROCESSING', 'PUBLISHED', 'FAILED')),
    retry_count INT NOT NULL DEFAULT 0,
    max_retries INT NOT NULL DEFAULT 5,
    last_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_outbox_events_status_retry ON outbox_events(status, retry_count);
CREATE INDEX IF NOT EXISTS idx_outbox_events_created_at ON outbox_events(created_at);
