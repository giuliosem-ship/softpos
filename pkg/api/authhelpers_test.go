package api

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"softpos/pkg/auth"
	"softpos/pkg/payments"
)

// authorizeTestPayment registers an AUTHORIZED card payment directly in the
// payment engine (settle now requires a real authorized payment).
func authorizeTestPayment(t *testing.T, s *Server, merchantID, paymentID string, amountCents int64) {
	t.Helper()
	if _, err := s.paymentEngine.Authorize(paymentID, merchantID, payments.EMVTags{
		AmountCents:    amountCents,
		CurrencyCode:   "0484",
		CryptogramType: payments.CryptogramARQC,
		PANMasked:      "415231******9012",
	}); err != nil {
		t.Fatalf("authorize %s: %v", paymentID, err)
	}
}

// asTenant signs req with an owner access token scoped to merchantID.
func asTenant(t *testing.T, s *Server, merchantID string, req *http.Request) *http.Request {
	t.Helper()
	return asRole(t, s, merchantID, auth.RoleOwner, req)
}

func asRole(t *testing.T, s *Server, merchantID string, role auth.Role, req *http.Request) *http.Request {
	t.Helper()
	tok, err := s.AuthService().MintAccessToken("usr_test_"+merchantID, merchantID, role)
	if err != nil {
		t.Fatalf("MintAccessToken: %v", err)
	}
	return withBearer(req, tok)
}

func withBearer(req *http.Request, token string) *http.Request {
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

var payoutKeySeq atomic.Int64

// withPayoutKey gives every POST /api/v1/disbursements test request a unique
// Idempotency-Key (required since MVP-12); tests of idempotency itself set
// their own key afterwards.
func withPayoutKey(req *http.Request) *http.Request {
	if req.Method == http.MethodPost && req.URL.Path == "/api/v1/disbursements" && req.Header.Get("Idempotency-Key") == "" {
		req.Header.Set("Idempotency-Key", fmt.Sprintf("test-payout-%d", payoutKeySeq.Add(1)))
	}
	return req
}
