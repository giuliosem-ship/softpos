package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"softpos/pkg/acquirer"
	"softpos/pkg/auth"
	"softpos/pkg/identity"
	"softpos/pkg/kyb"
	"softpos/pkg/store"
)

// Merchant onboarding (KYB/KYC), MVP-17. See docs/KYB.md.

var (
	// ErrKYBInvalidTransition is returned when a decision does not apply to
	// the merchant's current onboarding status.
	ErrKYBInvalidTransition = errors.New("kyb status does not allow this transition")
	// ErrKYBNotFound is returned when the merchant has no onboarding record.
	ErrKYBNotFound = errors.New("kyb record not found")
	// ErrKYBListed69B is returned when approving an onboarding whose RFC the
	// 69-B lookup found listed (a submission interrupted before the
	// automatic rejection).
	ErrKYBListed69B = errors.New("kyb RFC is listed in SAT 69-B")
)

// System actors recorded in decided_by.
const (
	kybActorProvisioning = "system:provisioning"
	kybActor69B          = "system:69b"
)

// kycCheckTimeout bounds the identity provider call made at submission.
const kycCheckTimeout = 20 * time.Second

// SetKYBRepository replaces the in-memory onboarding store (SQL with
// DATABASE_URL). blobs stores the encrypted document envelopes.
func (s *Server) SetKYBRepository(repo store.KYBRepository, blobs store.DocumentBlobStore) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kybRepo, s.docBlobs = repo, blobs
}

// SetKYCProvider installs the identity verification provider (nil: identity
// checks stay UNKNOWN and the operator verifies the documents manually).
func (s *Server) SetKYCProvider(p kyb.KYCProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kycProvider = p
}

func (s *Server) kybStore() (store.KYBRepository, store.DocumentBlobStore) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.kybRepo, s.docBlobs
}

func (s *Server) documentVault() *identity.DocumentVault {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.docVault
}

// MerchantKYB returns a merchant's onboarding record (tests / backoffice).
func (s *Server) MerchantKYB(ctx context.Context, merchantID string) (store.KYBRecord, bool, error) {
	repo, _ := s.kybStore()
	return repo.GetKYB(ctx, merchantID)
}

// loadKYB returns the merchant's onboarding record, creating a DRAFT one for
// merchants that predate onboarding (migration 012).
func (s *Server) loadKYB(ctx context.Context, merchantID string) (store.KYBRecord, error) {
	repo, _ := s.kybStore()
	rec, found, err := repo.GetKYB(ctx, merchantID)
	if err != nil || found {
		return rec, err
	}
	if _, err := repo.CreateKYB(ctx, store.KYBRecord{MerchantID: merchantID, Status: kyb.StatusDraft}); err != nil {
		return store.KYBRecord{}, err
	}
	rec, found, err = repo.GetKYB(ctx, merchantID)
	if err == nil && !found {
		err = ErrKYBNotFound
	}
	return rec, err
}

// merchantActive reports whether the merchant may move money: onboarding
// APPROVED and not blocked. code is the 403 error code otherwise.
func (s *Server) merchantActive(ctx context.Context, merchantID string) (ok bool, code string, err error) {
	repo, _ := s.kybStore()
	rec, found, err := repo.GetKYB(ctx, merchantID)
	if err != nil {
		return false, "", err
	}
	switch {
	case !found || rec.Status != kyb.StatusApproved:
		return false, "MERCHANT_NOT_APPROVED", nil
	case rec.Blocked:
		return false, "MERCHANT_BLOCKED", nil
	}
	return true, "", nil
}

// requireActiveMerchant gates every money operation (card intents and
// refunds, CoDi/SPEI QR, payouts, legacy EMV): 403 MERCHANT_NOT_APPROVED
// until the onboarding is approved, 403 MERCHANT_BLOCKED while blocked.
func (s *Server) requireActiveMerchant(w http.ResponseWriter, r *http.Request, merchantID string) bool {
	ok, code, err := s.merchantActive(r.Context(), merchantID)
	if err != nil {
		slog.Error("merchant status lookup failed", "merchant_id", merchantID, "error", err)
		writeError(w, http.StatusServiceUnavailable, "KYB_UNAVAILABLE")
		return false
	}
	if !ok {
		writeError(w, http.StatusForbidden, code)
		return false
	}
	return true
}

