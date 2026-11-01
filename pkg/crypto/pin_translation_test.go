package crypto

import (
	"bytes"
	"crypto/des"
	"testing"
)

func TestTranslatePinBlock_Success(t *testing.T) {
	bdk := decodeHexOrPanic("0123456789ABCDEFFEDCBA9876543210")
	ksn := decodeHexOrPanic("FFFF9876543210E00005")
	zpk := decodeHexOrPanic("FEDCBA98765432100123456789ABCDEF")
	pan := "4000123456789010"
	pin := "2580"

	// 1. Format plaintext ISO 9564-1 Format 0 PIN block
	plainPinBlock, err := FormatIso0PinBlock(pin, pan)
	if err != nil {
		t.Fatalf("FormatIso0PinBlock failed: %v", err)
	}
	if len(plainPinBlock) != 8 {
		t.Fatalf("Expected 8-byte PIN block, got %d", len(plainPinBlock))
	}

	// 2. Encrypt under terminal PEK (simulating SoftPOS terminal output)
	incomingBlock, err := EncryptPinBlock(plainPinBlock, ksn, bdk)
	if err != nil {
		t.Fatalf("EncryptPinBlock failed: %v", err)
	}

	// 3. Translate PIN block from terminal (PEK) to switch (ZPK)
	translatedBlock, err := TranslatePinBlock(incomingBlock, ksn, bdk, zpk, pan)
	if err != nil {
		t.Fatalf("TranslatePinBlock failed: %v", err)
	}
	if len(translatedBlock) != 8 {
		t.Fatalf("Expected 8-byte translated block, got %d", len(translatedBlock))
	}
	if bytes.Equal(translatedBlock, incomingBlock) {
		t.Fatal("Translated block must be re-encrypted under ZPK and differ from incoming block")
	}

	// 4. Simulate Bank Switch (PROSA/E-Global) decrypting under ZPK
	tripleZPK := append(zpk, zpk[:8]...)
	cipherZPK, err := des.NewTripleDESCipher(tripleZPK)
	if err != nil {
		t.Fatalf("Failed to initialize switch cipher: %v", err)
	}
	switchDecrypted := make([]byte, 8)
	cipherZPK.Decrypt(switchDecrypted, translatedBlock)

	if !bytes.Equal(switchDecrypted, plainPinBlock) {
		t.Fatalf("Switch decrypted PIN block does not match original plain block.\nGot:  %X\nWant: %X", switchDecrypted, plainPinBlock)
	}

	// 5. Verify original PIN extraction at switch
	extractedPin, err := ExtractPinFromIso0(switchDecrypted, pan)
	if err != nil {
		t.Fatalf("ExtractPinFromIso0 failed: %v", err)
	}
	if extractedPin != pin {
		t.Fatalf("Extracted PIN mismatch. Got: %s, Want: %s", extractedPin, pin)
	}
}

func TestTranslatePinBlock_InvalidInputs(t *testing.T) {
	validBDK := decodeHexOrPanic("0123456789ABCDEFFEDCBA9876543210")
	validKSN := decodeHexOrPanic("FFFF9876543210E00005")
	validZPK := decodeHexOrPanic("FEDCBA98765432100123456789ABCDEF")
	validBlock := make([]byte, 8)
	validPAN := "4000123456789010"

	// Short incoming block
	if _, err := TranslatePinBlock(validBlock[:7], validKSN, validBDK, validZPK, validPAN); err != ErrInvalidBlockLength {
		t.Errorf("Expected ErrInvalidBlockLength, got: %v", err)
	}

	// Short KSN
	if _, err := TranslatePinBlock(validBlock, validKSN[:9], validBDK, validZPK, validPAN); err != ErrInvalidKSNLength {
		t.Errorf("Expected ErrInvalidKSNLength, got: %v", err)
	}

	// Short BDK
	if _, err := TranslatePinBlock(validBlock, validKSN, validBDK[:15], validZPK, validPAN); err != ErrInvalidBDKLength {
		t.Errorf("Expected ErrInvalidBDKLength, got: %v", err)
	}

	// Short ZPK
	if _, err := TranslatePinBlock(validBlock, validKSN, validBDK, validZPK[:15], validPAN); err != ErrInvalidZPKLength {
		t.Errorf("Expected ErrInvalidZPKLength, got: %v", err)
	}

	// Invalid PAN (too short)
	if _, err := TranslatePinBlock(validBlock, validKSN, validBDK, validZPK, "123456"); err != ErrInvalidPAN {
		t.Errorf("Expected ErrInvalidPAN for short PAN, got: %v", err)
	}

	// Invalid PAN (non-numeric)
	if _, err := TranslatePinBlock(validBlock, validKSN, validBDK, validZPK, "400012345678901X"); err != ErrInvalidPAN {
		t.Errorf("Expected ErrInvalidPAN for non-numeric PAN, got: %v", err)
	}
}

func TestFormatAndExtractIso0_EdgeCases(t *testing.T) {
	pan := "4929000000006" // 13-digit PAN
	pin := "123456"         // 6-digit PIN

	block, err := FormatIso0PinBlock(pin, pan)
	if err != nil {
		t.Fatalf("FormatIso0PinBlock error: %v", err)
	}

	extracted, err := ExtractPinFromIso0(block, pan)
	if err != nil {
		t.Fatalf("ExtractPinFromIso0 error: %v", err)
	}
	if extracted != pin {
		t.Errorf("PIN extraction mismatch: got %s, want %s", extracted, pin)
	}

	// PIN too short (< 4)
	if _, err := FormatIso0PinBlock("123", pan); err == nil {
		t.Error("Expected error for 3-digit PIN, got nil")
	}

	// PIN too long (> 12)
	if _, err := FormatIso0PinBlock("1234567890123", pan); err == nil {
		t.Error("Expected error for 13-digit PIN, got nil")
	}
}
