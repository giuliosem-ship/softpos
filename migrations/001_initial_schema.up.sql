-- 001_initial_schema.up.sql
-- SoftPOS Hybrid Fiscal & Payment Platform (Mexico)
-- Relational Schema with Integer-Only Monetary Precision (BIGINT cents)

CREATE TABLE IF NOT EXISTS ledger_accounts (
    id VARCHAR(128) PRIMARY KEY,
    code VARCHAR(128) NOT NULL UNIQUE,
    name VARCHAR(255) NOT NULL,
    account_type VARCHAR(32) NOT NULL CHECK (account_type IN ('ASSET', 'LIABILITY', 'EQUITY', 'REVENUE', 'EXPENSE')),
    balance_cents BIGINT NOT NULL DEFAULT 0,
    currency VARCHAR(3) NOT NULL DEFAULT 'MXN',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS ledger_journals (
    id VARCHAR(128) PRIMARY KEY,
    description TEXT NOT NULL,
    metadata_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS ledger_entries (
    id BIGSERIAL PRIMARY KEY,
    journal_id VARCHAR(128) NOT NULL REFERENCES ledger_journals(id) ON DELETE RESTRICT,
    account_id VARCHAR(128) NOT NULL REFERENCES ledger_accounts(id) ON DELETE RESTRICT,
    direction VARCHAR(16) NOT NULL CHECK (direction IN ('DEBIT', 'CREDIT')),
    amount_cents BIGINT NOT NULL CHECK (amount_cents >= 0),
    currency VARCHAR(3) NOT NULL DEFAULT 'MXN',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_ledger_entries_account_id ON ledger_entries(account_id);
CREATE INDEX IF NOT EXISTS idx_ledger_entries_journal_id ON ledger_entries(journal_id);

CREATE TABLE IF NOT EXISTS sat_blacklist (
    rfc VARCHAR(13) PRIMARY KEY,
    status VARCHAR(32) NOT NULL CHECK (status IN ('CLEAN', 'PRESUNTO', 'DESVIRTUADO', 'DEFINITIVO', 'SENTENCIA_FAVORABLE')),
    oficio_sat VARCHAR(128) NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS merchants (
    id VARCHAR(128) PRIMARY KEY,
    rfc VARCHAR(13) NOT NULL,
    name VARCHAR(255) NOT NULL,
    is_resico BOOLEAN NOT NULL DEFAULT TRUE,
    virtual_clabe VARCHAR(18) UNIQUE,
    encrypted_csd_json JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_merchants_rfc ON merchants(rfc);
CREATE INDEX IF NOT EXISTS idx_merchants_virtual_clabe ON merchants(virtual_clabe);

CREATE TABLE IF NOT EXISTS merchant_monthly_sales (
    merchant_id VARCHAR(128) NOT NULL REFERENCES merchants(id) ON DELETE CASCADE,
    year_month VARCHAR(7) NOT NULL,
    accumulated_cents BIGINT NOT NULL DEFAULT 0 CHECK (accumulated_cents >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (merchant_id, year_month)
);

CREATE TABLE IF NOT EXISTS idempotency_keys (
    idempotency_key VARCHAR(255) PRIMARY KEY,
    status_code INT NOT NULL,
    response_body BYTEA NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_idempotency_keys_expires_at ON idempotency_keys(expires_at);