// requireActiveTenant is requireTenant + requireActiveMerchant.
func (s *Server) requireActiveTenant(w http.ResponseWriter, r *http.Request, claimed string) (string, bool) {
	tenant, ok := requireTenant(w, r, claimed)
	if !ok || !s.requireActiveMerchant(w, r, tenant) {
		return "", false
	}
	return tenant, true
}

// customerPersonaMoral is the "buyer is a persona moral" input of the RESICO
// split (Art. 113-J LISR ISR withholding). It is decided by the server, never
// by a request body: until CFDI receptor data (F8) identifies a persona moral
// buyer, every sale of an approved merchant (RESICO persona física only) is
// treated as a sale to the general public.
func (s *Server) customerPersonaMoral(context.Context, string) bool { return false }

// KYBResponse is the owner-facing onboarding view (GET/PUT/submit).
type KYBResponse struct {
	Status            kyb.Status         `json:"status"`
	Reason            string             `json:"reason,omitempty"`
	Checks            kyb.Checks         `json:"checks"`
	RequiredDocuments []kyb.DocumentType `json:"required_documents"`
	UploadedDocuments []kyb.DocumentType `json:"uploaded_documents"`
	SubmittedAt       string             `json:"submitted_at,omitempty"`
	DecidedAt         string             `json:"decided_at,omitempty"`
	Application       *kyb.Application   `json:"application,omitempty"` // owner only
	Error             string             `json:"error,omitempty"`       // set on 422
}

func fmtTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func docTypes(docs []store.KYBDocument) []kyb.DocumentType {
	seen := map[kyb.DocumentType]bool{}
	out := []kyb.DocumentType{}
	for _, d := range docs {
		if !seen[d.Type] {
			seen[d.Type] = true
			out = append(out, d.Type)
		}
	}
	return out
}

func (s *Server) kybResponse(ctx context.Context, rec store.KYBRecord, withApp bool) (KYBResponse, error) {
	repo, _ := s.kybStore()
	docs, err := repo.ListKYBDocuments(ctx, rec.MerchantID, false)
	if err != nil {
		return KYBResponse{}, err
	}
	uploaded := docTypes(docs)
	checks := kyb.Checks{RFCValid: rec.RFCValid, List69B: rec.List69B, DocumentsComplete: rec.DocumentsComplete}
	if rec.Status.Editable() { // live feedback while the owner fills the application
		checks.RFCValid = identity.ValidateRFCWithCheckDigit(rec.Application.RFC)
		checks.DocumentsComplete = len(kyb.MissingDocuments(uploaded)) == 0
	}
	if checks.List69B == "" {
		checks.List69B = kyb.List69BUnknown
	}
	out := KYBResponse{
		Status: rec.Status, Reason: rec.Reason, Checks: checks,
		RequiredDocuments: kyb.RequiredDocuments(), UploadedDocuments: uploaded,
		SubmittedAt: fmtTime(rec.SubmittedAt), DecidedAt: fmtTime(rec.DecidedAt),
	}
	if withApp {
		app := rec.Application
		out.Application = &app
	}
	return out, nil
}

func (s *Server) writeKYB(w http.ResponseWriter, r *http.Request, status int, rec store.KYBRecord, errCode string) {
	resp, err := s.kybResponse(r.Context(), rec, RoleFromContext(r.Context()) == auth.RoleOwner)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "KYB_UNAVAILABLE")
		return
	}
	resp.Error = errCode
	writeJSON(w, status, resp)
}

func writeKYBStoreError(w http.ResponseWriter, op string, err error) {
	if errors.Is(err, store.ErrKYBConflict) {
		writeError(w, http.StatusConflict, "KYB_CONCURRENT_UPDATE")
		return
	}
	slog.Error("kyb operation failed", "op", op, "error", err)
	writeError(w, http.StatusServiceUnavailable, "KYB_UNAVAILABLE")
}

