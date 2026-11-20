-- 003_codi_payments.up.sql
-- Persistence for CoDi QR payment requests and Banxico confirmations.

CREATE TABLE IF NOT EXISTS codi_requests (
    reference VARCHAR(128) PRIMARY KEY,
    seller_clabe VARCHAR(18) NOT NULL,
    amount_cents BIGINT NOT NULL CHECK (amount_cents > 0),
    concept TEXT NOT NULL DEFAULT '',
    institution_code VARCHAR(8) NOT NULL DEFAULT '646',
    version INT NOT NULL DEFAULT 1,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_codi_requests_seller_clabe ON codi_requests(seller_clabe);

CREATE TABLE IF NOT EXISTS codi_confirmations (
    tracking_key VARCHAR(30) PRIMARY KEY,
    seller_clabe VARCHAR(18) NOT NULL DEFAULT '',
    buyer_clabe VARCHAR(64) NOT NULL DEFAULT '',
    amount_cents BIGINT NOT NULL CHECK (amount_cents > 0),
    status VARCHAR(32) NOT NULL,
    banxico_approval_code VARCHAR(128) NOT NULL DEFAULT '',
    confirmed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_codi_confirmations_seller_clabe ON codi_confirmations(seller_clabe);
