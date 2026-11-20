-- 009_payouts_and_outbox_leases.up.sql (MVP-12)

-- Persistent payout (SPEI disbursement) records. A row is inserted in the
-- same transaction that reserves (debits) the merchant's funds, keyed by the
-- merchant-scoped Idempotency-Key, so a retried request replays the stored
-- outcome instead of paying twice, and a crash between the debit and STP
-- registraOrden leaves a RESERVED row the payout reconciler can resolve.
-- Status: RESERVED (debited, dispatch in flight or interrupted) ->
-- DISPATCHED (STP accepted) | PENDING_RECONCILIATION (outcome unknown) |
-- FAILED (rejected; reservation compensated); SETTLED (STP confirmed
-- liquidation during recovery).
CREATE TABLE IF NOT EXISTS payouts (
    clave_rastreo VARCHAR(30) PRIMARY KEY,
    merchant_id VARCHAR(128) NOT NULL,
    idempotency_key VARCHAR(255) NOT NULL,
    request_hash VARCHAR(64) NOT NULL,
    amount_cents BIGINT NOT NULL CHECK (amount_cents > 0),
    destination_clabe VARCHAR(18) NOT NULL,
    beneficiary_name VARCHAR(255) NOT NULL DEFAULT '',
    beneficiary_rfc VARCHAR(13) NOT NULL DEFAULT '',
    concept VARCHAR(255) NOT NULL DEFAULT '',
    fecha_operacion VARCHAR(8) NOT NULL,
    status VARCHAR(32) NOT NULL
        CHECK (status IN ('RESERVED', 'DISPATCHED', 'PENDING_RECONCILIATION', 'SETTLED', 'FAILED')),
    stp_order_id BIGINT NOT NULL DEFAULT 0,
    remaining_balance_cents BIGINT NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT payouts_merchant_idempotency_key UNIQUE (merchant_id, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_payouts_recovery ON payouts(status, updated_at);

-- Outbox leases: the dispatcher instance that claimed an event. Results are
-- recorded only by the current lease owner while the event is PROCESSING.
ALTER TABLE outbox_events ADD COLUMN IF NOT EXISTS lease_owner VARCHAR(128) NOT NULL DEFAULT '';
