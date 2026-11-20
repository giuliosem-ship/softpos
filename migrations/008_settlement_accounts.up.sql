-- 008_settlement_accounts.up.sql (MVP-08): the merchant's own bank account
-- (settlement CLABE) that payouts may go to, distinct from the virtual
-- collection CLABE. Payouts require status VERIFIED (ownership = merchant RFC).

ALTER TABLE merchants ADD COLUMN IF NOT EXISTS settlement_clabe VARCHAR(18);
ALTER TABLE merchants ADD COLUMN IF NOT EXISTS settlement_clabe_status VARCHAR(16) NOT NULL DEFAULT 'NONE'
    CHECK (settlement_clabe_status IN ('NONE', 'PENDING', 'VERIFIED', 'REJECTED'));
ALTER TABLE merchants ADD COLUMN IF NOT EXISTS settlement_clabe_updated_at TIMESTAMPTZ;
