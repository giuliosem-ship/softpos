-- 015_sat_69b_ingestion.down.sql
DROP TABLE IF EXISTS sat_blacklist_loads;
DROP TABLE IF EXISTS sat_blacklist_meta;
DROP TABLE IF EXISTS sat_blacklist_staging;
ALTER TABLE sat_blacklist DROP COLUMN IF EXISTS nombre;
