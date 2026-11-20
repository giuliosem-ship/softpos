package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"softpos/pkg/acquirer"
	"softpos/pkg/auth"
	"softpos/pkg/identity"
	"softpos/pkg/kyb"
	"softpos/pkg/store"
)

// kybRFC / kybCURP are SAT test identities (valid verification digit).
const (
	kybRFC  = "CACX7605101P8"
	kybCURP = "CACX760510MDFSHC09"
)

func kybApplication() map[string]any {
	return map[string]any{
		"legal_name": "Xochilt Casas Chavez", "rfc": kybRFC, "curp": kybCURP, "tax_regime": "RESICO_PF", "mcc": "5812",
		"address": map[string]any{"street": "Av. Reforma", "ext_number": "10", "neighborhood": "Centro",
			"city": "Ciudad de Mexico", "state": "CDMX", "postal_code": "06000"},
		"contact_email": "dueno@example.com", "contact_phone": "+525512345678",
	}
}

// draftMerchant registers a merchant through the public alta path (DRAFT
// onboarding, no CLABE) without HTTP login plumbing.
func draftMerchant(t *testing.T, s *Server, id string) {
	t.Helper()
	if err := s.registerMerchant(id, "GARM850101ABC", "Draft "+id, true); err != nil {
		t.Fatal(err)
	}
	repo, _ := s.kybStore()
	if _, err := repo.CreateKYB(context.Background(), store.KYBRecord{MerchantID: id, Status: kyb.StatusDraft}); err != nil {
		t.Fatal(err)
	}
}

var pngHeader = []byte("\x89PNG\r\n\x1a\n")

func docContent(prefix string) []byte {
	return append(append([]byte{}, pngHeader...), []byte(prefix+" synthetic test image, no real data")...)
}

func uploadDoc(t *testing.T, s *Server, merchantID string, role auth.Role, docType string, content []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("type", docType)
	fw, _ := mw.CreateFormFile("file", "doc.bin")
	_, _ = fw.Write(content)
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/merchants/"+merchantID+"/kyb/documents", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return serve(s, asRole(t, s, merchantID, role, req))
}

func kybCall(t *testing.T, s *Server, merchantID string, role auth.Role, method, path string, body any) (int, KYBResponse, string) {
	t.Helper()
	rec := serve(s, asRole(t, s, merchantID, role, jsonReq(method, path, body)))
	var out KYBResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Body.String()
}

func uploadAllDocs(t *testing.T, s *Server, merchantID string, selfie []byte) {
	t.Helper()
	for _, d := range kyb.RequiredDocuments() {
		content := docContent(string(d))
		if d == kyb.DocSelfie && selfie != nil {
			content = selfie
		}
		if rec := uploadDoc(t, s, merchantID, auth.RoleOwner, string(d), content); rec.Code != http.StatusCreated {
			t.Fatalf("upload %s: %d %s", d, rec.Code, rec.Body.String())
		}
	}
}

// completeKYB fills, documents and submits the onboarding of merchantID
// (IN_REVIEW) and approves it as an operator.
func completeKYB(t *testing.T, s *Server, merchantID string) store.KYBRecord {
	t.Helper()
	base := "/api/v1/merchants/" + merchantID + "/kyb"
	if code, _, body := kybCall(t, s, merchantID, auth.RoleOwner, http.MethodPut, base, kybApplication()); code != http.StatusOK {
		t.Fatalf("put kyb: %d %s", code, body)
	}
	uploadAllDocs(t, s, merchantID, nil)
	if code, resp, body := kybCall(t, s, merchantID, auth.RoleOwner, http.MethodPost, base+"/submit", nil); code != http.StatusOK || resp.Status != kyb.StatusInReview {
		t.Fatalf("submit: %d %s", code, body)
	}
	rec, err := s.ApproveMerchantKYB(context.Background(), merchantID, "usr_operator_test")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	return rec
}

