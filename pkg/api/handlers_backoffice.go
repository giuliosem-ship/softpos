package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"softpos/pkg/acquirer"
	"softpos/pkg/auth"
	"softpos/pkg/kyb"
	"softpos/pkg/payouts"
	"softpos/pkg/store"
)

// Internal backoffice (MVP-18): platform operators only (token scoped to
// auth.BackofficeScope with role operator, user listed in
// platform_operators). Every action writes an operator_audit_log row. See
// docs/BACKOFFICE.md.

const backofficePrefix = "/api/v1/backoffice/"

// SetOperatorAuditRepository replaces the in-memory audit log (SQL with DATABASE_URL).
func (s *Server) SetOperatorAuditRepository(repo store.OperatorAuditRepository) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auditRepo = repo
}

func (s *Server) auditLog() store.OperatorAuditRepository {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.auditRepo
}

// OperatorAudit returns the audit repository (tests / tooling).
func (s *Server) OperatorAudit() store.OperatorAuditRepository { return s.auditLog() }

func marshalAudit(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// audit appends one operator action. Reads of personal data call it before
// serving (fail closed); mutations after the change (a failure is logged as
// an ALERT, the change already happened).
func (s *Server) audit(ctx context.Context, operatorID, action, merchantID, target string, before, after any, detail string) error {
	err := s.auditLog().AppendAudit(ctx, store.AuditEntry{
		OperatorID: operatorID, Action: action, MerchantID: merchantID, Target: target,
		Before: marshalAudit(before), After: marshalAudit(after), Detail: detail,
	})
	if err != nil {
		slog.Error("ALERT operator audit write failed", "operator", operatorID, "action", action, "merchant_id", merchantID, "target", target, "error", err)
	}
	return err
}

// auditOperatorAction records an action of a (per-merchant or platform)
// operator from the request context, best effort.
func (s *Server) auditOperatorAction(r *http.Request, action, merchantID, target string, before, after any, detail string) {
	user, _ := UserIDFromContext(r.Context())
	if user == "" {
		user = "unknown"
	}
	_ = s.audit(r.Context(), user, action, merchantID, target, before, after, detail)
}

// requirePlatformOperator admits only backoffice sessions of current
// platform operators (removal takes effect on the next request).
func (s *Server) requirePlatformOperator(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenant, _ := MerchantIDFromContext(r.Context())
	user, _ := UserIDFromContext(r.Context())
	if tenant != auth.BackofficeScope || RoleFromContext(r.Context()) != auth.RoleOperator || user == "" {
		writeError(w, http.StatusForbidden, "BACKOFFICE_FORBIDDEN")
		return "", false
	}
	ok, err := s.authService().IsPlatformOperator(r.Context(), user)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "OPERATOR_LOOKUP_FAILED")
		return "", false
	}
	if !ok {
		writeError(w, http.StatusForbidden, "BACKOFFICE_FORBIDDEN")
		return "", false
	}
	return user, true
}

// requireNotMerchantMember enforces segregation of duties: a platform
// operator who is a member of the merchant (its owner, typically) cannot
// decide on it (approve its onboarding, lift its block).
func (s *Server) requireNotMerchantMember(w http.ResponseWriter, r *http.Request, operator, merchantID string) bool {
	memberships, err := s.authService().Repository().ListMemberships(r.Context(), operator)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "OPERATOR_LOOKUP_FAILED")
		return false
	}
	for _, m := range memberships {
		if m.MerchantID == merchantID {
			slog.Warn("operator decision on own merchant refused", "operator", operator, "merchant_id", merchantID)
			writeError(w, http.StatusForbidden, "OPERATOR_IS_MERCHANT_MEMBER")
			return false
		}
	}
	return true
}

