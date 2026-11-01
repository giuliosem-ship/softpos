package crypto

import (
	"context"
	"crypto/rand"
	"errors"
	"sync"

	"softpos/pkg/iso8583"
)

var (
	ErrHSMUnavailable = errors.New("payment HSM connection unavailable")
	ErrInvalidRequest = errors.New("invalid HSM request payload")
)

// PaymentHSM defines the core cryptographic contract for banking HSM operations
// including Zone PIN Key generation, PIN translation, and EMV ARQC/ARPC processing.
type PaymentHSM interface {
	GenerateZPK() ([]byte, error)
	TranslatePIN(ctx context.Context, req PINTranslationRequest) (*PINTranslationResponse, error)
	VerifyARQC(ctx context.Context, req ARQCVerifyRequest) (bool, error)
	GenerateARPC(ctx context.Context, req ARPCGenerateRequest) ([]byte, error)
}

// PINTranslationRequest encapsulates parameters for terminal-to-switch PIN translation.
type PINTranslationRequest struct {
	IncomingPinBlock []byte
	KSN              []byte
	BDK              []byte
	ZPK              []byte
	PAN              string
}

// PINTranslationResponse returns the translated PIN block under the switch ZPK.
type PINTranslationResponse struct {
	TranslatedPinBlock []byte
}

// ARQCVerifyRequest contains data needed to verify an EMV online transaction cryptogram.
type ARQCVerifyRequest struct {
	SessionKey      []byte
	TransactionData []byte
	CandidateARQC   []byte
}

// ARPCGenerateRequest contains data needed to generate the issuer authorization cryptogram.
type ARPCGenerateRequest struct {
	SessionKey   []byte
	ARQC         []byte
	ResponseCode string
}

// SoftwareHSMSimulator provides a thread-safe software simulation of a Payment HSM
// for testing and local development, adhering strictly to ANSI X9.24 and ISO 9564.
type SoftwareHSMSimulator struct {
	mu sync.RWMutex
}

// NewSoftwareHSMSimulator initializes a SoftwareHSMSimulator instance.
func NewSoftwareHSMSimulator() *SoftwareHSMSimulator {
	return &SoftwareHSMSimulator{}
}

// GenerateZPK generates a cryptographically secure 16-byte Zone PIN Key.
func (s *SoftwareHSMSimulator) GenerateZPK() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	zpk := make([]byte, 16)
	_, err := rand.Read(zpk)
	if err != nil {
		return nil, err
	}
	return zpk, nil
}

// TranslatePIN performs atomic terminal-to-switch PIN block re-encryption with zeroized buffers.
func (s *SoftwareHSMSimulator) TranslatePIN(ctx context.Context, req PINTranslationRequest) (*PINTranslationResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	translated, err := TranslatePinBlock(req.IncomingPinBlock, req.KSN, req.BDK, req.ZPK, req.PAN)
	if err != nil {
		return nil, err
	}

	return &PINTranslationResponse{
		TranslatedPinBlock: translated,
	}, nil
}

// VerifyARQC verifies an EMV 9F26 application cryptogram using Retail MAC.
func (s *SoftwareHSMSimulator) VerifyARQC(ctx context.Context, req ARQCVerifyRequest) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	return iso8583.VerifyARQC(req.SessionKey, req.TransactionData, req.CandidateARQC)
}

// GenerateARPC generates an EMV Method 1 ARPC response cryptogram.
func (s *SoftwareHSMSimulator) GenerateARPC(ctx context.Context, req ARPCGenerateRequest) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	return iso8583.GenerateARPC(req.SessionKey, req.ARQC, req.ResponseCode)
}
