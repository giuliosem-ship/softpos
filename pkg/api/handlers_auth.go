package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"softpos/pkg/auth"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

// writeAuthError maps auth package errors to HTTP responses.
func writeAuthError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrInvalidPhone):
		writeError(w, http.StatusBadRequest, "INVALID_PHONE")
	case errors.Is(err, auth.ErrOTPInvalid):
		writeError(w, http.StatusUnauthorized, "OTP_INVALID")
	case errors.Is(err, auth.ErrOTPLocked):
		writeError(w, http.StatusTooManyRequests, "OTP_LOCKED")
	case errors.Is(err, auth.ErrOTPDailyLimit):
		writeError(w, http.StatusTooManyRequests, "OTP_DAILY_LIMIT")
	case errors.Is(err, auth.ErrOTPDelivery):
		writeError(w, http.StatusBadGateway, "OTP_DELIVERY_FAILED")
	case errors.Is(err, auth.ErrRefreshTokenReused):
		writeError(w, http.StatusUnauthorized, "REFRESH_TOKEN_REUSED")
	case errors.Is(err, auth.ErrRefreshTokenInvalid):
		writeError(w, http.StatusUnauthorized, "REFRESH_TOKEN_INVALID")
	case errors.Is(err, auth.ErrNotMember):
		writeError(w, http.StatusForbidden, "NOT_A_MERCHANT_MEMBER")
	default:
		slog.Error("auth operation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "AUTH_INTERNAL_ERROR")
	}
}

type OTPRequestBody struct {
	Phone string `json:"phone"`
}

// HandleOTPRequest: POST /api/v1/auth/otp/request {phone}
func (s *Server) HandleOTPRequest(w http.ResponseWriter, r *http.Request) {
	var req OTPRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON")
		return
	}
	wait, err := s.authService().RequestOTP(r.Context(), req.Phone)
	if errors.Is(err, auth.ErrOTPRateLimited) {
		secs := int64(wait/time.Second) + 1
		w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "OTP_RATE_LIMITED", "retry_after_seconds": secs})
		return
	}
	if err != nil {
		writeAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "OTP_SENT"})
}

type OTPVerifyBody struct {
	Phone      string `json:"phone"`
	Code       string `json:"code"`
	MerchantID string `json:"merchant_id,omitempty"`
}

// HandleOTPVerify: POST /api/v1/auth/otp/verify {phone, code, merchant_id?}
func (s *Server) HandleOTPVerify(w http.ResponseWriter, r *http.Request) {
	var req OTPVerifyBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON")
		return
	}
	tp, err := s.authService().VerifyOTP(r.Context(), req.Phone, req.Code, req.MerchantID)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.withMerchantNames(tp))
}

// withMerchantNames fills each membership's merchant_name (MVP-25: the
// app's merchant picker shows names, not ids). Lookup failures leave it empty.
func (s *Server) withMerchantNames(tp auth.TokenPair) auth.TokenPair {
	for i := range tp.Memberships {
		if m, ok := s.GetMerchant(tp.Memberships[i].MerchantID); ok {
			tp.Memberships[i].MerchantName = m.Name
		}
	}
	return tp
}

type RefreshBody struct {
	RefreshToken string `json:"refresh_token"`
	MerchantID   string `json:"merchant_id,omitempty"`
}

// HandleRefresh: POST /api/v1/auth/refresh {refresh_token, merchant_id?}
func (s *Server) HandleRefresh(w http.ResponseWriter, r *http.Request) {
	var req RefreshBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON")
		return
	}
	tp, err := s.authService().Refresh(r.Context(), req.RefreshToken, req.MerchantID)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.withMerchantNames(tp))
}