// backoffice wraps a handler with the platform-operator check.
func (s *Server) backoffice(h func(w http.ResponseWriter, r *http.Request, operator string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		op, ok := s.requirePlatformOperator(w, r)
		if !ok {
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		h(w, r, op)
	}
}

func (s *Server) backofficeRoutes() {
	s.router.HandleFunc("GET "+backofficePrefix+"me", s.backoffice(s.handleBOMe))
	s.router.HandleFunc("GET "+backofficePrefix+"sat69b", s.backoffice(s.handleBOSAT69BStatus))
	s.router.HandleFunc("POST "+backofficePrefix+"sat69b/sync", s.backoffice(s.handleBOSAT69BSync))
	s.router.HandleFunc("GET "+backofficePrefix+"invoices", s.backoffice(s.handleBOInvoices))
	s.router.HandleFunc("POST "+backofficePrefix+"invoices/{invoice}/retry", s.backoffice(s.handleBORetryInvoice))
	s.router.HandleFunc("GET "+backofficePrefix+"credit-notes", s.backoffice(s.handleBOCreditNoteJobs))
	s.router.HandleFunc("GET "+backofficePrefix+"merchants", s.backoffice(s.handleBOListMerchants))
	s.router.HandleFunc("GET "+backofficePrefix+"merchants/{merchant}", s.backoffice(s.handleBOMerchantDetail))
	s.router.HandleFunc("GET "+backofficePrefix+"merchants/{merchant}/documents/{document}", s.backoffice(s.handleBODownloadDocument))
	s.router.HandleFunc("POST "+backofficePrefix+"merchants/{merchant}/kyb/approve", s.backoffice(s.handleBOApprove))
	s.router.HandleFunc("POST "+backofficePrefix+"merchants/{merchant}/kyb/reject", s.backoffice(s.handleBOReject))
	s.router.HandleFunc("POST "+backofficePrefix+"merchants/{merchant}/block", s.backoffice(s.handleBOBlock(true)))
	s.router.HandleFunc("POST "+backofficePrefix+"merchants/{merchant}/unblock", s.backoffice(s.handleBOBlock(false)))
	s.router.HandleFunc("GET "+backofficePrefix+"merchants/{merchant}/ledger", s.backoffice(s.handleBOLedger))
	s.router.HandleFunc("GET "+backofficePrefix+"payments", s.backoffice(s.handleBOPayments))
	s.router.HandleFunc("GET "+backofficePrefix+"payouts", s.backoffice(s.handleBOPayouts))
	s.router.HandleFunc("POST "+backofficePrefix+"payouts/{clave}/retry", s.backoffice(s.handleBORetryPayout))
	s.router.HandleFunc("GET "+backofficePrefix+"reconciliation", s.backoffice(func(w http.ResponseWriter, r *http.Request, _ string) {
		s.HandleAcquirerReconciliation(w, r)
	}))
	s.router.HandleFunc("POST "+backofficePrefix+"reconciliation/{item}/resolve", s.backoffice(func(w http.ResponseWriter, r *http.Request, _ string) {
		s.HandleResolveReconciliationItem(w, r)
	}))
	s.router.HandleFunc("GET "+backofficePrefix+"audit-log", s.backoffice(s.handleBOAuditLog))
}

func (s *Server) handleBOMe(w http.ResponseWriter, r *http.Request, op string) {
	writeJSON(w, http.StatusOK, map[string]any{"user_id": op, "role": auth.RoleOperator, "scope": auth.BackofficeScope})
}

func boLimit(w http.ResponseWriter, r *http.Request, def, max int) (int, bool) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 || n > max {
		writeError(w, http.StatusBadRequest, "INVALID_LIMIT")
		return 0, false
	}
	return n, true
}

// BOMerchantSummary is one row of the backoffice merchant list.
type BOMerchantSummary struct {
	MerchantID    string     `json:"merchant_id"`
	Name          string     `json:"name"`
	RFC           string     `json:"rfc"`
	KYBStatus     kyb.Status `json:"kyb_status"`
	Reason        string     `json:"reason,omitempty"`
	Blocked       bool       `json:"blocked"`
	IdentityCheck string     `json:"identity_check"`
	List69B       string     `json:"list_69b"`
	SubmittedAt   string     `json:"submitted_at,omitempty"`
	UpdatedAt     string     `json:"updated_at"`
}