func TestKYB_GetDraftShape(t *testing.T) {
	s, _ := setupTestServer(t)
	draftMerchant(t, s, "m_kyb_get")
	code, resp, body := kybCall(t, s, "m_kyb_get", auth.RoleCashier, http.MethodGet, "/api/v1/merchants/m_kyb_get/kyb", nil)
	if code != http.StatusOK || resp.Status != kyb.StatusDraft || resp.Checks.RFCValid || resp.Checks.DocumentsComplete ||
		resp.Checks.List69B != kyb.List69BUnknown || len(resp.RequiredDocuments) != 6 || resp.Application != nil {
		t.Fatalf("unexpected draft view (cashier gets no application data): %d %s", code, body)
	}
	var raw map[string]any
	_ = json.Unmarshal([]byte(body), &raw)
	for _, k := range []string{"status", "checks", "required_documents"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("contract field %q missing: %s", k, body)
		}
	}
	// Anti-IDOR: another tenant's KYB.
	if code, _, _ := kybCall(t, s, "m_other", auth.RoleOwner, http.MethodGet, "/api/v1/merchants/m_kyb_get/kyb", nil); code != http.StatusForbidden {
		t.Fatalf("foreign merchant kyb: want 403, got %d", code)
	}
}

func TestKYB_PutValidatesAndRequiresOwner(t *testing.T) {
	s, _ := setupTestServer(t)
	draftMerchant(t, s, "m_kyb_put")
	path := "/api/v1/merchants/m_kyb_put/kyb"
	if code, _, _ := kybCall(t, s, "m_kyb_put", auth.RoleCashier, http.MethodPut, path, kybApplication()); code != http.StatusForbidden {
		t.Fatalf("cashier put: want 403, got %d", code)
	}
	bad := kybApplication()
	bad["mcc"], bad["contact_email"] = "12", "not-an-email"
	rec := serve(s, asTenant(t, s, "m_kyb_put", jsonReq(http.MethodPut, path, bad)))
	if rec.Code != http.StatusBadRequest || !contains(rec.Body.String(), "INVALID_KYB_DATA") || !contains(rec.Body.String(), `"mcc"`) {
		t.Fatalf("invalid data: %d %s", rec.Code, rec.Body.String())
	}
	code, resp, body := kybCall(t, s, "m_kyb_put", auth.RoleOwner, http.MethodPut, path, kybApplication())
	if code != http.StatusOK || resp.Status != kyb.StatusDraft || !resp.Checks.RFCValid || resp.Application == nil || resp.Application.RFC != kybRFC {
		t.Fatalf("put: %d %s", code, body)
	}
}

