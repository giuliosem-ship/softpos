-- 012_merchant_kyb.up.sql (MVP-17): merchant onboarding (KYB/KYC) application,
-- automatic checks, operator decision, merchant block flag and encrypted KYC
-- documents. See docs/KYB.md.
--
-- Status flow: DRAFT -> SUBMITTED -> IN_REVIEW -> APPROVED | REJECTED
-- (REJECTED -> DRAFT when the owner edits). Only APPROVED and not blocked
-- merchants may create card intents, CoDi/SPEI QR codes or payouts.
-- version is an optimistic-concurrency counter (every update bumps it).
CREATE TABLE IF NOT EXISTS merchant_kyb (
    merchant_id VARCHAR(128) PRIMARY KEY REFERENCES merchants(id) ON DELETE CASCADE,
    status VARCHAR(16) NOT NULL CHECK (status IN ('DRAFT', 'SUBMITTED', 'IN_REVIEW', 'APPROVED', 'REJECTED')),
    application_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    persona_moral BOOLEAN NOT NULL DEFAULT FALSE,
    rfc_valid BOOLEAN NOT NULL DEFAULT FALSE,
    list_69b VARCHAR(16) NOT NULL DEFAULT 'UNKNOWN' CHECK (list_69b IN ('CLEAR', 'LISTED', 'UNKNOWN')),
    documents_complete BOOLEAN NOT NULL DEFAULT FALSE,
    identity_check VARCHAR(16) NOT NULL DEFAULT 'UNKNOWN',
    identity_reference VARCHAR(128) NOT NULL DEFAULT '',
    identity_detail TEXT NOT NULL DEFAULT '',
    acquirer_merchant_id VARCHAR(128) NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    submitted_at TIMESTAMPTZ,
    decided_at TIMESTAMPTZ,
    decided_by VARCHAR(128) NOT NULL DEFAULT '',
    blocked BOOLEAN NOT NULL DEFAULT FALSE,
    blocked_reason TEXT NOT NULL DEFAULT '',
    blocked_at TIMESTAMPTZ,
    version BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_merchant_kyb_status ON merchant_kyb(status, updated_at);

-- Uploaded KYC documents (metadata). The content is stored encrypted
-- (identity.DocumentVault envelope) in kyb_document_blobs under blob_key.
CREATE TABLE IF NOT EXISTS kyb_documents (
    id VARCHAR(64) PRIMARY KEY,
    merchant_id VARCHAR(128) NOT NULL REFERENCES merchants(id) ON DELETE CASCADE,
    doc_type VARCHAR(32) NOT NULL CHECK (doc_type IN ('INE_FRONT', 'INE_BACK', 'SELFIE', 'PROOF_OF_ADDRESS', 'BANK_STATEMENT', 'CSF')),
    content_type VARCHAR(64) NOT NULL,
    size_bytes BIGINT NOT NULL CHECK (size_bytes > 0),
    sha256 VARCHAR(64) NOT NULL,
    blob_key VARCHAR(255) NOT NULL,
    status VARCHAR(16) NOT NULL DEFAULT 'RECEIVED' CHECK (status IN ('RECEIVED', 'SUPERSEDED')),
    uploaded_by VARCHAR(64) NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_kyb_documents_merchant ON kyb_documents(merchant_id, doc_type, created_at);

-- Encrypted document blobs (DB implementation of the blob store; an object
-- store such as S3 replaces it behind store.DocumentBlobStore).
CREATE TABLE IF NOT EXISTS kyb_document_blobs (
    blob_key VARCHAR(255) PRIMARY KEY,
    envelope BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
