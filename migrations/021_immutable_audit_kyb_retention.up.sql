-- 021_immutable_audit_kyb_retention.up.sql (MVP-28). See docs/BACKOFFICE.md.
--
-- operator_audit_log is append-only at the database level: a trigger rejects
-- UPDATE, DELETE and TRUNCATE for every role (including the table owner).
-- Defense in depth: the app role should also lack UPDATE/DELETE/TRUNCATE
-- grants (SQL in docs/BACKOFFICE.md). Lines inside the function body must not
-- end with ";" (the migration runner splits statements on line-final ";").
CREATE OR REPLACE FUNCTION operator_audit_log_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'operator_audit_log is append-only: % rejected', TG_OP USING ERRCODE = 'insufficient_privilege'; RETURN NULL; END
$$;

DROP TRIGGER IF EXISTS operator_audit_log_no_update_delete ON operator_audit_log;
CREATE TRIGGER operator_audit_log_no_update_delete BEFORE UPDATE OR DELETE ON operator_audit_log
    FOR EACH ROW EXECUTE FUNCTION operator_audit_log_immutable();

DROP TRIGGER IF EXISTS operator_audit_log_no_truncate ON operator_audit_log;
CREATE TRIGGER operator_audit_log_no_truncate BEFORE TRUNCATE ON operator_audit_log
    FOR EACH STATEMENT EXECUTE FUNCTION operator_audit_log_immutable();

-- KYB retention: deleting a merchant no longer deletes its onboarding file
-- (application, checks, decision) nor its document metadata; the delete is
-- refused while they exist (a retention/deletion policy must remove them
-- explicitly, see docs/KYB.md).
ALTER TABLE merchant_kyb DROP CONSTRAINT IF EXISTS merchant_kyb_merchant_id_fkey;
ALTER TABLE merchant_kyb ADD CONSTRAINT merchant_kyb_merchant_id_fkey
    FOREIGN KEY (merchant_id) REFERENCES merchants(id) ON DELETE RESTRICT;
ALTER TABLE kyb_documents DROP CONSTRAINT IF EXISTS kyb_documents_merchant_id_fkey;
ALTER TABLE kyb_documents ADD CONSTRAINT kyb_documents_merchant_id_fkey
    FOREIGN KEY (merchant_id) REFERENCES merchants(id) ON DELETE RESTRICT;