func TestKYB_DocumentUploadLimitsAndEncryption(t *testing.T) {
	s, _ := setupTestServer(t)
	m := "m_kyb_docs"
	draftMerchant(t, s, m)
	if rec := uploadDoc(t, s, m, auth.RoleOwner, "PASSPORT", docContent("x")); rec.Code != http.StatusBadRequest || !contains(rec.Body.String(), "INVALID_DOCUMENT_TYPE") {
		t.Fatalf("bad type: %d %s", rec.Code, rec.Body.String())
	}
	if rec := uploadDoc(t, s, m, auth.RoleOwner, "CSF", []byte("plain text is not an accepted format")); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text file: want 415, got %d %s", rec.Code, rec.Body.String())
	}
	big := append(append([]byte{}, pngHeader...), make([]byte, kyb.MaxDocumentBytes)...)
	if rec := uploadDoc(t, s, m, auth.RoleOwner, "CSF", big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("too large: want 413, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := uploadDoc(t, s, m, auth.RoleCashier, "CSF", docContent("x")); rec.Code != http.StatusForbidden {
		t.Fatalf("cashier upload: want 403, got %d", rec.Code)
	}
	content := docContent("INE front of a fictitious person")
	rec := uploadDoc(t, s, m, auth.RoleOwner, "INE_FRONT", content)
	var out struct{ DocumentID, Type, Status string }
	_ = json.Unmarshal(rec.Body.Bytes(), &struct {
		DocumentID *string `json:"document_id"`
		Type       *string `json:"type"`
		Status     *string `json:"status"`
	}{&out.DocumentID, &out.Type, &out.Status})
	if rec.Code != http.StatusCreated || out.DocumentID == "" || out.Type != "INE_FRONT" || out.Status != "RECEIVED" {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	repo, blobs := s.kybStore()
	doc, found, _ := repo.GetKYBDocument(context.Background(), m, out.DocumentID)
	if !found || doc.ContentType != "image/png" || doc.SizeBytes != int64(len(content)) {
		t.Fatalf("metadata: %+v", doc)
	}
	env, found, _ := blobs.GetBlob(context.Background(), doc.BlobKey)
	if !found || bytes.Contains(env, []byte("fictitious")) {
		t.Fatal("blob must be stored encrypted")
	}
	plain, err := s.openKYBDocument(context.Background(), doc)
	if err != nil || !bytes.Equal(plain, content) {
		t.Fatalf("decrypt: %v", err)
	}
	// A new upload of the same type supersedes the previous one.
	uploadDoc(t, s, m, auth.RoleOwner, "INE_FRONT", docContent("second"))
	docs, _ := repo.ListKYBDocuments(context.Background(), m, false)
	if len(docs) != 1 || docs[0].ID == out.DocumentID {
		t.Fatalf("expected only the latest INE_FRONT active, got %+v", docs)
	}
}

func TestKYB_SubmitChecksFailKeepDraft(t *testing.T) {
	s, _ := setupTestServer(t)
	cases := []struct {
		name   string
		mod    func(map[string]any)
		docs   bool
		reason string
	}{
		{"documents missing", func(map[string]any) {}, false, kyb.ReasonDocumentsIncomplete},
		{"regime not RESICO PF", func(a map[string]any) { a["tax_regime"] = "ACTIVIDAD_EMPRESARIAL_PF" }, true, kyb.ReasonUnsupportedRegime},
		{"persona moral RFC", func(a map[string]any) { a["rfc"], a["curp"] = "EKU9003173C9", "" }, true, kyb.ReasonPersonaMoral},
		{"wrong verification digit", func(a map[string]any) { a["rfc"], a["curp"] = "CACX7605101P9", "" }, true, kyb.ReasonRFCInvalid},
	}
	for i, c := range cases {
		m := fmt.Sprintf("m_kyb_fail_%d", i)
		draftMerchant(t, s, m)
		app := kybApplication()
		c.mod(app)
		if code, _, body := kybCall(t, s, m, auth.RoleOwner, http.MethodPut, "/api/v1/merchants/"+m+"/kyb", app); code != http.StatusOK {
			t.Fatalf("%s: put %d %s", c.name, code, body)
		}
		if c.docs {
			uploadAllDocs(t, s, m, nil)
		}
		code, resp, body := kybCall(t, s, m, auth.RoleOwner, http.MethodPost, "/api/v1/merchants/"+m+"/kyb/submit", nil)
		if code != http.StatusUnprocessableEntity || resp.Status != kyb.StatusDraft || resp.Reason != c.reason || resp.Error != "KYB_CHECKS_FAILED" {
			t.Errorf("%s: want 422 DRAFT %s, got %d %s", c.name, c.reason, code, body)
		}
	}
}

func TestKYB_SubmitToReviewThenApproveAssignsCLABEAndOnboards(t *testing.T) {
	s, _ := setupTestServer(t)
	mock := acquirer.NewMockProvider(acquirer.MockConfig{})
	s.SetAcquirerProvider(mock)
	m := "m_kyb_ok"
	draftMerchant(t, s, m)
	base := "/api/v1/merchants/" + m + "/kyb"
	kybCall(t, s, m, auth.RoleOwner, http.MethodPut, base, kybApplication())
	uploadAllDocs(t, s, m, nil)

	// Not approved yet: every money operation is refused.
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/card-payments/intents"},
		{http.MethodPost, "/api/v1/payments/codi/qr"},
		{http.MethodPost, "/api/v1/disbursements"},
	} {
		req := jsonReq(c.method, c.path, map[string]any{"amount_cents": 1000, "concept": "x", "reference": "r1", "beneficiary_name": "x"})
		req.Header.Set("Idempotency-Key", "k-"+c.path)
		if rec := serve(s, asTenant(t, s, m, req)); rec.Code != http.StatusForbidden || !contains(rec.Body.String(), "MERCHANT_NOT_APPROVED") {
			t.Errorf("%s before approval: want 403 MERCHANT_NOT_APPROVED, got %d %s", c.path, rec.Code, rec.Body.String())
		}
	}

	s.blacklist.AddEntry("EFO110101XYZ", identity.BlacklistDefinitivo, "500-05-2023-1234") // a loaded 69-B list
	code, resp, body := kybCall(t, s, m, auth.RoleOwner, http.MethodPost, base+"/submit", nil)
	if code != http.StatusOK || resp.Status != kyb.StatusInReview || resp.SubmittedAt == "" || !resp.Checks.RFCValid ||
		!resp.Checks.DocumentsComplete || resp.Checks.List69B != kyb.List69BClear {
		t.Fatalf("submit: %d %s", code, body)
	}
	rec, _, _ := s.MerchantKYB(context.Background(), m)
	if rec.IdentityCheck != kyb.IdentityPassed || rec.IdentityReference == "" || rec.PersonaMoral {
		t.Fatalf("identity check not recorded: %+v", rec)
	}
	// Locked while in review.
	if code, _, _ := kybCall(t, s, m, auth.RoleOwner, http.MethodPut, base, kybApplication()); code != http.StatusConflict {
		t.Fatalf("put in review: want 409, got %d", code)
	}
	if r := uploadDoc(t, s, m, auth.RoleOwner, "CSF", docContent("late")); r.Code != http.StatusConflict {
		t.Fatalf("upload in review: want 409, got %d", r.Code)
	}
	if code, _, _ := kybCall(t, s, m, auth.RoleOwner, http.MethodPost, base+"/submit", nil); code != http.StatusConflict {
		t.Fatalf("resubmit: want 409, got %d", code)
	}

	if _, has := s.GetMerchantCLABE(m); has {
		t.Fatal("no virtual CLABE before approval")
	}
	approved, err := s.ApproveMerchantKYB(context.Background(), m, "usr_op_1")
	if err != nil || approved.Status != kyb.StatusApproved || approved.DecidedBy != "usr_op_1" || approved.AcquirerMerchantID == "" {
		t.Fatalf("approve: %+v %v", approved, err)
	}
	if mock.Calls("OnboardMerchant") != 1 {
		t.Fatalf("acquirer onboarding calls = %d", mock.Calls("OnboardMerchant"))
	}
	clabe, has := s.GetMerchantCLABE(m)
	if !has || len(clabe) != 18 {
		t.Fatalf("approval must assign the virtual CLABE, got %q", clabe)
	}
	if merch, _ := s.GetMerchant(m); merch.RFC != kybRFC || merch.Name != "Xochilt Casas Chavez" {
		t.Fatalf("approved identity must update the profile: %+v", merch)
	}
	if _, err := s.ApproveMerchantKYB(context.Background(), m, "usr_op_1"); !errors.Is(err, ErrKYBInvalidTransition) {
		t.Fatalf("double approval: %v", err)
	}
	code2, _, body2 := createIntent(t, s, m, "after-approval", map[string]any{"amount_cents": 1000, "is_persona_moral": true})
	if code2 != http.StatusCreated {
		t.Fatalf("intent after approval: %d %s", code2, body2)
	}
}

