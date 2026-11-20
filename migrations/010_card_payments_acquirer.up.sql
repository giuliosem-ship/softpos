-- 010_card_payments_acquirer.up.sql (MVP-14..16): card payments through a
-- certified processor (AcquirerProvider port), processor webhooks and
-- processor payout reconciliation. See docs/ACQUIRER_INTEGRATION.md.

-- One row per card payment intent. The status only moves on the processor's
-- signed webhooks (never on a client call):
--   CREATED -> AUTHORIZED -> CAPTURED -> PARTIALLY_REFUNDED -> REFUNDED
--   CREATED | AUTHORIZED -> DECLINED | CANCELED
--   CAPTURED | PARTIALLY_REFUNDED | CHARGEBACK_REVERSED -> CHARGEBACK -> CHARGEBACK_REVERSED
-- (merchant_id, intent_idempotency_key) makes intent creation idempotent;
-- (provider, provider_payment_id) maps webhooks back to the payment.
CREATE TABLE IF NOT EXISTS card_payments (
    id VARCHAR(64) PRIMARY KEY,
    merchant_id VARCHAR(128) NOT NULL,
    provider VARCHAR(32) NOT NULL,
    provider_payment_id VARCHAR(128),
    intent_idempotency_key VARCHAR(255) NOT NULL,
    request_hash VARCHAR(64) NOT NULL,
    amount_cents BIGINT NOT NULL CHECK (amount_cents > 0),
    currency VARCHAR(3) NOT NULL DEFAULT 'MXN',
    reference VARCHAR(255) NOT NULL DEFAULT '',
    persona_moral BOOLEAN NOT NULL DEFAULT FALSE,
    status VARCHAR(32) NOT NULL CHECK (status IN ('CREATED', 'AUTHORIZED', 'DECLINED', 'CANCELED',
        'CAPTURED', 'PARTIALLY_REFUNDED', 'REFUNDED', 'CHARGEBACK', 'CHARGEBACK_REVERSED')),
    captured_cents BIGINT NOT NULL DEFAULT 0 CHECK (captured_cents >= 0 AND captured_cents <= amount_cents),
    refunded_cents BIGINT NOT NULL DEFAULT 0 CHECK (refunded_cents >= 0 AND refunded_cents <= captured_cents),
    chargeback_cents BIGINT NOT NULL DEFAULT 0 CHECK (chargeback_cents >= 0),
    provider_fee_cents BIGINT NOT NULL DEFAULT 0 CHECK (provider_fee_cents >= 0),
    provider_settlement_date VARCHAR(10) NOT NULL DEFAULT '',
    provider_payout_id VARCHAR(128) NOT NULL DEFAULT '',
    provider_settled_at TIMESTAMPTZ,
    decline_code VARCHAR(64) NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT card_payments_merchant_idempotency_key UNIQUE (merchant_id, intent_idempotency_key)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_card_payments_provider_payment
    ON card_payments(provider, provider_payment_id) WHERE provider_payment_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_card_payments_unsettled
    ON card_payments(provider_settlement_date) WHERE provider_payout_id = '' AND captured_cents > 0;

-- Refund requests sent to the processor. Funds only move when the refund
-- webhook arrives; REQUESTED/PENDING amounts are held against the refundable
-- remainder so concurrent requests cannot over-refund.
CREATE TABLE IF NOT EXISTS card_payment_refunds (
    id VARCHAR(64) PRIMARY KEY,
    card_payment_id VARCHAR(64) NOT NULL REFERENCES card_payments(id) ON DELETE RESTRICT,
    merchant_id VARCHAR(128) NOT NULL,
    idempotency_key VARCHAR(255) NOT NULL,
    amount_cents BIGINT NOT NULL CHECK (amount_cents > 0),
    status VARCHAR(16) NOT NULL CHECK (status IN ('REQUESTED', 'PENDING', 'SUCCEEDED', 'FAILED')),
    provider_refund_id VARCHAR(128),
    last_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT card_payment_refunds_idempotency UNIQUE (card_payment_id, idempotency_key)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_card_payment_refunds_provider
    ON card_payment_refunds(card_payment_id, provider_refund_id) WHERE provider_refund_id IS NOT NULL;

-- Settlement records are shared by legacy EMV payments and processor card
-- payments (same split, same proportional refund reversal), so the FK to
-- emv_payments is dropped; payment ids are unique across both (cp_ prefix).
ALTER TABLE payment_settlements DROP CONSTRAINT IF EXISTS payment_settlements_payment_id_fkey;

-- Processor payouts (settlements of funds to Kerno's bank account, T+n).
CREATE TABLE IF NOT EXISTS acquirer_payouts (
    provider VARCHAR(32) NOT NULL,
    payout_id VARCHAR(128) NOT NULL,
    amount_cents BIGINT NOT NULL,
    expected_cents BIGINT NOT NULL,
    settlement_date VARCHAR(10) NOT NULL,
    item_count INTEGER NOT NULL DEFAULT 0,
    discrepancy_count INTEGER NOT NULL DEFAULT 0,
    status VARCHAR(16) NOT NULL CHECK (status IN ('RECONCILED', 'DISCREPANCY')),
    journal_id VARCHAR(128) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (provider, payout_id)
);

-- Reconciliation report: one row per difference found (missing payment,
-- amount / fee mismatch, duplicate settlement, payout total mismatch, capture
-- not settled by its expected date). dedupe_key makes every check idempotent.
CREATE TABLE IF NOT EXISTS acquirer_reconciliation_items (
    id BIGSERIAL PRIMARY KEY,
    dedupe_key VARCHAR(255) NOT NULL UNIQUE,
    provider VARCHAR(32) NOT NULL,
    payout_id VARCHAR(128) NOT NULL DEFAULT '',
    card_payment_id VARCHAR(64) NOT NULL DEFAULT '',
    provider_payment_id VARCHAR(128) NOT NULL DEFAULT '',
    kind VARCHAR(32) NOT NULL CHECK (kind IN ('MISSING_PAYMENT', 'AMOUNT_MISMATCH', 'FEE_MISMATCH',
        'DUPLICATE_SETTLEMENT', 'PAYOUT_TOTAL_MISMATCH', 'NOT_SETTLED')),
    expected_cents BIGINT NOT NULL DEFAULT 0,
    actual_cents BIGINT NOT NULL DEFAULT 0,
    detail TEXT NOT NULL DEFAULT '',
    status VARCHAR(16) NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN', 'RESOLVED')),
    resolution_note TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_acquirer_recon_status ON acquirer_reconciliation_items(status, created_at);
