-- 013_sales_and_refresh_grace.up.sql (MVP-19)
--
-- 1. merchant_sales: one row per credited SPEI abono / CoDi confirmation,
--    written in the same money transaction as the journal. Card payments
--    stay in card_payments; GET /api/v1/merchants/{id}/sales merges both,
--    newest first, with a (created_at, id) cursor.
CREATE TABLE IF NOT EXISTS merchant_sales (
    id VARCHAR(160) PRIMARY KEY, -- spei_<institucion>:<clave_rastreo> | codi_<tracking_key>
    merchant_id VARCHAR(128) NOT NULL,
    kind VARCHAR(8) NOT NULL CHECK (kind IN ('spei', 'codi')),
    status VARCHAR(16) NOT NULL,
    amount_cents BIGINT NOT NULL CHECK (amount_cents > 0),
    reference VARCHAR(255) NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_merchant_sales_page ON merchant_sales(merchant_id, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_merchant_sales_created ON merchant_sales(created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_card_payments_merchant_page ON card_payments(merchant_id, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_card_payments_created ON card_payments(created_at DESC, id DESC);

-- Backfill from the ledger: SPEI abono journals (spei_in_<inst>:<clave>, credit
-- to the merchant's AVAILABLE account) and CoDi journals (codi_<tracking>,
-- gross = the debit to the STP concentrator).
INSERT INTO merchant_sales (id, merchant_id, kind, status, amount_cents, reference, created_at)
SELECT 'spei_' || substr(j.id, 9), split_part(e.account_id, ':', 3), 'spei', 'RECEIVED', e.amount_cents,
       COALESCE(j.metadata_json->>'clave_rastreo', ''), j.created_at
FROM ledger_journals j
JOIN ledger_entries e ON e.journal_id = j.id
WHERE j.id LIKE 'spei\_in\_%' AND e.direction = 'CREDIT' AND e.account_id LIKE 'LIABILITY:MERCHANT:%:AVAILABLE'
ON CONFLICT (id) DO NOTHING;

INSERT INTO merchant_sales (id, merchant_id, kind, status, amount_cents, reference, created_at)
SELECT j.id, j.metadata_json->>'merchant_id', 'codi', 'CONFIRMED', SUM(e.amount_cents),
       COALESCE(j.metadata_json->>'tracking_key', ''), j.created_at
FROM ledger_journals j
JOIN ledger_entries e ON e.journal_id = j.id AND e.direction = 'DEBIT'
WHERE j.id LIKE 'codi\_%' AND COALESCE(j.metadata_json->>'merchant_id', '') <> ''
GROUP BY j.id, j.metadata_json, j.created_at
ON CONFLICT (id) DO NOTHING;

-- 2. Refresh-token grace window (docs/AUTH.md): the successor token sealed
--    under a key derived from its predecessor, and the one-time grace marker.
ALTER TABLE refresh_tokens ADD COLUMN IF NOT EXISTS grace_sealed BYTEA;
ALTER TABLE refresh_tokens ADD COLUMN IF NOT EXISTS grace_used_at TIMESTAMPTZ;
