package payments_test

import (
	"encoding/hex"
	"errors"
	"testing"

	"softpos/pkg/payments"
)

func TestMaskPAN(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "Standard 16-digit Visa",
			input:    "4152313333339012",
			expected: "415231******9012",
		},
		{
			name:     "15-digit Amex",
			input:    "378282246310005",
			expected: "378282*****0005",
		},
		{
			name:     "Short string less than 10 chars",
			input:    "123456",
			expected: "******",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := payments.MaskPAN(tt.input)
			if got != tt.expected {
				t.Errorf("MaskPAN(%q) = %q; want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestParseTLV(t *testing.T) {
	// Construct sample BER-TLV payload:
	// Tag 82 (AIP, 2 bytes): 0x01, 0x20
	// Tag 9F02 (Amount, 6 bytes): 0x00, 0x00, 0x00, 0x01, 0x50, 0x00 ($150.00)
	// Tag 5F2A (Currency, 2 bytes): 0x04, 0x84 (MXN)
	tlvHex := "82020120" + "9F0206000000015000" + "5F2A020484"
	data, err := hex.DecodeString(tlvHex)
	if err != nil {
		t.Fatalf("failed to decode hex: %v", err)
	}

	tags, err := payments.ParseTLV(data)
	if err != nil {
		t.Fatalf("ParseTLV failed: %v", err)
	}

	if aip, ok := tags["82"]; !ok || hex.EncodeToString(aip) != "0120" {
		t.Errorf("Tag 82 mismatch: got %x, want 0120", aip)
	}
	if amt, ok := tags["9F02"]; !ok || hex.EncodeToString(amt) != "000000015000" {
		t.Errorf("Tag 9F02 mismatch: got %x, want 000000015000", amt)
	}
	if curr, ok := tags["5F2A"]; !ok || hex.EncodeToString(curr) != "0484" {
		t.Errorf("Tag 5F2A mismatch: got %x, want 0484", curr)
	}

	// Malformed truncated TLV
	_, err = payments.ParseTLV(data[:len(data)-1])
	if !errors.Is(err, payments.ErrInvalidTLV) {
		t.Errorf("expected ErrInvalidTLV for truncated stream, got %v", err)
	}
}

func TestParseEMVTags_ValidAndRejections(t *testing.T) {
	// Valid EMV Contactless NFC stream:
	// 9F02 (Amount): 000000025000 ($250.00 MXN)
	// 5F2A (Currency): 0484 (MXN)
	// 9F27 (CID): 80 (ARQC)
	// 9F26 (AC): AABBCCDDEEFF0011 (8 bytes)
	// 9F36 (ATC): 0014 (20)
	// 5A   (PAN): 4152313333339012 (8 bytes BCD)
	// 5F24 (ExpDate): 2812 (Dec 2028)
	validHex := "9F0206000000025000" +
		"5F2A020484" +
		"9F270180" +
		"9F2608AABBCCDDEEFF0011" +
		"9F36020014" +
		"5A084152313333339012" +
		"5F24022812"

	raw, err := hex.DecodeString(validHex)
	if err != nil {
		t.Fatalf("hex decode failed: %v", err)
	}

	emv, err := payments.ParseEMVTags(raw)
	if err != nil {
		t.Fatalf("ParseEMVTags failed: %v", err)
	}

	if emv.AmountCents != 25000 {
		t.Errorf("AmountCents = %d; want 25000", emv.AmountCents)
	}
	if emv.CurrencyCode != "0484" {
		t.Errorf("CurrencyCode = %s; want 0484", emv.CurrencyCode)
	}
	if emv.CryptogramType != payments.CryptogramARQC {
		t.Errorf("CryptogramType = %s; want ARQC", emv.CryptogramType)
	}
	if emv.CryptogramHex != "aabbccddeeff0011" && emv.CryptogramHex != "AABBCCDDEEFF0011" {
		t.Errorf("CryptogramHex mismatch: %s", emv.CryptogramHex)
	}
	if emv.ATC != 20 {
		t.Errorf("ATC = %d; want 20", emv.ATC)
	}
	if emv.PANMasked != "415231******9012" {
		t.Errorf("PANMasked = %s; want 415231******9012", emv.PANMasked)
	}

	// Rejection 1: AAC Cryptogram (Transaction Declined by Card)
	aacHex := "9F0206000000025000" + "5F2A020484" + "9F270100" + "5F24022812"
	rawAAC, _ := hex.DecodeString(aacHex)
	_, err = payments.ParseEMVTags(rawAAC)
	if !errors.Is(err, payments.ErrUnsupportedCryptogram) {
		t.Errorf("expected ErrUnsupportedCryptogram on AAC, got %v", err)
	}

	// Rejection 2: Foreign Currency (e.g. USD 0840)
	usdHex := "9F0206000000025000" + "5F2A020840" + "9F270180" + "5F24022812"
	rawUSD, _ := hex.DecodeString(usdHex)
	_, err = payments.ParseEMVTags(rawUSD)
	if !errors.Is(err, payments.ErrInvalidCurrency) {
		t.Errorf("expected ErrInvalidCurrency on USD, got %v", err)
	}

	// Rejection 3: Expired Card (e.g. 2101 -> Jan 2021)
	expiredHex := "9F0206000000025000" + "5F2A020484" + "9F270180" + "5F24022101"
	rawExp, _ := hex.DecodeString(expiredHex)
	_, err = payments.ParseEMVTags(rawExp)
	if !errors.Is(err, payments.ErrExpiredCard) {
		t.Errorf("expected ErrExpiredCard on 2101, got %v", err)
	}
}

func TestPaymentEngine_Lifecycle(t *testing.T) {
	engine := payments.NewPaymentEngine()

	emv := payments.EMVTags{
		AmountCents:    50000, // $500.00 MXN
		CurrencyCode:   "0484",
		CryptogramType: payments.CryptogramTC,
		CryptogramHex:  "1122334455667788",
		ATC:            15,
		PANMasked:      "415231******9012",
		ExpirationDate: "2912",
	}

	// 1. Authorize
	payment, err := engine.Authorize("pay_001", "merchant_123", emv)
	if err != nil {
		t.Fatalf("Authorize failed: %v", err)
	}
	if payment.Status != payments.PaymentAuthorized {
		t.Errorf("expected AUTHORIZED status, got %s", payment.Status)
	}

	// Double authorization with same ID must fail
	_, err = engine.Authorize("pay_001", "merchant_123", emv)
	if err == nil {
		t.Error("expected error for duplicate payment ID")
	}

	// 2. Settle
	settled, err := engine.Settle("pay_001")
	if err != nil {
		t.Fatalf("Settle failed: %v", err)
	}
	if settled.Status != payments.PaymentSettled {
		t.Errorf("expected SETTLED status, got %s", settled.Status)
	}

	// Voiding an already settled payment must fail
	_, err = engine.Void("pay_001")
	if !errors.Is(err, payments.ErrInvalidStateTransition) {
		t.Errorf("expected ErrInvalidStateTransition on voiding settled payment, got %v", err)
	}

	// 3. Refund
	refunded, err := engine.Refund("pay_001", 50000)
	if err != nil {
		t.Fatalf("Refund failed: %v", err)
	}
	if refunded.Status != payments.PaymentRefunded {
		t.Errorf("expected REFUNDED status, got %s", refunded.Status)
	}

	// Refund exceeding amount must fail
	_, err = engine.Refund("pay_001", 60000)
	if !errors.Is(err, payments.ErrInvalidAmount) && !errors.Is(err, payments.ErrInvalidStateTransition) {
		t.Errorf("expected ErrInvalidAmount/ErrInvalidStateTransition, got %v", err)
	}

	// 4. Voiding an authorized payment flow
	payVoid, err := engine.Authorize("pay_002", "merchant_123", emv)
	if err != nil {
		t.Fatalf("Authorize pay_002 failed: %v", err)
	}
	voided, err := engine.Void(payVoid.ID)
	if err != nil {
		t.Fatalf("Void failed: %v", err)
	}
	if voided.Status != payments.PaymentVoided {
		t.Errorf("expected VOIDED status, got %s", voided.Status)
	}

	// Settling a voided payment must fail
	_, err = engine.Settle("pay_002")
	if !errors.Is(err, payments.ErrInvalidStateTransition) {
		t.Errorf("expected ErrInvalidStateTransition settling voided payment, got %v", err)
	}
}
