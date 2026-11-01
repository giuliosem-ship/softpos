package iso8583

import (
	"bytes"
	"errors"
	"testing"
)

func TestCryptogram_ARQCVerificationSuccess(t *testing.T) {
	// 16-byte AES or 2TDEA session key
	sessionKey := hexToBytes("0123456789ABCDEFFEDCBA9876543210")
	// Concatenated EMV transaction elements (Amount, Currency, TVR, Unpredictable Number, etc.)
	txData := hexToBytes("000000100000000000000000048400000000000484261031001234567819800042")

	arqc, err := ComputeARQC(sessionKey, txData)
	if err != nil {
		t.Fatalf("ComputeARQC failed: %v", err)
	}

	if len(arqc) != 8 {
		t.Errorf("expected 8-byte ARQC, got %d bytes", len(arqc))
	}

	valid, err := VerifyARQC(sessionKey, txData, arqc)
	if err != nil {
		t.Fatalf("VerifyARQC returned error: %v", err)
	}
	if !valid {
		t.Errorf("expected ARQC verification to succeed")
	}
}

func TestCryptogram_ARQCTamperedDataFails(t *testing.T) {
	sessionKey := hexToBytes("0123456789ABCDEFFEDCBA9876543210")
	txData := hexToBytes("000000100000000000000000048400000000000484261031001234567819800042")

	arqc, _ := ComputeARQC(sessionKey, txData)

	// Tamper transaction data (alter amount by 1 cent)
	tamperedTxData := make([]byte, len(txData))
	copy(tamperedTxData, txData)
	tamperedTxData[5] = 0x01

	valid, err := VerifyARQC(sessionKey, tamperedTxData, arqc)
	if err != nil {
		t.Fatalf("VerifyARQC returned error: %v", err)
	}
	if valid {
		t.Errorf("expected tampered data to fail ARQC verification")
	}
}

func TestCryptogram_GenerateARPC(t *testing.T) {
	sessionKey := hexToBytes("0123456789ABCDEFFEDCBA9876543210")
	arqc := hexToBytes("A1B2C3D4E5F60718")

	arpcApproved, err := GenerateARPC(sessionKey, arqc, "00")
	if err != nil {
		t.Fatalf("GenerateARPC failed: %v", err)
	}
	if len(arpcApproved) != 8 {
		t.Errorf("expected 8-byte ARPC, got %d bytes", len(arpcApproved))
	}

	arpcDeclined, err := GenerateARPC(sessionKey, arqc, "05")
	if err != nil {
		t.Fatalf("GenerateARPC failed: %v", err)
	}

	if bytes.Equal(arpcApproved, arpcDeclined) {
		t.Errorf("ARPC for '00' and '05' should not be identical")
	}
}

func TestCryptogram_InvalidInputsRaiseErrors(t *testing.T) {
	// Wrong key size
	badKey := hexToBytes("01020304")
	_, err := ComputeARQC(badKey, []byte("data"))
	if !errors.Is(err, ErrInvalidKeyLength) {
		t.Errorf("expected ErrInvalidKeyLength, got: %v", err)
	}

	// Empty data
	goodKey := hexToBytes("0123456789ABCDEFFEDCBA9876543210")
	_, errEmpty := ComputeARQC(goodKey, nil)
	if !errors.Is(errEmpty, ErrInvalidTransactionData) {
		t.Errorf("expected ErrInvalidTransactionData, got: %v", errEmpty)
	}

	// Invalid ARC length
	_, errARC := GenerateARPC(goodKey, hexToBytes("A1B2C3D4E5F60718"), "0")
	if !errors.Is(errARC, ErrInvalidResponseCode) {
		t.Errorf("expected ErrInvalidResponseCode, got: %v", errARC)
	}
}
