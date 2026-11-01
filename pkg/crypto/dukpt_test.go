package crypto

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func decodeHexOrPanic(h string) []byte {
	b, err := hex.DecodeString(h)
	if err != nil {
		panic(err)
	}
	return b
}

func TestDeriveIPEK_OfficialVector(t *testing.T) {
	// ANSI X9.24 standard test vector
	bdk := decodeHexOrPanic("0123456789ABCDEFFEDCBA9876543210")
	ksn := decodeHexOrPanic("FFFF9876543210E00000")
	expectedIPEK := decodeHexOrPanic("6AC292FAA1315B4D858AB3A3D7D5933A")

	ipek, err := DeriveIPEK(bdk, ksn)
	if err != nil {
		t.Fatalf("DeriveIPEK returned unexpected error: %v", err)
	}

	if !bytes.Equal(ipek, expectedIPEK) {
		t.Fatalf("DeriveIPEK mismatch.\nGot:  %X\nWant: %X", ipek, expectedIPEK)
	}
}

func TestDeriveTransactionKey_OfficialVector(t *testing.T) {
	bdk := decodeHexOrPanic("0123456789ABCDEFFEDCBA9876543210")
	ksn := decodeHexOrPanic("FFFF9876543210E00001")
	expectedSessionKey := decodeHexOrPanic("042666B49184CFA368DE9628D0397BC9")
	expectedPEK := decodeHexOrPanic("042666B49184CF5C68DE9628D0397B36")

	ipek, err := DeriveIPEK(bdk, ksn)
	if err != nil {
		t.Fatalf("DeriveIPEK failed: %v", err)
	}

	sessionKey, err := DeriveTransactionKey(ipek, ksn)
	if err != nil {
		t.Fatalf("DeriveTransactionKey failed: %v", err)
	}
	if !bytes.Equal(sessionKey, expectedSessionKey) {
		t.Fatalf("DeriveTransactionKey mismatch.\nGot:  %X\nWant: %X", sessionKey, expectedSessionKey)
	}

	pek, err := DerivePEK(bdk, ksn)
	if err != nil {
		t.Fatalf("DerivePEK failed: %v", err)
	}
	if !bytes.Equal(pek, expectedPEK) {
		t.Fatalf("DerivePEK mismatch.\nGot:  %X\nWant: %X", pek, expectedPEK)
	}
}

func TestExtractTransactionCounter(t *testing.T) {
	testCases := []struct {
		ksnHex          string
		expectedCounter uint32
	}{
		{"FFFF9876543210E00000", 0},
		{"FFFF9876543210E00001", 1},
		{"FFFF9876543210E00002", 2},
		{"FFFF9876543210E00010", 16},
		{"FFFF9876543210E1FFFF", 131071},
	}

	for _, tc := range testCases {
		ksn := decodeHexOrPanic(tc.ksnHex)
		counter, err := ExtractTransactionCounter(ksn)
		if err != nil {
			t.Errorf("ExtractTransactionCounter(%s) error = %v", tc.ksnHex, err)
			continue
		}
		if counter != tc.expectedCounter {
			t.Errorf("ExtractTransactionCounter(%s) = %d, want %d", tc.ksnHex, counter, tc.expectedCounter)
		}
	}
}

func TestDerivePEK_IncrementingCounter(t *testing.T) {
	bdk := decodeHexOrPanic("0123456789ABCDEFFEDCBA9876543210")
	ksn1 := decodeHexOrPanic("FFFF9876543210E00001")
	ksn2 := decodeHexOrPanic("FFFF9876543210E00002")
	ksn3 := decodeHexOrPanic("FFFF9876543210E00003")

	pek1, err := DerivePEK(bdk, ksn1)
	if err != nil {
		t.Fatalf("DerivePEK 1 failed: %v", err)
	}
	pek2, err := DerivePEK(bdk, ksn2)
	if err != nil {
		t.Fatalf("DerivePEK 2 failed: %v", err)
	}
	pek3, err := DerivePEK(bdk, ksn3)
	if err != nil {
		t.Fatalf("DerivePEK 3 failed: %v", err)
	}

	if bytes.Equal(pek1, pek2) || bytes.Equal(pek2, pek3) || bytes.Equal(pek1, pek3) {
		t.Fatal("Derived PEK keys for different transaction counters must be distinct")
	}
}

func TestEncryptAndDecryptPinBlock_RoundTrip(t *testing.T) {
	bdk := decodeHexOrPanic("0123456789ABCDEFFEDCBA9876543210")
	ksn := decodeHexOrPanic("FFFF9876543210E00005")

	// Sample ISO 9564 Format 0 PIN block: 04 12 34 FF FF FF FF FF XOR PAN field
	plainBlock := decodeHexOrPanic("0412A88888FFFFFF")

	encrypted, err := EncryptPinBlock(plainBlock, ksn, bdk)
	if err != nil {
		t.Fatalf("EncryptPinBlock failed: %v", err)
	}
	if len(encrypted) != 8 {
		t.Fatalf("Encrypted PIN block length must be 8, got %d", len(encrypted))
	}
	if bytes.Equal(encrypted, plainBlock) {
		t.Fatal("Encrypted PIN block should not equal plaintext")
	}

	decrypted, err := DecryptPinBlock(encrypted, ksn, bdk)
	if err != nil {
		t.Fatalf("DecryptPinBlock failed: %v", err)
	}
	if !bytes.Equal(decrypted, plainBlock) {
		t.Fatalf("Decrypted PIN block does not match plaintext.\nGot:  %X\nWant: %X", decrypted, plainBlock)
	}
}

func TestInvalidInputs_Rejections(t *testing.T) {
	validBDK := decodeHexOrPanic("0123456789ABCDEFFEDCBA9876543210")
	validKSN := decodeHexOrPanic("FFFF9876543210E00001")
	validBlock := make([]byte, 8)

	// Short BDK
	if _, err := DeriveIPEK(validBDK[:15], validKSN); err != ErrInvalidBDKLength {
		t.Errorf("Expected ErrInvalidBDKLength for short BDK, got: %v", err)
	}

	// Short KSN
	if _, err := DeriveIPEK(validBDK, validKSN[:9]); err != ErrInvalidKSNLength {
		t.Errorf("Expected ErrInvalidKSNLength for short KSN, got: %v", err)
	}

	// Short KSN for counter
	if _, err := ExtractTransactionCounter(validKSN[:8]); err != ErrInvalidKSNLength {
		t.Errorf("Expected ErrInvalidKSNLength for short KSN counter, got: %v", err)
	}

	// Short block for DecryptPinBlock
	if _, err := DecryptPinBlock(validBlock[:7], validKSN, validBDK); err != ErrInvalidBlockLength {
		t.Errorf("Expected ErrInvalidBlockLength for short PIN block, got: %v", err)
	}

	// Short block for EncryptPinBlock
	if _, err := EncryptPinBlock(validBlock[:6], validKSN, validBDK); err != ErrInvalidBlockLength {
		t.Errorf("Expected ErrInvalidBlockLength for short PIN block, got: %v", err)
	}
}

func TestZeroBytes_Sanitization(t *testing.T) {
	buf := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02, 0x03, 0x04}
	ZeroBytes(buf)
	for i, b := range buf {
		if b != 0x00 {
			t.Fatalf("Byte at index %d is not zero: 0x%02X", i, b)
		}
	}
}
