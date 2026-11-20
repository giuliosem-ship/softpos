-- 006_attestation_nonces.up.sql
-- Single-use Play Integrity nonces (MVP-05). Stored as SHA-256; used_at marks consumption.

CREATE TABLE IF NOT EXISTS attestation_nonces (
    nonce_hash VARCHAR(64) PRIMARY KEY,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_attestation_nonces_expires ON attestation_nonces(expires_at);
