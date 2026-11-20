package kyb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

// IdentityResult is the outcome of an identity verification.
type IdentityResult string

const (
	IdentityPassed  IdentityResult = "PASSED"
	IdentityFailed  IdentityResult = "FAILED"
	IdentityReview  IdentityResult = "REVIEW"  // provider could not decide
	IdentityUnknown IdentityResult = "UNKNOWN" // not run / provider unavailable
)

// ErrKYCProviderUnavailable means the verification outcome is unknown; the
// application still goes to manual review with IdentityUnknown.
var ErrKYCProviderUnavailable = errors.New("kyc provider unavailable")

// IdentityCheckRequest carries what a provider needs for a liveness + INE
// check. Document bytes are plaintext only in memory, for this call.
type IdentityCheckRequest struct {
	MerchantID string
	LegalName  string
	CURP       string
	INEFront   []byte
	INEBack    []byte
	Selfie     []byte
}

// IdentityCheck is the provider's verdict.
type IdentityCheck struct {
	Result    IdentityResult
	Liveness  bool   // selfie is a live person
	FaceMatch bool   // selfie matches the INE photo
	INEValid  bool   // INE validated (e.g. against the INE list nominal)
	Reference string // provider verification id (for the operator)
	Detail    string
}

// KYCProvider is the port to an identity-verification provider (Truora,
// Metamap, ...). Only MockKYCProvider exists today (docs/KYB.md).
type KYCProvider interface {
	Name() string
	VerifyIdentity(ctx context.Context, req IdentityCheckRequest) (IdentityCheck, error)
}

// MockKYCProvider is the deterministic provider of tests and DEV_MODE: it
// passes unless the selfie content starts with "FAIL" (failed liveness) or
// "REVIEW", and reports unavailable when it starts with "TIMEOUT".
type MockKYCProvider struct{}

var _ KYCProvider = MockKYCProvider{}

func (MockKYCProvider) Name() string { return "mock" }

func (MockKYCProvider) VerifyIdentity(_ context.Context, req IdentityCheckRequest) (IdentityCheck, error) {
	sum := sha256.Sum256(append(append(append([]byte(req.MerchantID), req.INEFront...), req.INEBack...), req.Selfie...))
	ref := "mockkyc_" + hex.EncodeToString(sum[:8])
	switch {
	case len(req.Selfie) == 0 || len(req.INEFront) == 0 || len(req.INEBack) == 0:
		return IdentityCheck{Result: IdentityFailed, Reference: ref, Detail: "missing INE or selfie"}, nil
	case bytes.HasPrefix(req.Selfie, []byte("TIMEOUT")):
		return IdentityCheck{}, ErrKYCProviderUnavailable
	case bytes.HasPrefix(req.Selfie, []byte("FAIL")):
		return IdentityCheck{Result: IdentityFailed, INEValid: true, Reference: ref, Detail: "liveness failed"}, nil
	case bytes.HasPrefix(req.Selfie, []byte("REVIEW")):
		return IdentityCheck{Result: IdentityReview, Liveness: true, INEValid: true, Reference: ref, Detail: "low face-match score"}, nil
	}
	return IdentityCheck{Result: IdentityPassed, Liveness: true, FaceMatch: true, INEValid: true, Reference: ref}, nil
}
