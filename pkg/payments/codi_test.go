package payments_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"softpos/pkg/payments"
)

func TestGenerateCoDiString_Success(t *testing.T) {
	// Valid STP CLABE: 646180123400000007 (bank 646, plaza 180, seq 12340000000)
	// Control digit for 64618012340000000 is 8 (checked via stp package)
	validCLABE := "646180123400000007"

	req := payments.CoDiRequest{
		Version:         1,
		SellerCLABE:     validCLABE,
		AmountCents:     12550, // $125.50 MXN
		Concept:         "Cafe y dona",
		Reference:       "REF102938",
		Expiration:      time.Now().Add(15 * time.Minute),
		InstitutionCode: "646",
	}

	codiStr, err := payments.GenerateCoDiString(req)
	if err != nil {
		t.Fatalf("GenerateCoDiString failed: %v", err)
	}

	if !strings.Contains(codiStr, validCLABE) {
		t.Errorf("expected string to contain CLABE, got: %s", codiStr)
	}
	if !strings.Contains(codiStr, "125.50") {
		t.Errorf("expected formatted amount 125.50, got: %s", codiStr)
	}
	if !strings.Contains(codiStr, "REF102938") {
		t.Errorf("expected reference, got: %s", codiStr)
	}

	// Parse back
	parsed, err := payments.ParseCoDiString(codiStr)
	if err != nil {
		t.Fatalf("ParseCoDiString failed: %v", err)
	}
	if parsed.SellerCLABE != validCLABE {
		t.Errorf("parsed CLABE mismatch: got %s, want %s", parsed.SellerCLABE, validCLABE)
	}
	if parsed.AmountCents != 12550 {
		t.Errorf("parsed AmountCents mismatch: got %d, want 12550", parsed.AmountCents)
	}
}

func TestGenerateCoDiString_Validations(t *testing.T) {
	validCLABE := "646180123400000007"

	// 1. Invalid / Negative Amount
	_, err := payments.GenerateCoDiString(payments.CoDiRequest{
		SellerCLABE: validCLABE,
		AmountCents: 0,
		Expiration:  time.Now().Add(10 * time.Minute),
	})
	if !errors.Is(err, payments.ErrInvalidCoDiAmount) {
		t.Errorf("expected ErrInvalidCoDiAmount on 0 amount, got %v", err)
	}

	// 2. Invalid CLABE checksum
	_, err = payments.GenerateCoDiString(payments.CoDiRequest{
		SellerCLABE: "646180123400000009", // Bad checksum digit
		AmountCents: 1000,
		Expiration:  time.Now().Add(10 * time.Minute),
	})
	if !errors.Is(err, payments.ErrInvalidCLABE) {
		t.Errorf("expected ErrInvalidCLABE on bad checksum, got %v", err)
	}

	// 3. Expired request
	_, err = payments.GenerateCoDiString(payments.CoDiRequest{
		SellerCLABE: validCLABE,
		AmountCents: 1000,
		Expiration:  time.Now().Add(-5 * time.Minute),
	})
	if !errors.Is(err, payments.ErrExpiredCoDiRequest) {
		t.Errorf("expected ErrExpiredCoDiRequest on past expiration, got %v", err)
	}
}

func TestValidateCoDiConfirmation(t *testing.T) {
	validJSON := `{
		"tracking_key": "BNX20260919012345",
		"seller_clabe": "646180123400000007",
		"buyer_clabe": "012180000118359719",
		"amount_cents": 12550,
		"timestamp": "2026-09-19T10:00:00Z",
		"status": "APROBADO",
		"banxico_approval_code": "AUT893120"
	}`

	conf, err := payments.ValidateCoDiConfirmation([]byte(validJSON))
	if err != nil {
		t.Fatalf("ValidateCoDiConfirmation failed: %v", err)
	}
	if conf.TrackingKey != "BNX20260919012345" {
		t.Errorf("TrackingKey mismatch: %s", conf.TrackingKey)
	}
	if conf.Status != "APROBADO" {
		t.Errorf("Status mismatch: %s", conf.Status)
	}

	// Rejection: Rejected status
	rejectedJSON := `{
		"tracking_key": "BNX20260919012346",
		"status": "RECHAZADO"
	}`
	_, err = payments.ValidateCoDiConfirmation([]byte(rejectedJSON))
	if !errors.Is(err, payments.ErrInvalidCoDiPayload) {
		t.Errorf("expected ErrInvalidCoDiPayload for non-approved status, got %v", err)
	}
}