// HandleGetKYB: GET /api/v1/merchants/{id}/kyb (any member; the application
// data is only included for the owner).
func (s *Server) HandleGetKYB(w http.ResponseWriter, r *http.Request) {
	tenant, ok := requireTenant(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	rec, err := s.loadKYB(r.Context(), tenant)
	if err != nil {
		writeKYBStoreError(w, "get", err)
		return
	}
	s.writeKYB(w, r, http.StatusOK, rec, "")
}

// HandlePutKYB: PUT /api/v1/merchants/{id}/kyb (owner). Stores the
// application while it is editable (DRAFT / REJECTED -> DRAFT).
func (s *Server) HandlePutKYB(w http.ResponseWriter, r *http.Request) {
	tenant, ok := requireTenant(w, r, r.PathValue("id"))
	if !ok || !requireRole(w, r, auth.RoleOwner) {
		return
	}
	var app kyb.Application
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&app); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON")
		return
	}
	app = app.Normalize()
	if bad := app.Validate(); len(bad) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "INVALID_KYB_DATA", "fields": bad})
		return
	}
	ctx := r.Context()
	rec, err := s.loadKYB(ctx, tenant)
	if err != nil {
		writeKYBStoreError(w, "put", err)
		return
	}
	if !rec.Status.Editable() {
		writeError(w, http.StatusConflict, "KYB_NOT_EDITABLE")
		return
	}
	rec.Application = app
	rec.Status, rec.Reason = kyb.StatusDraft, ""
	rec.PersonaMoral = kyb.PersonaMoral(app.RFC)
	rec.RFCValid = identity.ValidateRFCWithCheckDigit(app.RFC)
	rec.List69B = kyb.List69BUnknown // re-checked at submission (the RFC may have changed)
	repo, _ := s.kybStore()
	if rec, err = repo.UpdateKYB(ctx, rec); err != nil {
		writeKYBStoreError(w, "put", err)
		return
	}
	s.writeKYB(w, r, http.StatusOK, rec, "")
}

// documentLabel binds a document envelope to its merchant and id (AAD).
func documentLabel(merchantID, documentID string) string { return merchantID + "/" + documentID }

// HandleUploadKYBDocument: POST /api/v1/merchants/{id}/kyb/documents (owner,
// multipart: type, file). The content type is sniffed (JPEG, PNG or PDF),
// the size bounded by kyb.MaxDocumentBytes, and the file encrypted with the
// vault key provider before it reaches blob storage.
func (s *Server) HandleUploadKYBDocument(w http.ResponseWriter, r *http.Request) {
	tenant, ok := requireTenant(w, r, r.PathValue("id"))
	if !ok || !requireRole(w, r, auth.RoleOwner) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, kyb.MaxDocumentBytes+(64<<10))
	if err := r.ParseMultipartForm(kyb.MaxDocumentBytes + (64 << 10)); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "DOCUMENT_TOO_LARGE")
			return
		}
		writeError(w, http.StatusBadRequest, "INVALID_MULTIPART")
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	docType := r.FormValue("type")
	if !kyb.ValidDocumentType(docType) {
		writeError(w, http.StatusBadRequest, "INVALID_DOCUMENT_TYPE")
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "MISSING_FILE")
		return
	}
	content, err := io.ReadAll(io.LimitReader(f, kyb.MaxDocumentBytes+1))
	_ = f.Close()
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_MULTIPART")
		return
	}
	defer clear(content) // personal data: do not leave the plaintext around
	if len(content) == 0 {
		writeError(w, http.StatusBadRequest, "EMPTY_FILE")
		return
	}
	if len(content) > kyb.MaxDocumentBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "DOCUMENT_TOO_LARGE")
		return
	}
	contentType := http.DetectContentType(content)
	if !kyb.AllowedContentTypes[contentType] {
		writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_DOCUMENT_FORMAT")
		return
	}

	ctx := r.Context()
	rec, err := s.loadKYB(ctx, tenant)
	if err != nil {
		writeKYBStoreError(w, "upload", err)
		return
	}
	if !rec.Status.Editable() {
		writeError(w, http.StatusConflict, "KYB_NOT_EDITABLE")
		return
	}
	docID, err := newCardID("kdoc_")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ID_GENERATION_FAILED")
		return
	}
	label := documentLabel(tenant, docID)
	envelope, err := s.documentVault().Seal(ctx, label, content)
	if err != nil {
		slog.Error("kyb document encryption failed", "merchant_id", tenant, "error", err)
		writeError(w, http.StatusInternalServerError, "DOCUMENT_ENCRYPTION_FAILED")
		return
	}
	repo, blobs := s.kybStore()
	if err := blobs.PutBlob(ctx, label, envelope); err != nil {
		writeKYBStoreError(w, "upload_blob", err)
		return
	}
	sum := sha256.Sum256(content)
	user, _ := UserIDFromContext(ctx)
	if err := repo.AddKYBDocument(ctx, store.KYBDocument{
		ID: docID, MerchantID: tenant, Type: kyb.DocumentType(docType), ContentType: contentType,
		SizeBytes: int64(len(content)), SHA256: hex.EncodeToString(sum[:]), BlobKey: label, UploadedBy: user,
	}); err != nil {
		writeKYBStoreError(w, "upload_meta", err)
		return
	}
	if rec.Status == kyb.StatusRejected { // a rejected application is being fixed
		rec.Status, rec.Reason = kyb.StatusDraft, ""
		if _, err := repo.UpdateKYB(ctx, rec); err != nil && !errors.Is(err, store.ErrKYBConflict) {
			slog.Error("kyb reopen after upload failed", "merchant_id", tenant, "error", err)
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{"document_id": docID, "type": docType, "status": store.KYBDocReceived})
}

