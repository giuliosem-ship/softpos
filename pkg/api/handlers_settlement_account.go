package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"softpos/pkg/auth"
	"softpos/pkg/store"
	"softpos/pkg/stp"
)

// SettlementAccountVerifier checks that a CLABE belongs to the merchant (bank
// account holder RFC == merchant RFC). The real implementation (STP/bank
// "titularidad" lookup or a KYB provider) is pending; PendingAccountVerifier
// leaves every account PENDING until an operator marks it.
type SettlementAccountVerifier interface {
	VerifyOwnership(ctx context.Context, merchantRFC, clabe string) (store.SettlementAccountStatus, error)
}

// PendingAccountVerifier is the stub verifier: always PENDING.
type PendingAccountVerifier struct{}

func (PendingAccountVerifier) VerifyOwnership(context.Context, string, string) (store.SettlementAccountStatus, error) {
	return store.SettlementPending, nil
}

// SetSettlementAccountVerifier replaces the ownership verifier.
func (s *Server) SetSettlementAccountVerifier(v SettlementAccountVerifier) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accountVerifier = v
}

type SettlementAccountRequest struct {
	CLABE string `json:"clabe"`
}

type SettlementAccountResponse struct {
	MerchantID string                        `json:"merchant_id"`
	CLABE      string                        `json:"clabe"`
	Status     store.SettlementAccountStatus `json:"status"`
	UpdatedAt  string                        `json:"updated_at,omitempty"`
}

func settlementAccountResponse(merchantID string, a store.SettlementAccount) SettlementAccountResponse {
	resp := SettlementAccountResponse{MerchantID: merchantID, CLABE: a.CLABE, Status: a.Status}
	if !a.UpdatedAt.IsZero() {
		resp.UpdatedAt = a.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return resp
}

// HandleGetSettlementAccount: GET /api/v1/merchants/{id}/settlement-account.
func (s *Server) HandleGetSettlementAccount(w http.ResponseWriter, r *http.Request) {
	tenant, ok := requireTenant(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	acct, found, err := s.merchantRepo.GetSettlementAccount(r.Context(), tenant)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "SETTLEMENT_ACCOUNT_UNAVAILABLE")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "SETTLEMENT_ACCOUNT_NOT_REGISTERED")
		return
	}
	writeJSON(w, http.StatusOK, settlementAccountResponse(tenant, acct))
}

// HandlePutSettlementAccount: PUT /api/v1/merchants/{id}/settlement-account
// (owner only). Registers the merchant's own bank CLABE for payouts. Any
// change resets verification (status from the verifier, PENDING by default),
// so a hijacked session cannot redirect payouts to a fresh account.
func (s *Server) HandlePutSettlementAccount(w http.ResponseWriter, r *http.Request) {
	tenant, ok := requireTenant(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if !requireRole(w, r, auth.RoleOwner) {
		return
	}
	var req SettlementAccountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON")
		return
	}
	if !stp.ValidateCLABE(req.CLABE) {
		writeError(w, http.StatusBadRequest, "INVALID_CLABE")
		return
	}
	merchant, exists := s.GetMerchant(tenant)
	if !exists {
		writeError(w, http.StatusNotFound, "MERCHANT_NOT_FOUND")
		return
	}
	// Payouts must leave the platform: a collection (virtual) CLABE of any
	// merchant is not a valid settlement account.
	if _, isCollection := s.GetMerchantByCLABE(req.CLABE); isCollection {
		writeError(w, http.StatusConflict, "SETTLEMENT_CLABE_IS_COLLECTION_CLABE")
		return
	}
	ctx := r.Context()
	if cur, found, err := s.merchantRepo.GetSettlementAccount(ctx, tenant); err == nil && found && cur.CLABE == req.CLABE {
		writeJSON(w, http.StatusOK, settlementAccountResponse(tenant, cur)) // unchanged: keep status
		return
	}
	s.mu.RLock()
	verifier := s.accountVerifier
	s.mu.RUnlock()
	status, err := verifier.VerifyOwnership(ctx, merchant.RFC, req.CLABE)
	if err != nil {
		slog.Warn("settlement account verification failed, leaving PENDING", "merchant_id", tenant, "error", err)
		status = store.SettlementPending
	}
	if err := s.merchantRepo.SetSettlementAccount(ctx, tenant, req.CLABE, status); err != nil {
		writeError(w, http.StatusServiceUnavailable, "SETTLEMENT_ACCOUNT_UPDATE_FAILED")
		return
	}
	acct, _, _ := s.merchantRepo.GetSettlementAccount(ctx, tenant)
	writeJSON(w, http.StatusOK, settlementAccountResponse(tenant, acct))
}

type SettlementAccountReviewRequest struct {
	CLABE  string                        `json:"clabe"`
	Status store.SettlementAccountStatus `json:"status"` // VERIFIED | REJECTED
}

// HandleReviewSettlementAccount: POST /api/v1/merchants/{id}/settlement-account/verify.
// Operator decision on ownership (role operator, or the owner in DEV_MODE for
// the demo). The CLABE in the body must equal the registered one, so a change
// racing the review cannot be approved by accident.
func (s *Server) HandleReviewSettlementAccount(w http.ResponseWriter, r *http.Request) {
	tenant, ok := requireTenant(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	s.mu.RLock()
	dev := s.devMode
	s.mu.RUnlock()
	role := RoleFromContext(r.Context())
	if role != auth.RoleOperator && !(dev && role == auth.RoleOwner) {
		writeError(w, http.StatusForbidden, "ROLE_FORBIDDEN")
		return
	}
	var req SettlementAccountReviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON")
		return
	}
	if req.Status != store.SettlementVerified && req.Status != store.SettlementRejected {
		writeError(w, http.StatusBadRequest, "INVALID_STATUS")
		return
	}
	err := s.merchantRepo.SetSettlementAccountStatus(r.Context(), tenant, req.CLABE, req.Status)
	if errors.Is(err, store.ErrSettlementCLABEChanged) {
		writeError(w, http.StatusConflict, "SETTLEMENT_CLABE_CHANGED")
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "SETTLEMENT_ACCOUNT_UPDATE_FAILED")
		return
	}
	slog.Info("settlement account reviewed", "merchant_id", tenant, "status", req.Status, "role", role)
	s.auditOperatorAction(r, "settlement_account.review", tenant, req.CLABE, nil, map[string]any{"status": req.Status}, string(role))
	acct, _, _ := s.merchantRepo.GetSettlementAccount(r.Context(), tenant)
	writeJSON(w, http.StatusOK, settlementAccountResponse(tenant, acct))
}

// SeedVerifiedSettlementAccount registers clabe as an already VERIFIED
// settlement account (dev/demo seeding and tests only; production accounts go
// through PUT + operator review).
func (s *Server) SeedVerifiedSettlementAccount(ctx context.Context, merchantID, clabe string) error {
	if !stp.ValidateCLABE(clabe) {
		return stp.ErrInvalidCLABE
	}
	return s.merchantRepo.SetSettlementAccount(ctx, merchantID, clabe, store.SettlementVerified)
}
