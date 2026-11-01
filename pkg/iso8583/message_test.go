package iso8583

import (
	"testing"
)

func TestISO8583Message_PackUnpack0100AuthRequest(t *testing.T) {
	msg := NewMessage("0100")
	msg.Set(2, "4152313899999012") // LLVAR PAN
	msg.Set(3, "000000")           // Purchase
	msg.Set(4, "000000100000")     // $1,000.00 MXN in cents
	msg.Set(11, "001234")          // STAN
	msg.Set(12, "123045")          // HHMMSS
	msg.Set(13, "1031")            // MMDD
	msg.Set(22, "071")             // Contactless EMV chip
	msg.Set(41, "TERM0001")        // Terminal ID (8 chars)
	msg.Set(42, "MERCH0000000001") // Merchant ID (15 chars)
	msg.Set(49, "484")             // MXN Currency code

	packed, err := msg.Pack()
	if err != nil {
		t.Fatalf("Pack failed: %v", err)
	}

	unpacked, err := UnpackMessage(packed)
	if err != nil {
		t.Fatalf("UnpackMessage failed: %v", err)
	}

	if unpacked.MTI != "0100" {
		t.Errorf("expected MTI 0100, got %q", unpacked.MTI)
	}

	expectedFields := map[int]string{
		2:  "4152313899999012",
		3:  "000000",
		4:  "000000100000",
		11: "001234",
		12: "123045",
		13: "1031",
		22: "071",
		41: "TERM0001",
		42: "MERCH0000000001",
		49: "484",
	}

	for de, expectedVal := range expectedFields {
		val, ok := unpacked.Get(de)
		if !ok {
			t.Errorf("missing expected field DE %d", de)
		} else if val != expectedVal {
			t.Errorf("DE %d: expected %q, got %q", de, expectedVal, val)
		}
	}
}

func TestISO8583Message_PackUnpack0110AuthResponse(t *testing.T) {
	msg := NewMessage("0110")
	msg.Set(3, "000000")
	msg.Set(4, "000000100000")
	msg.Set(11, "001234")
	msg.Set(38, "AUTH88") // Auth identification code
	msg.Set(39, "00")     // Action/Response Code (00 = Approved)

	packed, err := msg.Pack()
	if err != nil {
		t.Fatalf("Pack failed: %v", err)
	}

	unpacked, err := UnpackMessage(packed)
	if err != nil {
		t.Fatalf("UnpackMessage failed: %v", err)
	}

	if unpacked.MTI != "0110" {
		t.Errorf("expected MTI 0110, got %q", unpacked.MTI)
	}

	val38, ok := unpacked.Get(38)
	if !ok || val38 != "AUTH88" {
		t.Errorf("expected DE 38 AUTH88, got %q (ok=%v)", val38, ok)
	}

	val39, ok := unpacked.Get(39)
	if !ok || val39 != "00" {
		t.Errorf("expected DE 39 00, got %q (ok=%v)", val39, ok)
	}
}

func TestISO8583Message_SecondaryBitmapForFieldsAbove64(t *testing.T) {
	msg := NewMessage("0800")
	msg.Set(11, "000001")
	msg.Set(70, "301") // Network Management Information Code (DE > 64)

	packed, err := msg.Pack()
	if err != nil {
		t.Fatalf("Pack failed with secondary bitmap: %v", err)
	}

	unpacked, err := UnpackMessage(packed)
	if err != nil {
		t.Fatalf("UnpackMessage failed: %v", err)
	}

	val70, ok := unpacked.Get(70)
	if !ok || val70 != "301" {
		t.Errorf("expected DE 70 '301', got %q (ok=%v)", val70, ok)
	}
}

func TestISO8583Message_DE55_LLLVAR_ICCData(t *testing.T) {
	msg := NewMessage("0200")
	msg.Set(3, "000000")
	msg.Set(4, "000000050000") // $500.00 MXN
	de55Data := "9F2608A1B2C3D4E5F607189F27018095050000000000"
	msg.Set(55, de55Data)

	packed, err := msg.Pack()
	if err != nil {
		t.Fatalf("Pack failed: %v", err)
	}

	unpacked, err := UnpackMessage(packed)
	if err != nil {
		t.Fatalf("UnpackMessage failed: %v", err)
	}

	val55, ok := unpacked.Get(55)
	if !ok || val55 != de55Data {
		t.Errorf("expected DE 55 %q, got %q", de55Data, val55)
	}
}

func TestISO8583Message_MalformedDataRejections(t *testing.T) {
	// 1. Empty buffer
	_, err := UnpackMessage([]byte{})
	if err == nil {
		t.Errorf("expected error for empty slice")
	}

	// 2. Truncated buffer (less than MTI + primary bitmap)
	_, errTrunc := UnpackMessage([]byte("01001234"))
	if errTrunc == nil {
		t.Errorf("expected error for truncated message")
	}
}