// openKYBDocument decrypts a stored document.
func (s *Server) openKYBDocument(ctx context.Context, doc store.KYBDocument) ([]byte, error) {
	_, blobs := s.kybStore()
	env, found, err := blobs.GetBlob(ctx, doc.BlobKey)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("kyb blob %s missing", doc.BlobKey)
	}
	return s.documentVault().Open(ctx, documentLabel(doc.MerchantID, doc.ID), env)
}

// HandleSubmitKYB: POST /api/v1/merchants/{id}/kyb/submit (owner). Runs the
// automatic checks: application complete, RFC format + verification digit,
// persona física (derived from the RFC length), RESICO PF regime, CURP
// consistent with the RFC, documents complete (failures: 422, status stays
// DRAFT with the reason); then SUBMITTED, SAT 69-B lookup (listed ->
// REJECTED), identity verification through the KYCProvider and IN_REVIEW
// for an operator.
func (s *Server) HandleSubmitKYB(w http.ResponseWriter, r *http.Request) {
	tenant, ok := requireTenant(w, r, r.PathValue("id"))
	if !ok || !requireRole(w, r, auth.RoleOwner) {
		return
	}
	ctx := r.Context()
	rec, err := s.loadKYB(ctx, tenant)
	if err != nil {
		writeKYBStoreError(w, "submit", err)
		return
	}
	if !rec.Status.Editable() {
		writeError(w, http.StatusConflict, "KYB_ALREADY_SUBMITTED")
		return
	}
	repo, _ := s.kybStore()
	docs, err := repo.ListKYBDocuments(ctx, tenant, false)
	if err != nil {
		writeKYBStoreError(w, "submit", err)
		return
	}
	app := rec.Application.Normalize()
	checks, reason := kyb.Precheck(app, docTypes(docs))
	rec.RFCValid, rec.DocumentsComplete, rec.PersonaMoral = checks.RFCValid, checks.DocumentsComplete, kyb.PersonaMoral(app.RFC)
	if reason != "" {
		rec.Status, rec.Reason, rec.List69B = kyb.StatusDraft, reason, kyb.List69BUnknown
		if rec, err = repo.UpdateKYB(ctx, rec); err != nil {
			writeKYBStoreError(w, "submit", err)
			return
		}
		s.writeKYB(w, r, http.StatusUnprocessableEntity, rec, "KYB_CHECKS_FAILED")
		return
	}

	now := nowUTC()
	rec.Status, rec.Reason, rec.SubmittedAt, rec.DecidedAt, rec.DecidedBy = kyb.StatusSubmitted, "", &now, nil, ""
	rec.List69B = kyb.List69BUnknown
	if s.blacklist != nil {
		rec.List69B = kyb.Classify69B(s.blacklist.GetStatus(app.RFC))
	}
	if rec, err = repo.UpdateKYB(ctx, rec); err != nil {
		writeKYBStoreError(w, "submit", err)
		return
	}
	if rec.List69B == kyb.List69BListed {
		decided := nowUTC()
		rec.Status, rec.Reason, rec.DecidedAt, rec.DecidedBy = kyb.StatusRejected, kyb.ReasonListed69B, &decided, kybActor69B
		if rec, err = repo.UpdateKYB(ctx, rec); err != nil {
			writeKYBStoreError(w, "submit", err)
			return
		}
		slog.Warn("kyb rejected: RFC listed in SAT 69-B", "merchant_id", tenant)
		s.writeKYB(w, r, http.StatusOK, rec, "")
		return
	}

	res := s.verifyIdentity(ctx, rec, docs)
	rec.IdentityCheck, rec.IdentityReference, rec.IdentityDetail = res.Result, res.Reference, res.Detail
	rec.Status = kyb.StatusInReview
	if rec, err = repo.UpdateKYB(ctx, rec); err != nil {
		writeKYBStoreError(w, "submit", err)
		return
	}
	s.writeKYB(w, r, http.StatusOK, rec, "")
}