func (s *Server) handleBOListMerchants(w http.ResponseWriter, r *http.Request, _ string) {
	limit, ok := boLimit(w, r, 100, 500)
	if !ok {
		return
	}
	q := store.KYBQuery{Limit: limit}
	if v := r.URL.Query().Get("kyb_status"); v != "" {
		if !kyb.Status(v).Valid() {
			writeError(w, http.StatusBadRequest, "INVALID_KYB_STATUS")
			return
		}
		q.Status = kyb.Status(v)
	}
	switch v := r.URL.Query().Get("blocked"); v {
	case "":
	case "true", "false":
		b := v == "true"
		q.Blocked = &b
	default:
		writeError(w, http.StatusBadRequest, "INVALID_BLOCKED_FILTER")
		return
	}
	repo, _ := s.kybStore()
	recs, err := repo.ListKYB(r.Context(), q)
	if err != nil {
		writeKYBStoreError(w, "bo_list", err)
		return
	}
	out := make([]BOMerchantSummary, 0, len(recs))
	for _, rec := range recs {
		m, _ := s.GetMerchant(rec.MerchantID)
		name, rfc := m.Name, m.RFC
		if rec.Application.LegalName != "" {
			name, rfc = rec.Application.LegalName, rec.Application.RFC
		}
		out = append(out, BOMerchantSummary{
			MerchantID: rec.MerchantID, Name: name, RFC: rfc, KYBStatus: rec.Status, Reason: rec.Reason,
			Blocked: rec.Blocked, IdentityCheck: string(rec.IdentityCheck), List69B: string(rec.List69B),
			SubmittedAt: fmtTime(rec.SubmittedAt), UpdatedAt: rec.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// BOKYBDetail is the operator view of an onboarding (includes personal data).
type BOKYBDetail struct {
	Status             kyb.Status      `json:"status"`
	Reason             string          `json:"reason,omitempty"`
	Application        kyb.Application `json:"application"`
	PersonaMoral       bool            `json:"persona_moral"`
	Checks             kyb.Checks      `json:"checks"`
	IdentityCheck      string          `json:"identity_check"`
	IdentityReference  string          `json:"identity_reference,omitempty"`
	IdentityDetail     string          `json:"identity_detail,omitempty"`
	AcquirerMerchantID string          `json:"acquirer_merchant_id,omitempty"`
	SubmittedAt        string          `json:"submitted_at,omitempty"`
	DecidedAt          string          `json:"decided_at,omitempty"`
	DecidedBy          string          `json:"decided_by,omitempty"`
	Blocked            bool            `json:"blocked"`
	BlockedReason      string          `json:"blocked_reason,omitempty"`
	BlockedAt          string          `json:"blocked_at,omitempty"`
	Version            int64           `json:"version"`
}

// BODocument is the metadata of an uploaded document.
type BODocument struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
	SHA256      string `json:"sha256"`
	Status      string `json:"status"`
	UploadedBy  string `json:"uploaded_by,omitempty"`
	CreatedAt   string `json:"created_at"`
}

func boKYB(rec store.KYBRecord) BOKYBDetail {
	return BOKYBDetail{
		Status: rec.Status, Reason: rec.Reason, Application: rec.Application, PersonaMoral: rec.PersonaMoral,
		Checks:        kyb.Checks{RFCValid: rec.RFCValid, List69B: rec.List69B, DocumentsComplete: rec.DocumentsComplete},
		IdentityCheck: string(rec.IdentityCheck), IdentityReference: rec.IdentityReference, IdentityDetail: rec.IdentityDetail,
		AcquirerMerchantID: rec.AcquirerMerchantID, SubmittedAt: fmtTime(rec.SubmittedAt), DecidedAt: fmtTime(rec.DecidedAt),
		DecidedBy: rec.DecidedBy, Blocked: rec.Blocked, BlockedReason: rec.BlockedReason, BlockedAt: fmtTime(rec.BlockedAt),
		Version: rec.Version,
	}
}

// kybStateSummary is what the audit log records before/after a decision (no
// personal data).
type kybStateSummary struct {
	Status             kyb.Status `json:"status"`
	Reason             string     `json:"reason,omitempty"`
	Blocked            bool       `json:"blocked"`
	BlockedReason      string     `json:"blocked_reason,omitempty"`
	AcquirerMerchantID string     `json:"acquirer_merchant_id,omitempty"`
	VirtualCLABE       string     `json:"virtual_clabe,omitempty"`
}

func (s *Server) kybSummary(rec store.KYBRecord) kybStateSummary {
	clabe, _ := s.GetMerchantCLABE(rec.MerchantID)
	return kybStateSummary{Status: rec.Status, Reason: rec.Reason, Blocked: rec.Blocked, BlockedReason: rec.BlockedReason,
		AcquirerMerchantID: rec.AcquirerMerchantID, VirtualCLABE: clabe}
}

func (s *Server) handleBOMerchantDetail(w http.ResponseWriter, r *http.Request, op string) {
	id := r.PathValue("merchant")
	m, found := s.GetMerchant(id)
	if !found {
		writeError(w, http.StatusNotFound, "MERCHANT_NOT_FOUND")
		return
	}
	ctx := r.Context()
	rec, err := s.loadKYB(ctx, id)
	if err != nil {
		writeKYBStoreError(w, "bo_detail", err)
		return
	}
	repo, _ := s.kybStore()
	docs, err := repo.ListKYBDocuments(ctx, id, true)
	if err != nil {
		writeKYBStoreError(w, "bo_detail", err)
		return
	}
	// Personal data access is audited before it is served (fail closed).
	if err := s.audit(ctx, op, "kyb.view", id, "", nil, nil, ""); err != nil {
		writeError(w, http.StatusServiceUnavailable, "AUDIT_UNAVAILABLE")
		return
	}
	outDocs := make([]BODocument, 0, len(docs))
	for _, d := range docs {
		outDocs = append(outDocs, BODocument{ID: d.ID, Type: string(d.Type), ContentType: d.ContentType, SizeBytes: d.SizeBytes,
			SHA256: d.SHA256, Status: d.Status, UploadedBy: d.UploadedBy, CreatedAt: d.CreatedAt.UTC().Format(time.RFC3339)})
	}
	clabe, _ := s.GetMerchantCLABE(id)
	m.CLABE = clabe
	resp := map[string]any{"merchant": m, "kyb": boKYB(rec), "documents": outDocs}
	if acct, found, err := s.merchantRepo.GetSettlementAccount(ctx, id); err == nil && found {
		resp["settlement_account"] = map[string]any{"clabe": acct.CLABE, "status": acct.Status}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleBODownloadDocument(w http.ResponseWriter, r *http.Request, op string) {
	id, docID := r.PathValue("merchant"), r.PathValue("document")
	ctx := r.Context()
	repo, _ := s.kybStore()
	doc, found, err := repo.GetKYBDocument(ctx, id, docID)
	if err != nil {
		writeKYBStoreError(w, "bo_download", err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "DOCUMENT_NOT_FOUND")
		return
	}
	if err := s.audit(ctx, op, "document.download", id, docID, nil, nil, string(doc.Type)); err != nil {
		writeError(w, http.StatusServiceUnavailable, "AUDIT_UNAVAILABLE")
		return
	}
	content, err := s.openKYBDocument(ctx, doc)
	if err != nil {
		slog.Error("kyb document decryption failed", "merchant_id", id, "document_id", docID, "error", err)
		writeError(w, http.StatusInternalServerError, "DOCUMENT_UNREADABLE")
		return
	}
	defer clear(content)
	ext := map[string]string{"image/jpeg": "jpg", "image/png": "png", "application/pdf": "pdf"}[doc.ContentType]
	w.Header().Set("Content-Type", doc.ContentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s_%s.%s"`, doc.Type, doc.ID, ext))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func decodeBOBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(dst); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "INVALID_JSON")
		return false
	}
	return true
}

func writeDecisionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrKYBNotFound):
		writeError(w, http.StatusNotFound, "KYB_NOT_FOUND")
	case errors.Is(err, ErrKYBInvalidTransition):
		writeError(w, http.StatusConflict, "KYB_INVALID_TRANSITION")
	case errors.Is(err, ErrKYBListed69B):
		writeError(w, http.StatusConflict, kyb.ReasonListed69B)
	case errors.Is(err, store.ErrKYBConflict):
		writeError(w, http.StatusConflict, "KYB_CONCURRENT_UPDATE")
	case errors.Is(err, acquirer.ErrProviderUnavailable), errors.Is(err, acquirer.ErrProviderRejected):
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "ACQUIRER_ONBOARDING_FAILED", "detail": err.Error()})
	default:
		slog.Error("kyb decision failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "KYB_DECISION_FAILED")
	}
}

func (s *Server) handleBOApprove(w http.ResponseWriter, r *http.Request, op string) {
	var body struct {
		Note string `json:"note"`
	}
	if !decodeBOBody(w, r, &body) {
		return
	}
	id := r.PathValue("merchant")
	if !s.requireNotMerchantMember(w, r, op, id) {
		return
	}
	before, found, err := s.MerchantKYB(r.Context(), id)
	if err == nil && !found {
		err = ErrKYBNotFound
	}
	if err != nil {
		writeDecisionError(w, err)
		return
	}
	beforeSum := s.kybSummary(before)
	rec, err := s.ApproveMerchantKYB(r.Context(), id, op)
	if err != nil {
		writeDecisionError(w, err)
		return
	}
	_ = s.audit(r.Context(), op, "kyb.approve", id, "", beforeSum, s.kybSummary(rec), body.Note)
	writeJSON(w, http.StatusOK, map[string]any{"merchant_id": id, "kyb": boKYB(rec)})
}

func (s *Server) handleBOReject(w http.ResponseWriter, r *http.Request, op string) {
	var body struct {
		Reason string `json:"reason"`
	}
	if !decodeBOBody(w, r, &body) {
		return
	}
	body.Reason = strings.TrimSpace(body.Reason)
	if body.Reason == "" || len(body.Reason) > 1000 {
		writeError(w, http.StatusBadRequest, "REASON_REQUIRED")
		return
	}
	id := r.PathValue("merchant")
	before, found, err := s.MerchantKYB(r.Context(), id)
	if err == nil && !found {
		err = ErrKYBNotFound
	}
	if err != nil {
		writeDecisionError(w, err)
		return
	}
	beforeSum := s.kybSummary(before)
	rec, err := s.RejectMerchantKYB(r.Context(), id, op, body.Reason)
	if err != nil {
		writeDecisionError(w, err)
		return
	}
	_ = s.audit(r.Context(), op, "kyb.reject", id, "", beforeSum, s.kybSummary(rec), body.Reason)
	writeJSON(w, http.StatusOK, map[string]any{"merchant_id": id, "kyb": boKYB(rec)})
}

// handleBOBlock blocks (every money operation answers 403 MERCHANT_BLOCKED)
// or unblocks a merchant, whatever its onboarding status.
func (s *Server) handleBOBlock(block bool) func(http.ResponseWriter, *http.Request, string) {
	return func(w http.ResponseWriter, r *http.Request, op string) {
		var body struct {
			Reason string `json:"reason"`
		}
		if !decodeBOBody(w, r, &body) {
			return
		}
		body.Reason = strings.TrimSpace(body.Reason)
		if block && (body.Reason == "" || len(body.Reason) > 1000) {
			writeError(w, http.StatusBadRequest, "REASON_REQUIRED")
			return
		}
		id := r.PathValue("merchant")
		if _, found := s.GetMerchant(id); !found {
			writeError(w, http.StatusNotFound, "MERCHANT_NOT_FOUND")
			return
		}
		if !block && !s.requireNotMerchantMember(w, r, op, id) {
			return
		}
		ctx := r.Context()
		rec, err := s.loadKYB(ctx, id)
		if err != nil {
			writeKYBStoreError(w, "bo_block", err)
			return
		}
		if rec.Blocked == block {
			if block {
				writeError(w, http.StatusConflict, "ALREADY_BLOCKED")
			} else {
				writeError(w, http.StatusConflict, "NOT_BLOCKED")
			}
			return
		}
		beforeSum := s.kybSummary(rec)
		rec.Blocked = block
		if block {
			now := nowUTC()
			rec.BlockedReason, rec.BlockedAt = body.Reason, &now
		} else {
			rec.BlockedReason, rec.BlockedAt = "", nil
		}
		repo, _ := s.kybStore()
		if rec, err = repo.UpdateKYB(ctx, rec); err != nil {
			writeKYBStoreError(w, "bo_block", err)
			return
		}
		action := "merchant.unblock"
		if block {
			action = "merchant.block"
			slog.Warn("merchant blocked by operator", "merchant_id", id, "operator", op)
		}
		_ = s.audit(ctx, op, action, id, "", beforeSum, s.kybSummary(rec), body.Reason)
		writeJSON(w, http.StatusOK, map[string]any{"merchant_id": id, "blocked": rec.Blocked, "blocked_reason": rec.BlockedReason})
	}
}

// BOJournalLine is one ledger entry of the merchant's accounts.
type BOJournalLine struct {
	JournalID   string            `json:"journal_id"`
	Description string            `json:"description"`
	AccountID   string            `json:"account_id"`
	Direction   string            `json:"direction"`
	AmountCents int64             `json:"amount_cents"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	CreatedAt   string            `json:"created_at"`
}

func (s *Server) handleBOLedger(w http.ResponseWriter, r *http.Request, op string) {
	id := r.PathValue("merchant")
	if _, found := s.GetMerchant(id); !found {
		writeError(w, http.StatusNotFound, "MERCHANT_NOT_FOUND")
		return
	}
	limit, ok := boLimit(w, r, 100, 500)
	if !ok {
		return
	}
	accounts := map[string]string{
		"available_cents":  fmt.Sprintf("LIABILITY:MERCHANT:%s:AVAILABLE", id),
		"tax_escrow_cents": fmt.Sprintf("LIABILITY:MERCHANT:%s:TAX_ESCROW", id),
		"rbf_debt_cents":   fmt.Sprintf("ASSET:RBF_LOANS_RECEIVABLE:%s", id),
	}
	balances := map[string]int64{}
	codes := make([]string, 0, len(accounts))
	for k, code := range accounts {
		codes = append(codes, code)
		if bal, err := s.ledger.GetBalance(code); err == nil {
			balances[k] = bal.AmountCents
		}
	}
	reader, ok := s.moneyStore().(store.JournalReader)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "LEDGER_UNAVAILABLE")
		return
	}
	lines, err := reader.ListAccountEntries(r.Context(), codes, limit)
	if err != nil {
		slog.Error("ledger listing failed", "merchant_id", id, "error", err)
		writeError(w, http.StatusServiceUnavailable, "LEDGER_UNAVAILABLE")
		return
	}
	out := make([]BOJournalLine, 0, len(lines))
	for _, l := range lines {
		out = append(out, BOJournalLine{JournalID: l.JournalID, Description: l.Description, AccountID: l.AccountID,
			Direction: string(l.Direction), AmountCents: l.AmountCents, Metadata: l.Metadata, CreatedAt: l.CreatedAt.UTC().Format(time.RFC3339)})
	}
	_ = s.audit(r.Context(), op, "ledger.view", id, "", nil, nil, "")
	writeJSON(w, http.StatusOK, map[string]any{"merchant_id": id, "balances": balances, "entries": out})
}

// parseBODate accepts RFC 3339 or YYYY-MM-DD (start of that day in
// America/Mexico_City).
func parseBODate(v string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, true
	}
	loc, err := time.LoadLocation("America/Mexico_City")
	if err != nil {
		loc = time.UTC
	}
	if t, err := time.ParseInLocation("2006-01-02", v, loc); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// BOSale is a payment in the cross-merchant search.
type BOSale struct {
	SaleItem
	MerchantID string `json:"merchant_id"`
	Reference  string `json:"reference,omitempty"`
}

func (s *Server) handleBOPayments(w http.ResponseWriter, r *http.Request, _ string) {
	limit, cursor, ok := parseSalesPage(w, r, 500)
	if !ok {
		return
	}
	qs := r.URL.Query()
	q := store.SalesQuery{MerchantID: qs.Get("merchant_id"), ID: qs.Get("id"), Kind: qs.Get("kind"), Before: cursor, Limit: limit}
	switch q.Kind {
	case "", store.SaleCard, store.SaleSPEI, store.SaleCoDi:
	default:
		writeError(w, http.StatusBadRequest, "INVALID_KIND")
		return
	}
	for name, dst := range map[string]*time.Time{"from": &q.From, "to": &q.To} {
		if v := qs.Get(name); v != "" {
			t, ok := parseBODate(v)
			if !ok {
				writeError(w, http.StatusBadRequest, "INVALID_DATE")
				return
			}
			*dst = t
		}
	}
	recs, next, err := s.listSalesPage(r, q)
	if err != nil {
		slog.Error("backoffice payment search failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "SALES_UNAVAILABLE")
		return
	}
	items := make([]BOSale, 0, len(recs))
	for _, rec := range recs {
		items = append(items, BOSale{SaleItem: saleItem(rec), MerchantID: rec.MerchantID, Reference: rec.Reference})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}

// BOPayout is a payout in the backoffice listing.
type BOPayout struct {
	ClaveRastreo     string `json:"clave_rastreo"`
	MerchantID       string `json:"merchant_id"`
	AmountCents      int64  `json:"amount_cents"`
	DestinationCLABE string `json:"destination_clabe"`
	Status           string `json:"status"`
	STPOrderID       int64  `json:"stp_order_id,omitempty"`
	LastError        string `json:"last_error,omitempty"`
	FechaOperacion   string `json:"fecha_operacion"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
}

func boPayout(p store.PayoutRecord) BOPayout {
	return BOPayout{ClaveRastreo: p.ClaveRastreo, MerchantID: p.MerchantID, AmountCents: p.AmountCents, DestinationCLABE: p.DestinationCLABE,
		Status: string(p.Status), STPOrderID: p.STPOrderID, LastError: p.LastError, FechaOperacion: p.FechaOperacion,
		CreatedAt: p.CreatedAt.UTC().Format(time.RFC3339), UpdatedAt: p.UpdatedAt.UTC().Format(time.RFC3339)}
}

func (s *Server) payoutQueries(w http.ResponseWriter) (store.PayoutQueryRepository, bool) {
	q, ok := s.moneyStore().(store.PayoutQueryRepository)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "PAYOUTS_UNAVAILABLE")
	}
	return q, ok
}

