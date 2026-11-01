package iso8583

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"
)

func hexToBytes(h string) []byte {
	b, _ := hex.DecodeString(h)
	return b
}

func TestDE55_BuildAndParseRoundTrip(t *testing.T) {
	tags := map[string][]byte{
		"9F26": hexToBytes("A1B2C3D4E5F60718"), // ARQC (8 bytes)
		"9F27": hexToBytes("80"),               // CID (1 byte)
		"9F10": hexToBytes("06010A03A00000"),   // IAD (7 bytes)
		"9F37": hexToBytes("12345678"),         // Unpredictable Number (4 bytes)
		"9F36": hexToBytes("0042"),             // ATC (2 bytes)
		"95":   hexToBytes("0000000000"),       // TVR (5 bytes)
		"9A":   hexToBytes("261031"),           // Date YYMMDD (3 bytes)
		"9C":   hexToBytes("00"),               // Transaction Type (1 byte)
		"9F02": hexToBytes("000000100000"),     // Amount (6 bytes BCD)
		"5F2A": hexToBytes("0484"),             // Currency MXN (2 bytes)
		"82":   hexToBytes("1980"),             // AIP (2 bytes)
	}

	encoded, err := BuildDE55(tags)
	if err != nil {
		t.Fatalf("BuildDE55 failed: %v", err)
	}

	decoded, err := ParseDE55(encoded)
	if err != nil {
		t.Fatalf("ParseDE55 failed: %v", err)
	}

	for k, expectedVal := range tags {
		val, ok := decoded[k]
		if !ok {
			t.Errorf("missing tag %s in decoded DE55", k)
		} else if !bytes.Equal(val, expectedVal) {
			t.Errorf("tag %s: expected %x, got %x", k, expectedVal, val)
		}
	}
}

func TestDE55_ValidateMandatoryOnlineTags_Success(t *testing.T) {
	tags := map[string][]byte{
		"9F26": hexToBytes("A1B2C3D4E5F60718"),
		"9F27": hexToBytes("80"),
		"9F10": hexToBytes("06010A03A00000"),
		"9F37": hexToBytes("12345678"),
		"9F36": hexToBytes("0042"),
		"95":   hexToBytes("0000000000"),
		"9A":   hexToBytes("261031"),
		"9C":   hexToBytes("00"),
		"9F02": hexToBytes("000000100000"),
		"5F2A": hexToBytes("0484"),
	}

	if err := ValidateMandatoryOnlineTags(tags); err != nil {
		t.Errorf("expected mandatory tags validation to pass, got: %v", err)
	}
}

func TestDE55_ValidateMandatoryOnlineTags_MissingTagFails(t *testing.T) {
	tags := map[string][]byte{
		// Missing 9F26 (ARQC)
		"9F27": hexToBytes("80"),
		"9F37": hexToBytes("12345678"),
	}

	err := ValidateMandatoryOnlineTags(tags)
	if !errors.Is(err, ErrMissingMandatoryEMVTag) {
		t.Errorf("expected ErrMissingMandatoryEMVTag, got: %v", err)
	}
}

func TestDE55_MalformedTLVTruncatedBytes(t *testing.T) {
	// Tag 9F02 claims length 6, but only 2 bytes provided
	truncated := hexToBytes("9F02061234")
	_, err := ParseDE55(truncated)
	if !errors.Is(err, ErrMalformedDE55TLV) {
		t.Errorf("expected ErrMalformedDE55TLV, got: %v", err)
	}
}
