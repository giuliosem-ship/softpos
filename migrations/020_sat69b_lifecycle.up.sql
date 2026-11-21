-- 020_sat69b_lifecycle.up.sql (MVP-27): 69-B block lifecycle. See docs/KYB.md.
--
-- blocked_source records who placed the current block: OPERATOR (backoffice)
-- or SAT_69B (the re-review after a list load). Only SAT_69B blocks are
-- lifted automatically when the RFC leaves PRESUNTO/DEFINITIVO.
ALTER TABLE merchant_kyb ADD COLUMN IF NOT EXISTS blocked_source VARCHAR(16) NOT NULL DEFAULT '';
UPDATE merchant_kyb SET blocked_source = CASE WHEN blocked_reason = 'RFC_LISTED_69B' THEN 'SAT_69B' ELSE 'OPERATOR' END
    WHERE blocked AND blocked_source = '';
ALTER TABLE merchant_kyb DROP CONSTRAINT IF EXISTS merchant_kyb_blocked_source_check;
ALTER TABLE merchant_kyb ADD CONSTRAINT merchant_kyb_blocked_source_check CHECK (blocked_source IN ('', 'OPERATOR', 'SAT_69B'));

-- Documented exceptions: an operator unblocked a merchant whose RFC is still
-- listed. The next loads respect the exception for the same merchant, RFC and
-- situation; a situation change (e.g. PRESUNTO -> DEFINITIVO) or the RFC
-- leaving the list revokes it.
CREATE TABLE IF NOT EXISTS sat69b_exceptions (
    id BIGSERIAL PRIMARY KEY,
    merchant_id VARCHAR(128) NOT NULL REFERENCES merchants(id) ON DELETE RESTRICT,
    rfc VARCHAR(13) NOT NULL,
    situation VARCHAR(32) NOT NULL,
    reason TEXT NOT NULL CHECK (reason <> ''),
    operator_id VARCHAR(128) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at TIMESTAMPTZ,
    revoked_by VARCHAR(128) NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_sat69b_exceptions_active
    ON sat69b_exceptions(merchant_id, rfc, situation) WHERE revoked_at IS NULL;

-- Change protection: a load whose situations changed massively (same row
-- count, different content) is refused (REJECTED_CHANGES) unless overridden.
ALTER TABLE sat_blacklist_loads ADD COLUMN IF NOT EXISTS override_changes BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE sat_blacklist_loads DROP CONSTRAINT IF EXISTS sat_blacklist_loads_status_check;
ALTER TABLE sat_blacklist_loads ADD CONSTRAINT sat_blacklist_loads_status_check
    CHECK (status IN ('LOADED', 'UNCHANGED', 'REJECTED_SHRINK', 'REJECTED_CHANGES', 'FAILED'));