func (s *Server) handleBOPayouts(w http.ResponseWriter, r *http.Request, _ string) {
	limit, ok := boLimit(w, r, 100, 500)
	if !ok {
		return
	}
	repo, ok := s.payoutQueries(w)
	if !ok {
		return
	}
	q := store.PayoutQuery{MerchantID: r.URL.Query().Get("merchant_id"), Status: store.PayoutStatus(r.URL.Query().Get("status")), Limit: limit}
	list, err := repo.ListPayouts(r.Context(), q)
	if err != nil {
		slog.Error("backoffice payout listing failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "PAYOUTS_UNAVAILABLE")
		return
	}
	out := make([]BOPayout, 0, len(list))
	for _, p := range list {
		out = append(out, boPayout(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// handleBORetryPayout runs the payout reconciler on one payout stuck in
// PENDING_RECONCILIATION (or RESERVED older than the reconciler's minimum
// age, so an in-flight registraOrden is never raced).
func (s *Server) handleBORetryPayout(w http.ResponseWriter, r *http.Request, op string) {
	repo, ok := s.payoutQueries(w)
	if !ok {
		return
	}
	clave := r.PathValue("clave")
	ctx := r.Context()
	p, found, err := repo.GetPayout(ctx, clave)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "PAYOUTS_UNAVAILABLE")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "PAYOUT_NOT_FOUND")
		return
	}
	stale := p.Status == store.PayoutReserved && time.Since(p.UpdatedAt) >= payouts.DefaultReconcileAfter
	if p.Status != store.PayoutPendingReconciliation && !stale {
		writeError(w, http.StatusConflict, "PAYOUT_NOT_RETRYABLE")
		return
	}
	outcome := payouts.NewReconciler(nil, s.moneyStore(), s, payouts.ReconcilerConfig{}).ReconcilePayout(ctx, p)
	after, _, _ := repo.GetPayout(ctx, clave)
	_ = s.audit(ctx, op, "payout.retry", p.MerchantID, clave,
		map[string]any{"status": p.Status}, map[string]any{"status": after.Status, "last_error": after.LastError}, string(outcome))
	writeJSON(w, http.StatusOK, map[string]any{"outcome": outcome, "payout": boPayout(after)})
}

// BOAuditEntry is one operator_audit_log row.
type BOAuditEntry struct {
	ID         int64           `json:"id"`
	OperatorID string          `json:"operator_id"`
	Action     string          `json:"action"`
	MerchantID string          `json:"merchant_id,omitempty"`
	Target     string          `json:"target,omitempty"`
	Before     json.RawMessage `json:"before,omitempty"`
	After      json.RawMessage `json:"after,omitempty"`
	Detail     string          `json:"detail,omitempty"`
	CreatedAt  string          `json:"created_at"`
}

func (s *Server) handleBOAuditLog(w http.ResponseWriter, r *http.Request, _ string) {
	limit, ok := boLimit(w, r, 100, 500)
	if !ok {
		return
	}
	rows, err := s.auditLog().ListAudit(r.Context(), store.AuditQuery{MerchantID: r.URL.Query().Get("merchant_id"), Action: r.URL.Query().Get("action"), Limit: limit})
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "AUDIT_UNAVAILABLE")
		return
	}
	out := make([]BOAuditEntry, 0, len(rows))
	for _, e := range rows {
		out = append(out, BOAuditEntry{ID: e.ID, OperatorID: e.OperatorID, Action: e.Action, MerchantID: e.MerchantID, Target: e.Target,
			Before: e.Before, After: e.After, Detail: e.Detail, CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}