type failingOnboarding struct{ *acquirer.MockProvider }

func (failingOnboarding) OnboardMerchant(context.Context, acquirer.OnboardRequest) (acquirer.OnboardResult, error) {
	return acquirer.OnboardResult{}, acquirer.ErrProviderUnavailable
}

func TestKYB_ApprovalAbortsWhenAcquirerOnboardingFails(t *testing.T) {
	s, _ := setupTestServer(t)
	s.SetAcquirerProvider(failingOnboarding{acquirer.NewMockProvider(acquirer.MockConfig{})})
	m := "m_kyb_acqfail"
	draftMerchant(t, s, m)
	kybCall(t, s, m, auth.RoleOwner, http.MethodPut, "/api/v1/merchants/"+m+"/kyb", kybApplication())
	uploadAllDocs(t, s, m, nil)
	kybCall(t, s, m, auth.RoleOwner, http.MethodPost, "/api/v1/merchants/"+m+"/kyb/submit", nil)
	if _, err := s.ApproveMerchantKYB(context.Background(), m, "usr_op"); !errors.Is(err, acquirer.ErrProviderUnavailable) {
		t.Fatalf("want onboarding error, got %v", err)
	}
	if rec, _, _ := s.MerchantKYB(context.Background(), m); rec.Status != kyb.StatusInReview {
		t.Fatalf("status must stay IN_REVIEW for a retry, got %s", rec.Status)
	}
}

