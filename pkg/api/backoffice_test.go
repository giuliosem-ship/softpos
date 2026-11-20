package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"softpos/pkg/acquirer"
	"softpos/pkg/auth"
	"softpos/pkg/kyb"
	"softpos/pkg/store"
	"softpos/pkg/stp"
)

// operatorToken grants a platform operator and returns a backoffice token.
func operatorToken(t *testing.T, s *Server, phone string) (string, string) {
	t.Helper()
	u, err := s.AuthService().GrantPlatformOperator(context.Background(), phone, "test")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := s.AuthService().MintAccessToken(u.ID, auth.BackofficeScope, auth.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	return tok, u.ID
}

func boCall(s *Server, tok, method, path string, body any) *httptest.ResponseRecorder {
	return serve(s, withBearer(jsonReq(method, path, body), tok))
}

func auditActions(t *testing.T, s *Server, merchantID string) []string {
	t.Helper()
	rows, err := s.OperatorAudit().ListAudit(context.Background(), store.AuditQuery{MerchantID: merchantID})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for i := len(rows) - 1; i >= 0; i-- {
		out = append(out, rows[i].Action)
	}
	return out
}

// submittedMerchant prepares a merchant whose onboarding is IN_REVIEW.
func submittedMerchant(t *testing.T, s *Server, m string) {
	t.Helper()
	draftMerchant(t, s, m)
	base := "/api/v1/merchants/" + m + "/kyb"
	kybCall(t, s, m, auth.RoleOwner, http.MethodPut, base, kybApplication())
	uploadAllDocs(t, s, m, nil)
	if code, _, body := kybCall(t, s, m, auth.RoleOwner, http.MethodPost, base+"/submit", nil); code != http.StatusOK {
		t.Fatalf("submit: %d %s", code, body)
	}
}

func TestBackoffice_OperatorLoginAndAccessControl(t *testing.T) {
	s, _ := setupTestServer(t)
	sender := withTestAuth(t, s)
	if _, err := s.AuthService().GrantPlatformOperator(context.Background(), "+525500000001", "test"); err != nil {
		t.Fatal(err)
	}
	tp := httpLogin(t, s, sender, "+525500000001", auth.BackofficeScope)
	if tp.MerchantID != auth.BackofficeScope || tp.Role != auth.RoleOperator {
		t.Fatalf("backoffice session = %+v", tp)
	}
	if rec := boCall(s, tp.AccessToken, http.MethodGet, "/api/v1/backoffice/me", nil); rec.Code != http.StatusOK {
		t.Fatalf("me: %d %s", rec.Code, rec.Body.String())
	}
	// Refresh keeps the backoffice scope.
	ref := postJSON(t, s.Router(), "/api/v1/auth/refresh", map[string]string{"refresh_token": tp.RefreshToken})
	var refreshed auth.TokenPair
	_ = json.Unmarshal(ref.Body.Bytes(), &refreshed)
	if ref.Code != http.StatusOK || refreshed.MerchantID != auth.BackofficeScope || refreshed.Role != auth.RoleOperator {
		t.Fatalf("refresh: %d %s", ref.Code, ref.Body.String())
	}

	// A regular user cannot select the backoffice scope.
	postJSON(t, s.Router(), "/api/v1/auth/otp/request", map[string]string{"phone": "+525500000002"})
	code, _ := sender.LastCode("+525500000002")
	rec := postJSON(t, s.Router(), "/api/v1/auth/otp/verify", map[string]string{"phone": "+525500000002", "code": code, "merchant_id": auth.BackofficeScope})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-operator backoffice login: want 403, got %d %s", rec.Code, rec.Body.String())
	}

	_ = s.RegisterMerchant("m_bo_acl", "GARM850101ABC", "ACL", true)
	for name, tok := range map[string]string{
		"merchant owner":    mustMint(t, s, "usr_owner", "m_bo_acl", auth.RoleOwner),
		"merchant operator": mustMint(t, s, "usr_mop", "m_bo_acl", auth.RoleOperator),
	} {
		if rec := boCall(s, tok, http.MethodGet, "/api/v1/backoffice/merchants", nil); rec.Code != http.StatusForbidden {
			t.Errorf("%s on backoffice: want 403, got %d", name, rec.Code)
		}
	}
	if rec := serve(s, jsonReq(http.MethodGet, "/api/v1/backoffice/merchants", nil)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: want 401, got %d", rec.Code)
	}
	// A backoffice token cannot act as a merchant.
	if rec := boCall(s, tp.AccessToken, http.MethodGet, "/api/v1/merchants/m_bo_acl/balances", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("backoffice token on merchant route: want 403, got %d", rec.Code)
	}
	// Removing the operator takes effect on the next request.
	_ = s.AuthService().Repository().RemovePlatformOperator(context.Background(), tp.UserID)
	if rec := boCall(s, tp.AccessToken, http.MethodGet, "/api/v1/backoffice/me", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("removed operator: want 403, got %d", rec.Code)
	}
}

