package store

import (
	"context"
	"strings"
	"testing"

	"softpos/pkg/kyb"
)

// TestPGIntegration_OperatorAuditLogImmutable (MVP-28): the database itself
// rejects UPDATE, DELETE and TRUNCATE on operator_audit_log (even for the
// table owner the tests connect as); INSERT keeps working.
func TestPGIntegration_OperatorAuditLogImmutable(t *testing.T) {
	db := openMigratedPG(t)
	ctx := context.Background()
	audit := NewSQLAuditRepository(db)
	if err := audit.AppendAudit(ctx, AuditEntry{OperatorID: "op", Action: "merchant.block", MerchantID: "m1", Detail: "x"}); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE operator_audit_log SET detail = 'tampered'`,
		`DELETE FROM operator_audit_log`,
		`TRUNCATE operator_audit_log`,
	} {
		_, err := db.ExecContext(ctx, stmt)
		if err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("%s: want append-only rejection, got %v", stmt, err)
		}
	}
	rows, err := audit.ListAudit(ctx, AuditQuery{MerchantID: "m1"})
	if err != nil || len(rows) != 1 || rows[0].Detail != "x" {
		t.Fatalf("row must be intact: %+v %v", rows, err)
	}
	if err := audit.AppendAudit(ctx, AuditEntry{OperatorID: "op", Action: "merchant.unblock", MerchantID: "m1"}); err != nil {
		t.Fatalf("insert must still work: %v", err)
	}
}

// TestPGIntegration_KYBRetentionRestrict (MVP-28): deleting a merchant with
// an onboarding file or KYC documents is refused (ON DELETE RESTRICT) and
// the file survives.
func TestPGIntegration_KYBRetentionRestrict(t *testing.T) {
	db := openMigratedPG(t)
	ctx := context.Background()
	merchants := NewSQLMerchantRepository(db)
	repo := NewSQLKYBRepository(db)
	for _, id := range []string{"m_kyb", "m_doc"} {
		if err := merchants.SaveMerchant(ctx, MerchantRecord{ID: id, RFC: "CACX7605101P8", Name: id, IsRESICO: true}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.CreateKYB(ctx, KYBRecord{MerchantID: "m_kyb", Status: kyb.StatusApproved}); err != nil {
		t.Fatal(err)
	}
	if err := repo.AddKYBDocument(ctx, KYBDocument{ID: "doc1", MerchantID: "m_doc", Type: kyb.DocINEFront, ContentType: "image/png",
		SizeBytes: 10, SHA256: strings.Repeat("a", 64), BlobKey: "k1"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"m_kyb", "m_doc"} {
		_, err := db.ExecContext(ctx, `DELETE FROM merchants WHERE id = $1`, id)
		if err == nil || !isForeignKeyViolation(err) {
			t.Fatalf("delete %s: want FK restrict, got %v", id, err)
		}
	}
	if _, found, _ := repo.GetKYB(ctx, "m_kyb"); !found {
		t.Fatal("onboarding file must survive")
	}
	if docs, _ := repo.ListKYBDocuments(ctx, "m_doc", true); len(docs) != 1 {
		t.Fatalf("documents must survive: %+v", docs)
	}
	var rule string
	if err := db.QueryRowContext(ctx, `SELECT confdeltype FROM pg_constraint WHERE conname = 'merchant_kyb_merchant_id_fkey'`).Scan(&rule); err != nil || rule != "r" {
		t.Fatalf("merchant_kyb FK delete rule: %q %v", rule, err)
	}
}
