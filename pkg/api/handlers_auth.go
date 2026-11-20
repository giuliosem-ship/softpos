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
	writeJSON(w, http.StatusOK, tp)
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
	writeJSON(w, http.StatusOK, tp)
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