// verifyIdentity runs the KYC provider over the latest INE and selfie. Any
// failure to run it yields IdentityUnknown (the operator reviews manually).
func (s *Server) verifyIdentity(ctx context.Context, rec store.KYBRecord, docs []store.KYBDocument) kyb.IdentityCheck {
	s.mu.RLock()
	prov := s.kycProvider
	s.mu.RUnlock()
	if prov == nil {
		return kyb.IdentityCheck{Result: kyb.IdentityUnknown, Detail: "no identity provider configured"}
	}
	latest := map[kyb.DocumentType]store.KYBDocument{}
	for _, d := range docs { // oldest first: the last one wins
		latest[d.Type] = d
	}
	req := kyb.IdentityCheckRequest{MerchantID: rec.MerchantID, LegalName: rec.Application.LegalName, CURP: rec.Application.CURP}
	for t, dst := range map[kyb.DocumentType]*[]byte{kyb.DocINEFront: &req.INEFront, kyb.DocINEBack: &req.INEBack, kyb.DocSelfie: &req.Selfie} {
		content, err := s.openKYBDocument(ctx, latest[t])
		if err != nil {
			slog.Error("kyb document decryption failed", "merchant_id", rec.MerchantID, "type", t, "error", err)
			return kyb.IdentityCheck{Result: kyb.IdentityUnknown, Detail: "document unreadable: " + string(t)}
		}
		*dst = content
	}
	defer func() { clear(req.INEFront); clear(req.INEBack); clear(req.Selfie) }()
	cctx, cancel := context.WithTimeout(ctx, kycCheckTimeout)
	defer cancel()
	res, err := prov.VerifyIdentity(cctx, req)
	if err != nil {
		slog.Warn("kyc provider unavailable; manual review", "merchant_id", rec.MerchantID, "provider", prov.Name(), "error", err)
		return kyb.IdentityCheck{Result: kyb.IdentityUnknown, Detail: "provider unavailable"}
	}
	return res
}