// HandleLogout: POST /api/v1/auth/logout {refresh_token} revokes the session family.
func (s *Server) HandleLogout(w http.ResponseWriter, r *http.Request) {
	var req RefreshBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON")
		return
	}
	if err := s.authService().Logout(r.Context(), req.RefreshToken); err != nil {
		writeAuthError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type AttestationNonceResponse struct {
	Nonce      string `json:"nonce"`
	IssuedAtMs int64  `json:"issued_at_ms"`
	TTLSeconds int64  `json:"ttl_seconds"`
}

// HandleAttestationNonce: GET|POST /api/v1/auth/attestation-nonce. Shape matches
// the Android AttestationNonce(nonce, issuedAtTimestampMs, ttlSeconds).
func (s *Server) HandleAttestationNonce(w http.ResponseWriter, r *http.Request) {
	nonce, issuedAt, ttl, err := s.authService().IssueAttestationNonce(r.Context())
	if err != nil {
		writeAuthError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, AttestationNonceResponse{
		Nonce:      nonce,
		IssuedAtMs: issuedAt.UnixMilli(),
		TTLSeconds: int64(ttl / time.Second),
	})
}

type AttestationVerifyRequest struct {
	Nonce          string `json:"nonce"`
	IntegrityToken string `json:"integrity_token"`
}

// HandleAttestationVerify: POST /api/v1/auth/attestation/verify. The nonce is
// burned before the token is checked, so every nonce allows one attempt.
func (s *Server) HandleAttestationVerify(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	verifier := s.integrityVerifier
	s.mu.RUnlock()
	if verifier == nil {
		writeError(w, http.StatusServiceUnavailable, "ATTESTATION_NOT_CONFIGURED")
		return
	}
	var req AttestationVerifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON")
		return
	}
	if req.IntegrityToken == "" {
		req.IntegrityToken = r.Header.Get("X-Play-Integrity-Token")
	}
	if err := s.authService().ConsumeAttestationNonce(r.Context(), req.Nonce); err != nil {
		if errors.Is(err, auth.ErrAttestationNonceInvalid) {
			writeError(w, http.StatusUnauthorized, "ATTESTATION_NONCE_INVALID")
			return
		}
		writeAuthError(w, err)
		return
	}
	verdict, err := verifier.VerifyToken(req.IntegrityToken, req.Nonce)
	if err != nil {
		slog.Warn("play integrity verification failed", "error", err)
		writeError(w, http.StatusUnauthorized, "ATTESTATION_FAILED")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":                     "ATTESTED",
		"package_name":               verdict.PackageName,
		"device_recognition_verdict": verdict.DeviceRecognitionVerdict,
	})
}

// devSessionPhone is the synthetic user the DEV_MODE demo session logs in as.
const devSessionPhone = "+520000000001"

type DevSessionRequest struct {
	MerchantID string `json:"merchant_id"`
}

// HandleDevSession: POST /api/v1/auth/dev-session (DEV_MODE only, 404
// otherwise). Gives the Web POS demo an owner token for an existing merchant.
func (s *Server) HandleDevSession(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	dev := s.devMode
	s.mu.RUnlock()
	if !dev {
		http.NotFound(w, r)
		return
	}
	var req DevSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.MerchantID == "" {
		writeError(w, http.StatusBadRequest, "MISSING_MERCHANT_ID")
		return
	}
	if _, exists := s.GetMerchant(req.MerchantID); !exists {
		writeError(w, http.StatusNotFound, "MERCHANT_NOT_FOUND")
		return
	}
	svc := s.authService()
	user, err := svc.EnsureUser(r.Context(), devSessionPhone)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	if err := svc.AddMembership(r.Context(), user.ID, req.MerchantID, auth.RoleOwner); err != nil {
		writeAuthError(w, err)
		return
	}
	token, err := svc.MintAccessToken(user.ID, req.MerchantID, auth.RoleOwner)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	slog.Warn("DEV_MODE demo session issued", "merchant_id", req.MerchantID)
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": token,
		"token_type":   "Bearer",
		"expires_in":   int64(svc.AccessTTL() / time.Second),
		"merchant_id":  req.MerchantID,
		"role":         auth.RoleOwner,
	})
}
