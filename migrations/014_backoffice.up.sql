-- 014_backoffice.up.sql (MVP-18): internal operators (backoffice) and the
-- operator audit log. See docs/BACKOFFICE.md.

-- Platform (Kerno-internal) operators: global, not tied to a merchant. They
-- log in with the normal OTP flow selecting merchant_id "kerno:backoffice".
-- Rows are created only by the bootstrap env var or the operatorctl CLI,
-- never through the public API.
CREATE TABLE IF NOT EXISTS platform_operators (
    user_id VARCHAR(64) PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    created_by VARCHAR(128) NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Append-only record of every operator action (who, what, when, before/after).
CREATE TABLE IF NOT EXISTS operator_audit_log (
    id BIGSERIAL PRIMARY KEY,
    operator_id VARCHAR(64) NOT NULL,
    action VARCHAR(64) NOT NULL,
    merchant_id VARCHAR(128) NOT NULL DEFAULT '',
    target VARCHAR(255) NOT NULL DEFAULT '',
    before_json JSONB,
    after_json JSONB,
    detail TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_operator_audit_merchant ON operator_audit_log(merchant_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_operator_audit_operator ON operator_audit_log(operator_id, id DESC);
