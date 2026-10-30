-- 001_initial_schema.down.sql
-- Rollback initial schema for SoftPOS Hybrid Fiscal & Payment Platform

DROP TABLE IF EXISTS idempotency_keys CASCADE;
DROP TABLE IF EXISTS merchant_monthly_sales CASCADE;
DROP TABLE IF EXISTS merchants CASCADE;
DROP TABLE IF EXISTS sat_blacklist CASCADE;
DROP TABLE IF EXISTS ledger_entries CASCADE;
DROP TABLE IF EXISTS ledger_journals CASCADE;
DROP TABLE IF EXISTS ledger_accounts CASCADE;
