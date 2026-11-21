-- 015_sat_69b_ingestion.up.sql (MVP-20)
-- SAT Article 69-B list ingestion: the complete list is loaded into a
-- staging table and swapped into sat_blacklist in one transaction, so
-- lookups never see a partial list. sat_blacklist_meta records which file
-- is loaded (sha-256 version, row count, fetched/verified times) and
-- sat_blacklist_loads keeps the history of every attempt.

ALTER TABLE sat_blacklist ADD COLUMN IF NOT EXISTS nombre VARCHAR(255) NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS sat_blacklist_staging (
    rfc VARCHAR(13) PRIMARY KEY,
    status VARCHAR(32) NOT NULL,
    oficio_sat VARCHAR(128) NOT NULL DEFAULT '',
    nombre VARCHAR(255) NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS sat_blacklist_meta (
    id SMALLINT PRIMARY KEY CHECK (id = 1),
    list_version VARCHAR(64) NOT NULL,
    source VARCHAR(512) NOT NULL DEFAULT '',
    published VARCHAR(128) NOT NULL DEFAULT '',
    row_count INTEGER NOT NULL CHECK (row_count >= 0),
    malformed_count INTEGER NOT NULL DEFAULT 0,
    fetched_at TIMESTAMPTZ NOT NULL,
    verified_at TIMESTAMPTZ NOT NULL,
    loaded_by VARCHAR(128) NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS sat_blacklist_loads (
    id BIGSERIAL PRIMARY KEY,
    status VARCHAR(16) NOT NULL CHECK (status IN ('LOADED', 'UNCHANGED', 'REJECTED_SHRINK', 'FAILED')),
    list_version VARCHAR(64) NOT NULL DEFAULT '',
    source VARCHAR(512) NOT NULL DEFAULT '',
    row_count INTEGER NOT NULL DEFAULT 0,
    previous_count INTEGER NOT NULL DEFAULT 0,
    malformed_count INTEGER NOT NULL DEFAULT 0,
    override_shrink BOOLEAN NOT NULL DEFAULT FALSE,
    detail TEXT NOT NULL DEFAULT '',
    actor VARCHAR(128) NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_sat_blacklist_loads_created ON sat_blacklist_loads(created_at DESC, id DESC);