// ApproveMerchantKYB approves an onboarding in review: assigns the virtual
// collection CLABE if the merchant has none, onboards it as a sub-merchant at
// the processor (idempotent; an error aborts the approval so it can be
// retried) and records the decision. Every step before the final status write
// is idempotent.
func (s *Server) ApproveMerchantKYB(ctx context.Context, merchantID, operatorID string) (store.KYBRecord, error) {
	repo, _ := s.kybStore()
	rec, found, err := repo.GetKYB(ctx, merchantID)
	if err != nil {
		return store.KYBRecord{}, err
	}
	if !found {
		return store.KYBRecord{}, ErrKYBNotFound
	}
	if rec.Status != kyb.StatusInReview && rec.Status != kyb.StatusSubmitted {
		return rec, ErrKYBInvalidTransition
	}
	if rec.List69B == kyb.List69BListed {
		return rec, ErrKYBListed69B
	}
	if clabe, found := s.GetMerchantCLABE(merchantID); !found || clabe == "" {
		if _, err := s.AssignVirtualCLABE(ctx, merchantID); err != nil {
			return rec, fmt.Errorf("assign virtual CLABE: %w", err)
		}
	}
	app := rec.Application
	if prov := s.AcquirerProvider(); prov != nil && rec.AcquirerMerchantID == "" {
		res, err := prov.OnboardMerchant(ctx, acquirer.OnboardRequest{
			IdempotencyKey: "kerno_onboard_" + merchantID, MerchantID: merchantID, LegalName: app.LegalName,
			RFC: app.RFC, MCC: app.MCC, PostalCode: app.Address.PostalCode, ContactEmail: app.ContactEmail, ContactPhone: app.ContactPhone,
		})
		if err != nil {
			return rec, fmt.Errorf("acquirer onboarding: %w", err)
		}
		rec.AcquirerMerchantID = res.ProviderMerchantID
	}
	if m, ok := s.GetMerchant(merchantID); ok && app.RFC != "" && app.LegalName != "" {
		// The approved (verified) identity replaces what the alta declared.
		if err := s.merchantRepo.SaveMerchant(ctx, store.MerchantRecord{ID: m.ID, RFC: app.RFC, Name: app.LegalName, IsRESICO: app.TaxRegime == kyb.RegimeRESICOPF}); err != nil {
			return rec, fmt.Errorf("update merchant profile: %w", err)
		}
	}
	now := nowUTC()
	rec.Status, rec.Reason, rec.DecidedAt, rec.DecidedBy = kyb.StatusApproved, "", &now, operatorID
	return repo.UpdateKYB(ctx, rec)
}

// RejectMerchantKYB rejects an onboarding in review with a reason; the owner
// may fix the application and resubmit.
func (s *Server) RejectMerchantKYB(ctx context.Context, merchantID, operatorID, reason string) (store.KYBRecord, error) {
	repo, _ := s.kybStore()
	rec, found, err := repo.GetKYB(ctx, merchantID)
	if err != nil {
		return store.KYBRecord{}, err
	}
	if !found {
		return store.KYBRecord{}, ErrKYBNotFound
	}
	if rec.Status != kyb.StatusInReview && rec.Status != kyb.StatusSubmitted {
		return rec, ErrKYBInvalidTransition
	}
	now := nowUTC()
	rec.Status, rec.Reason, rec.DecidedAt, rec.DecidedBy = kyb.StatusRejected, reason, &now, operatorID
	return repo.UpdateKYB(ctx, rec)
}

// provisionApprovedKYB records an out-of-band approved onboarding for
// merchants created by trusted server code (RegisterMerchant: seeding, tests,
// migrations of existing merchants), never for the public alta.
func (s *Server) provisionApprovedKYB(ctx context.Context, merchantID, rfc, name string, isRESICO bool) error {
	repo, _ := s.kybStore()
	now := nowUTC()
	regime := kyb.RegimeOther
	if isRESICO {
		regime = kyb.RegimeRESICOPF
	}
	_, err := repo.CreateKYB(ctx, store.KYBRecord{
		MerchantID: merchantID, Status: kyb.StatusApproved,
		Application:  kyb.Application{LegalName: name, RFC: rfc, TaxRegime: regime},
		PersonaMoral: kyb.PersonaMoral(rfc), RFCValid: identity.ValidateRFCWithCheckDigit(rfc), List69B: kyb.List69BUnknown,
		DecidedAt: &now, DecidedBy: kybActorProvisioning,
	})
	return err
}

// EnsureMerchantApproved marks an existing merchant's onboarding APPROVED by
// trusted server code (DEV_MODE demo seeding); no-op if already approved.
func (s *Server) EnsureMerchantApproved(ctx context.Context, merchantID string) error {
	m, ok := s.GetMerchant(merchantID)
	if !ok {
		return store.ErrMerchantNotFound
	}
	if err := s.provisionApprovedKYB(ctx, merchantID, m.RFC, m.Name, m.IsRESICO); err != nil {
		return err
	}
	rec, err := s.loadKYB(ctx, merchantID)
	if err != nil || rec.Status == kyb.StatusApproved {
		return err
	}
	now := nowUTC()
	rec.Status, rec.DecidedAt, rec.DecidedBy, rec.Reason = kyb.StatusApproved, &now, kybActorProvisioning, ""
	repo, _ := s.kybStore()
	_, err = repo.UpdateKYB(ctx, rec)
	return err
}