func TestKYB_Listed69BIsRejectedAndRejectedCanBeFixed(t *testing.T) {
	s, _ := setupTestServer(t)
	s.blacklist.AddEntry(kybRFC, identity.BlacklistDefinitivo, "500-05-2026-1")
	m := "m_kyb_69b"
	draftMerchant(t, s, m)
	base := "/api/v1/merchants/" + m + "/kyb"
	kybCall(t, s, m, auth.RoleOwner, http.MethodPut, base, kybApplication())
	uploadAllDocs(t, s, m, nil)
	code, resp, body := kybCall(t, s, m, auth.RoleOwner, http.MethodPost, base+"/submit", nil)
	if code != http.StatusOK || resp.Status != kyb.StatusRejected || resp.Reason != kyb.ReasonListed69B || resp.Checks.List69B != kyb.List69BListed || resp.DecidedAt == "" {
		t.Fatalf("69-B listed: %d %s", code, body)
	}
	// The owner can edit a rejected application (back to DRAFT).
	if code, resp, _ := kybCall(t, s, m, auth.RoleOwner, http.MethodPut, base, kybApplication()); code != http.StatusOK || resp.Status != kyb.StatusDraft {
		t.Fatalf("edit after rejection: %d %s", code, resp.Status)
	}
}

func TestKYB_IdentityFailureStillGoesToReviewAndOperatorRejects(t *testing.T) {
	s, _ := setupTestServer(t)
	m := "m_kyb_idfail"
	draftMerchant(t, s, m)
	base := "/api/v1/merchants/" + m + "/kyb"
	kybCall(t, s, m, auth.RoleOwner, http.MethodPut, base, kybApplication())
	uploadAllDocs(t, s, m, append(append([]byte{}, pngHeader...), []byte("FAIL")...))
	// The mock keys on the selfie prefix: a PNG header first, so use the
	// provider directly to assert the failure path.
	s.SetKYCProvider(kybFailProvider{})
	if code, resp, body := kybCall(t, s, m, auth.RoleOwner, http.MethodPost, base+"/submit", nil); code != http.StatusOK || resp.Status != kyb.StatusInReview {
		t.Fatalf("submit: %d %s", code, body)
	}
	if rec, _, _ := s.MerchantKYB(context.Background(), m); rec.IdentityCheck != kyb.IdentityFailed {
		t.Fatalf("identity result: %s", rec.IdentityCheck)
	}
	rec, err := s.RejectMerchantKYB(context.Background(), m, "usr_op", "INE does not match the selfie")
	if err != nil || rec.Status != kyb.StatusRejected || rec.Reason != "INE does not match the selfie" {
		t.Fatalf("reject: %+v %v", rec, err)
	}
}

type kybFailProvider struct{}

func (kybFailProvider) Name() string { return "fail" }
func (kybFailProvider) VerifyIdentity(context.Context, kyb.IdentityCheckRequest) (kyb.IdentityCheck, error) {
	return kyb.IdentityCheck{Result: kyb.IdentityFailed, Detail: "liveness failed"}, nil
}

