-- 021_immutable_audit_kyb_retention.down.sql
ALTER TABLE kyb_documents DROP CONSTRAINT IF EXISTS kyb_documents_merchant_id_fkey;
ALTER TABLE kyb_documents ADD CONSTRAINT kyb_documents_merchant_id_fkey
    FOREIGN KEY (merchant_id) REFERENCES merchants(id) ON DELETE CASCADE;
ALTER TABLE merchant_kyb DROP CONSTRAINT IF EXISTS merchant_kyb_merchant_id_fkey;
ALTER TABLE merchant_kyb ADD CONSTRAINT merchant_kyb_merchant_id_fkey
    FOREIGN KEY (merchant_id) REFERENCES merchants(id) ON DELETE CASCADE;
DROP TRIGGER IF EXISTS operator_audit_log_no_truncate ON operator_audit_log;
DROP TRIGGER IF EXISTS operator_audit_log_no_update_delete ON operator_audit_log;
DROP FUNCTION IF EXISTS operator_audit_log_immutable();