func mustMint(t *testing.T, s *Server, user, merchant string, role auth.Role) string {
	t.Helper()
	tok, err := s.AuthService().MintAccessToken(user, merchant, role)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestBackoffice_ReviewDownloadApproveWithAudit(t *testing.T) {
	s, _ := setupTestServer(t)
	mock := acquirer.NewMockProvider(acquirer.MockConfig{})
	s.SetAcquirerProvider(mock)
	tok, opID := operatorToken(t, s, "+525500000010")
	const m = "m_bo_review"
	submittedMerchant(t, s, m)
	draftMerchant(t, s, "m_bo_draft")

	rec := boCall(s, tok, http.MethodGet, "/api/v1/backoffice/merchants?kyb_status=IN_REVIEW", nil)
	var list struct{ Items []BOMerchantSummary }
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if rec.Code != http.StatusOK || len(list.Items) != 1 || list.Items[0].MerchantID != m || list.Items[0].IdentityCheck != "PASSED" {
		t.Fatalf("list in review: %d %s", rec.Code, rec.Body.String())
	}
	if rec := boCall(s, tok, http.MethodGet, "/api/v1/backoffice/merchants?kyb_status=NOPE", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad filter: %d", rec.Code)
	}

	rec = boCall(s, tok, http.MethodGet, "/api/v1/backoffice/merchants/"+m, nil)
	var detail struct {
		KYB       BOKYBDetail  `json:"kyb"`
		Documents []BODocument `json:"documents"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &detail)
	if rec.Code != http.StatusOK || detail.KYB.Application.RFC != kybRFC || len(detail.Documents) != 6 || detail.KYB.Status != kyb.StatusInReview {
		t.Fatalf("detail: %d %s", rec.Code, rec.Body.String())
	}
	var docID string
	for _, d := range detail.Documents {
		if d.Type == "SELFIE" {
			docID = d.ID
		}
	}
	dl := boCall(s, tok, http.MethodGet, "/api/v1/backoffice/merchants/"+m+"/documents/"+docID, nil)
	if dl.Code != http.StatusOK || !bytes.Equal(dl.Body.Bytes(), docContent("SELFIE")) || dl.Header().Get("Content-Type") != "image/png" ||
		!strings.HasPrefix(dl.Header().Get("Content-Disposition"), "attachment;") || dl.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("download: %d %v", dl.Code, dl.Header())
	}
	if rec := boCall(s, tok, http.MethodGet, "/api/v1/backoffice/merchants/m_bo_draft/documents/"+docID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("document under another merchant: want 404, got %d", rec.Code)
	}

	if rec := boCall(s, tok, http.MethodPost, "/api/v1/backoffice/merchants/m_bo_draft/kyb/approve", map[string]any{}); rec.Code != http.StatusConflict {
		t.Fatalf("approve a DRAFT: want 409, got %d %s", rec.Code, rec.Body.String())
	}
	rec = boCall(s, tok, http.MethodPost, "/api/v1/backoffice/merchants/"+m+"/kyb/approve", map[string]any{"note": "INE verified"})
	if rec.Code != http.StatusOK || !contains(rec.Body.String(), `"status":"APPROVED"`) {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	if clabe, ok := s.GetMerchantCLABE(m); !ok || clabe == "" || mock.Calls("OnboardMerchant") != 1 {
		t.Fatalf("approval must assign the CLABE and onboard (clabe=%q calls=%d)", clabe, mock.Calls("OnboardMerchant"))
	}
	rows, _ := s.OperatorAudit().ListAudit(context.Background(), store.AuditQuery{MerchantID: m, Action: "kyb.approve"})
	if len(rows) != 1 || rows[0].OperatorID != opID || !contains(string(rows[0].Before), `"IN_REVIEW"`) ||
		!contains(string(rows[0].After), `"APPROVED"`) || !contains(string(rows[0].After), `"virtual_clabe":"646180`) || rows[0].Detail != "INE verified" {
		t.Fatalf("approve audit: %+v", rows)
	}
	if got := strings.Join(auditActions(t, s, m), ","); got != "kyb.view,document.download,kyb.approve" {
		t.Fatalf("audit trail = %s", got)
	}
	logRec := boCall(s, tok, http.MethodGet, "/api/v1/backoffice/audit-log?merchant_id="+m, nil)
	if logRec.Code != http.StatusOK || !contains(logRec.Body.String(), "document.download") {
		t.Fatalf("audit log endpoint: %d %s", logRec.Code, logRec.Body.String())
	}
}

func TestBackoffice_RejectNeedsReason(t *testing.T) {
	s, _ := setupTestServer(t)
	tok, _ := operatorToken(t, s, "+525500000020")
	submittedMerchant(t, s, "m_bo_reject")
	if rec := boCall(s, tok, http.MethodPost, "/api/v1/backoffice/merchants/m_bo_reject/kyb/reject", map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("no reason: want 400, got %d", rec.Code)
	}
	rec := boCall(s, tok, http.MethodPost, "/api/v1/backoffice/merchants/m_bo_reject/kyb/reject", map[string]any{"reason": "Comprobante de domicilio ilegible"})
	if rec.Code != http.StatusOK || !contains(rec.Body.String(), "REJECTED") {
		t.Fatalf("reject: %d %s", rec.Code, rec.Body.String())
	}
	code, resp, _ := kybCall(t, s, "m_bo_reject", auth.RoleOwner, http.MethodGet, "/api/v1/merchants/m_bo_reject/kyb", nil)
	if code != http.StatusOK || resp.Status != kyb.StatusRejected || resp.Reason != "Comprobante de domicilio ilegible" {
		t.Fatalf("owner view after rejection: %d %+v", code, resp)
	}
}

func TestBackoffice_BlockStopsEveryMoneyOperation(t *testing.T) {
	const m = "m_bo_block"
	s := setupPayoutMerchant(t, m, 50000)
	s.SetAcquirerProvider(acquirer.NewMockProvider(acquirer.MockConfig{}))
	tok, _ := operatorToken(t, s, "+525500000030")
	if rec := boCall(s, tok, http.MethodPost, "/api/v1/backoffice/merchants/"+m+"/block", map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("block without reason: want 400, got %d", rec.Code)
	}
	if rec := boCall(s, tok, http.MethodPost, "/api/v1/backoffice/merchants/"+m+"/block", map[string]any{"reason": "chargeback fraud"}); rec.Code != http.StatusOK {
		t.Fatalf("block: %d %s", rec.Code, rec.Body.String())
	}
	if rec := boCall(s, tok, http.MethodPost, "/api/v1/backoffice/merchants/"+m+"/block", map[string]any{"reason": "again"}); rec.Code != http.StatusConflict {
		t.Fatalf("double block: want 409, got %d", rec.Code)
	}
	blocked := func(rec *httptest.ResponseRecorder, what string) {
		t.Helper()
		if rec.Code != http.StatusForbidden || !contains(rec.Body.String(), "MERCHANT_BLOCKED") {
			t.Errorf("%s while blocked: want 403 MERCHANT_BLOCKED, got %d %s", what, rec.Code, rec.Body.String())
		}
	}
	code, _, body := createIntent(t, s, m, "blk-1", map[string]any{"amount_cents": 1000})
	if code != http.StatusForbidden || !contains(body, "MERCHANT_BLOCKED") {
		t.Errorf("intent while blocked: %d %s", code, body)
	}
	blocked(serve(s, payoutReq(t, s, m, "blk-payout", map[string]any{"amount_cents": 1000})), "payout")
	blocked(serve(s, asTenant(t, s, m, jsonReq(http.MethodPost, "/api/v1/payments/codi/qr", map[string]any{"amount_cents": 1000, "concept": "x", "reference": "r"}))), "CoDi QR")
	blocked(serve(s, asTenant(t, s, m, jsonReq(http.MethodPost, "/api/v1/card-payments/cp_x/refunds", map[string]any{}))), "refund")

	if rec := boCall(s, tok, http.MethodPost, "/api/v1/backoffice/merchants/"+m+"/unblock", map[string]any{"reason": "cleared"}); rec.Code != http.StatusOK {
		t.Fatalf("unblock: %d %s", rec.Code, rec.Body.String())
	}
	if code, _, body := createIntent(t, s, m, "blk-2", map[string]any{"amount_cents": 1000}); code != http.StatusCreated {
		t.Fatalf("intent after unblock: %d %s", code, body)
	}
	if got := strings.Join(auditActions(t, s, m), ","); got != "merchant.block,merchant.unblock" {
		t.Fatalf("audit = %s", got)
	}
}

type ambiguousWithStatus struct {
	ambiguousSTP
	*stp.MockOrderStatusClient
}

func TestBackoffice_PaymentsLedgerAndPayoutRetry(t *testing.T) {
	const m = "m_bo_money"
	s, _ := setupTestServer(t)
	_ = s.RegisterMerchantCLABE("646180123400000007", m, "GARM850101ABC")
	if err := s.SeedVerifiedSettlementAccount(context.Background(), m, settlementCLABE); err != nil {
		t.Fatal(err)
	}
	s.SetAcquirerProvider(acquirer.NewMockProvider(acquirer.MockConfig{}))
	tok, _ := operatorToken(t, s, "+525500000040")
	if rec := serve(s, jsonReq(http.MethodPost, "/api/v1/webhooks/stp/abono", signedAbono(t, s, "SPEIBO0001", "646180123400000007", 80000))); rec.Code != http.StatusOK {
		t.Fatalf("abono: %d %s", rec.Code, rec.Body.String())
	}
	_, intent, _ := createIntent(t, s, m, "bo-card", map[string]any{"amount_cents": 1234})

	rec := boCall(s, tok, http.MethodGet, "/api/v1/backoffice/payments?merchant_id="+m, nil)
	if rec.Code != http.StatusOK || !contains(rec.Body.String(), "spei_40012:SPEIBO0001") || !contains(rec.Body.String(), intent.CardPaymentID) ||
		!contains(rec.Body.String(), `"merchant_id":"`+m+`"`) {
		t.Fatalf("payments by merchant: %d %s", rec.Code, rec.Body.String())
	}
	rec = boCall(s, tok, http.MethodGet, "/api/v1/backoffice/payments?id="+intent.CardPaymentID, nil)
	var byID struct{ Items []BOSale }
	_ = json.Unmarshal(rec.Body.Bytes(), &byID)
	if len(byID.Items) != 1 || byID.Items[0].Kind != "card" {
		t.Fatalf("payment by id: %s", rec.Body.String())
	}
	if rec := boCall(s, tok, http.MethodGet, "/api/v1/backoffice/payments?from=2026-13-01", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad date: want 400, got %d", rec.Code)
	}

	rec = boCall(s, tok, http.MethodGet, "/api/v1/backoffice/merchants/"+m+"/ledger", nil)
	if rec.Code != http.StatusOK || !contains(rec.Body.String(), `"available_cents":80000`) || !contains(rec.Body.String(), "spei_in_40012:SPEIBO0001") {
		t.Fatalf("ledger: %d %s", rec.Code, rec.Body.String())
	}

	// A payout whose registraOrden outcome is unknown, then STP says liquidated.
	s.SetSTPClient(ambiguousSTP{})
	pr := serve(s, payoutReq(t, s, m, "bo-payout-1", map[string]any{"amount_cents": 30000}))
	var pending struct {
		ClaveRastreo string `json:"clave_rastreo"`
	}
	_ = json.Unmarshal(pr.Body.Bytes(), &pending)
	if pr.Code != http.StatusAccepted || pending.ClaveRastreo == "" {
		t.Fatalf("pending payout: %d %s", pr.Code, pr.Body.String())
	}
	rec = boCall(s, tok, http.MethodGet, "/api/v1/backoffice/payouts?status=PENDING_RECONCILIATION", nil)
	if rec.Code != http.StatusOK || !contains(rec.Body.String(), pending.ClaveRastreo) {
		t.Fatalf("payouts: %d %s", rec.Code, rec.Body.String())
	}
	statuses := stp.NewMockOrderStatusClient()
	statuses.Set(pending.ClaveRastreo, stp.OrderStatus{State: stp.OrderLiquidated, STPOrderID: 777})
	s.SetSTPClient(ambiguousWithStatus{MockOrderStatusClient: statuses})
	rec = boCall(s, tok, http.MethodPost, "/api/v1/backoffice/payouts/"+pending.ClaveRastreo+"/retry", nil)
	if rec.Code != http.StatusOK || !contains(rec.Body.String(), `"outcome":"SETTLED"`) || !contains(rec.Body.String(), `"status":"SETTLED"`) {
		t.Fatalf("retry: %d %s", rec.Code, rec.Body.String())
	}
	if rec := boCall(s, tok, http.MethodPost, "/api/v1/backoffice/payouts/"+pending.ClaveRastreo+"/retry", nil); rec.Code != http.StatusConflict {
		t.Fatalf("retry of a settled payout: want 409, got %d", rec.Code)
	}
	if rec := boCall(s, tok, http.MethodPost, "/api/v1/backoffice/payouts/NOPE/retry", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown payout: want 404, got %d", rec.Code)
	}
	if got := auditActions(t, s, m); len(got) != 2 || got[0] != "ledger.view" || got[1] != "payout.retry" {
		t.Fatalf("audit = %v", got)
	}
	if rec := boCall(s, tok, http.MethodGet, "/api/v1/backoffice/reconciliation", nil); rec.Code != http.StatusOK {
		t.Fatalf("reconciliation report: %d %s", rec.Code, rec.Body.String())
	}
}

func TestBackoffice_StaticUIWithCSP(t *testing.T) {
	s, _ := setupTestServer(t)
	s.SetBackofficeDir("../../web/backoffice")
	for _, path := range []string{"/backoffice/", "/backoffice/app.js", "/backoffice/styles.css"} {
		rec := serve(s, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		csp := rec.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") || rec.Header().Get("X-Frame-Options") != "DENY" {
			t.Fatalf("%s: missing security headers %v", path, rec.Header())
		}
	}
	index := serve(s, httptest.NewRequest(http.MethodGet, "/backoffice/", nil)).Body.String()
	if strings.Contains(index, "<script>") || strings.Contains(index, " onclick=") || strings.Contains(index, "style=") {
		t.Fatal("the backoffice page must not use inline script or style (CSP)")
	}
}

// Audit (batch G): segregation of duties. A platform operator who is also a
// member of a merchant (its owner, typically) cannot approve that merchant's
// onboarding nor lift its block.
func TestBackoffice_OperatorCannotDecideOwnMerchant(t *testing.T) {
	s, _ := setupTestServer(t)
	s.SetAcquirerProvider(acquirer.NewMockProvider(acquirer.MockConfig{}))
	m := "m_bo_own"
	submittedMerchant(t, s, m)
	tok, opID := operatorToken(t, s, "+525500000071")
	if err := s.AuthService().AddMembership(context.Background(), opID, m, auth.RoleOwner); err != nil {
		t.Fatal(err)
	}
	if rec := boCall(s, tok, http.MethodPost, "/api/v1/backoffice/merchants/"+m+"/kyb/approve", map[string]any{}); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "OPERATOR_IS_MERCHANT_MEMBER") {
		t.Fatalf("self approval: want 403 OPERATOR_IS_MERCHANT_MEMBER, got %d %s", rec.Code, rec.Body.String())
	}
	if rec, _, _ := s.MerchantKYB(context.Background(), m); rec.Status == kyb.StatusApproved {
		t.Fatal("operator approved its own merchant")
	}
	other, _ := operatorToken(t, s, "+525500000072")
	if rec := boCall(s, other, http.MethodPost, "/api/v1/backoffice/merchants/"+m+"/block", map[string]any{"reason": "review"}); rec.Code != http.StatusOK {
		t.Fatalf("block: %d %s", rec.Code, rec.Body.String())
	}
	if rec := boCall(s, tok, http.MethodPost, "/api/v1/backoffice/merchants/"+m+"/unblock", map[string]any{}); rec.Code != http.StatusForbidden {
		t.Fatalf("self unblock: want 403, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := boCall(s, other, http.MethodPost, "/api/v1/backoffice/merchants/"+m+"/kyb/approve", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("approval by another operator: %d %s", rec.Code, rec.Body.String())
	}
}