func TestKYB_BlockedMerchantCannotMoveMoney(t *testing.T) {
	s, _ := setupCardServer(t)
	repo, _ := s.kybStore()
	rec, _, _ := repo.GetKYB(context.Background(), cardMerchant)
	rec.Blocked, rec.BlockedReason = true, "fraud investigation"
	if _, err := repo.UpdateKYB(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	code, _, body := createIntent(t, s, cardMerchant, "blocked-1", map[string]any{"amount_cents": 1000})
	if code != http.StatusForbidden || !contains(body, "MERCHANT_BLOCKED") {
		t.Fatalf("blocked merchant intent: %d %s", code, body)
	}
}

func TestKYB_ClientPersonaMoralIsIgnored(t *testing.T) {
	s, _ := setupCardServer(t)
	code, out, body := createIntent(t, s, cardMerchant, "pm-1", map[string]any{"amount_cents": 1000, "is_persona_moral": true})
	if code != http.StatusCreated {
		t.Fatalf("intent: %d %s", code, body)
	}
	ms := s.moneyStore().(*store.MemoryMoneyStore)
	cp, ok := ms.CardPayment(out.CardPaymentID)
	if !ok || cp.PersonaMoral {
		t.Fatalf("persona moral must be server-derived, got %+v", cp)
	}
}

// Audit (batch G): RBF origination credits the merchant's AVAILABLE balance,
// so it is a money route: never for a not-approved merchant, and never
// self-served by the owner (who could mint a fee-free "loan" and pay it out).
func TestKYB_RBFOriginationIsGatedAndNotSelfServed(t *testing.T) {
	s, _ := setupTestServer(t)
	draft := "m_kyb_rbf_draft"
	draftMerchant(t, s, draft)
	loan := func(m string, role auth.Role) *httptest.ResponseRecorder {
		return serve(s, asRole(t, s, m, role, jsonReq(http.MethodPost, "/api/v1/rbf/loans",
			map[string]any{"merchant_id": m, "principal_cents": 10_000_000, "fixed_fee_bps": 0, "holdback_bps": 100})))
	}
	if rec := loan(draft, auth.RoleOperator); rec.Code != http.StatusForbidden || !contains(rec.Body.String(), "MERCHANT_NOT_APPROVED") {
		t.Fatalf("loan for a not-approved merchant: want 403 MERCHANT_NOT_APPROVED, got %d %s", rec.Code, rec.Body.String())
	}
	approved := "m_kyb_rbf_ok"
	if err := s.RegisterMerchant(approved, "GARM850101ABC", "RBF ok", true); err != nil {
		t.Fatal(err)
	}
	if rec := loan(approved, auth.RoleOwner); rec.Code != http.StatusForbidden {
		t.Fatalf("owner self-originated loan: want 403, got %d %s", rec.Code, rec.Body.String())
	}
	if bal, err := s.ledger.GetBalance("LIABILITY:MERCHANT:" + approved + ":AVAILABLE"); err == nil && bal.AmountCents != 0 {
		t.Fatalf("refused loan must not credit the merchant, balance %d", bal.AmountCents)
	}
	if rec := loan(approved, auth.RoleOperator); rec.Code != http.StatusCreated {
		t.Fatalf("operator loan for an approved merchant: want 201, got %d %s", rec.Code, rec.Body.String())
	}
}

// Audit (batch G): two approvals racing past the "has no CLABE" check must
// not hand the merchant a second virtual CLABE (the loser would overwrite the
// one the winner already recorded and announced).
func TestKYB_VirtualCLABEIsAssignedOnlyOnce(t *testing.T) {
	s, _ := setupTestServer(t)
	m := "m_kyb_clabe_once"
	draftMerchant(t, s, m)
	first, err := s.AssignVirtualCLABE(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.AssignVirtualCLABE(context.Background(), m) // the racing approval
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetMerchantCLABE(m); second != first || got != first {
		t.Fatalf("virtual CLABE reassigned: first %s, second %s, stored %s", first, second, got)
	}
}

// Audit (batch G): a submission interrupted between the SUBMITTED write and
// the automatic 69-B rejection stays SUBMITTED with list_69b LISTED; the
// operator "approve a stuck SUBMITTED" path must not approve it.
func TestKYB_ApprovalRefusesRFCListed69B(t *testing.T) {
	s, _ := setupTestServer(t)
	m := "m_kyb_listed_stuck"
	draftMerchant(t, s, m)
	repo, _ := s.kybStore()
	rec, _, _ := repo.GetKYB(context.Background(), m)
	rec.Status, rec.List69B, rec.RFCValid, rec.DocumentsComplete = kyb.StatusSubmitted, kyb.List69BListed, true, true
	if _, err := repo.UpdateKYB(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApproveMerchantKYB(context.Background(), m, "usr_op_1"); !errors.Is(err, ErrKYBListed69B) {
		t.Fatalf("approve a 69-B listed RFC: want ErrKYBListed69B, got %v", err)
	}
	if got, _, _ := repo.GetKYB(context.Background(), m); got.Status == kyb.StatusApproved {
		t.Fatal("69-B listed merchant approved")
	}
	if _, has := s.GetMerchantCLABE(m); has {
		t.Fatal("no CLABE for a refused approval")
	}
}

// Audit (batch G): with no SAT 69-B list ingested (the worker is not wired
// yet) the submission must not tell the operator the RFC is CLEAR.
func TestKYB_SubmitWithoutA69BListIsUnknown(t *testing.T) {
	s, _ := setupTestServer(t)
	m := "m_kyb_no_69b_list"
	draftMerchant(t, s, m)
	base := "/api/v1/merchants/" + m + "/kyb"
	kybCall(t, s, m, auth.RoleOwner, http.MethodPut, base, kybApplication())
	uploadAllDocs(t, s, m, nil)
	code, resp, body := kybCall(t, s, m, auth.RoleOwner, http.MethodPost, base+"/submit", nil)
	if code != http.StatusOK || resp.Status != kyb.StatusInReview || resp.Checks.List69B != kyb.List69BUnknown {
		t.Fatalf("submit without a 69-B list: want IN_REVIEW with list_69b UNKNOWN, got %d %s", code, body)
	}
}
