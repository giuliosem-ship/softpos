package stp

import (
	"testing"
)

func TestComputeControlDigitValid(t *testing.T) {
	// Banxico test vector: 03218000011835971 -> check digit should be 9
	// Full CLABE: 032180000118359719
	first17 := "03218000011835971"
	digit, err := ComputeControlDigit(first17)
	if err != nil {
		t.Fatalf("unexpected error computing control digit: %v", err)
	}
	if digit != 9 {
		t.Errorf("expected control digit 9, got %d", digit)
	}
}

func TestComputeControlDigitInvalidLengthOrChars(t *testing.T) {
	// Too short
	_, err := ComputeControlDigit("0321800001183597")
	if err != ErrInvalidCLABE {
		t.Errorf("expected ErrInvalidCLABE for 16 digits, got %v", err)
	}

	// Too long
	_, err = ComputeControlDigit("032180000118359719")
	if err != ErrInvalidCLABE {
		t.Errorf("expected ErrInvalidCLABE for 18 digits, got %v", err)
	}

	// Non-numeric
	_, err = ComputeControlDigit("0321800001183597A")
	if err != ErrInvalidCLABE {
		t.Errorf("expected ErrInvalidCLABE for non-numeric, got %v", err)
	}
}

func TestValidateCLABE(t *testing.T) {
	// Valid CLABEs
	validCLABEs := []string{
		"032180000118359719",
		"646180123456789014", // Will verify when computed
	}
	// Compute check digit for the 646 one to be sure
	digit, _ := ComputeControlDigit("64618012345678901")
	validCLABEs[1] = "64618012345678901" + string(rune('0'+digit))

	for _, c := range validCLABEs {
		if !ValidateCLABE(c) {
			t.Errorf("expected CLABE %s to be valid", c)
		}
	}

	// Invalid check digit
	invalidCheck := "032180000118359710"
	if ValidateCLABE(invalidCheck) {
		t.Errorf("expected CLABE %s to fail validation", invalidCheck)
	}

	// Invalid length
	if ValidateCLABE("03218000011835971") {
		t.Errorf("expected 17-digit CLABE to fail validation")
	}

	// Non-digit
	if ValidateCLABE("03218000011835971X") {
		t.Errorf("expected alphanumeric CLABE to fail validation")
	}
}

func TestGenerateCLABE(t *testing.T) {
	// Generate with default bank 646 (STP) and default plaza 180
	clabe, err := GenerateCLABE(1, "180", "646")
	if err != nil {
		t.Fatalf("unexpected error generating CLABE: %v", err)
	}

	if len(clabe) != 18 {
		t.Errorf("expected 18 digits, got %d (%s)", len(clabe), clabe)
	}

	if clabe[:6] != "646180" {
		t.Errorf("expected prefix 646180, got %s", clabe[:6])
	}

	if clabe[6:17] != "00000000001" {
		t.Errorf("expected account sequence 00000000001, got %s", clabe[6:17])
	}

	if !ValidateCLABE(clabe) {
		t.Errorf("generated CLABE %s failed validation", clabe)
	}
}

func TestGenerateCLABEBandLimits(t *testing.T) {
	// Negative sequence
	_, err := GenerateCLABE(-1, "180", "646")
	if err != ErrInvalidCLABE {
		t.Errorf("expected ErrInvalidCLABE for negative account seq, got %v", err)
	}

	// Exceeds 11 digits (100,000,000,000)
	_, err = GenerateCLABE(100000000000, "180", "646")
	if err != ErrInvalidCLABE {
		t.Errorf("expected ErrInvalidCLABE for sequence exceeding 11 digits, got %v", err)
	}

	// Invalid plaza length
	_, err = GenerateCLABE(1, "18", "646")
	if err != ErrInvalidCLABE {
		t.Errorf("expected ErrInvalidCLABE for plaza len != 3, got %v", err)
	}

	// Invalid bank length
	_, err = GenerateCLABE(1, "180", "64")
	if err != ErrInvalidCLABE {
		t.Errorf("expected ErrInvalidCLABE for bank len != 3, got %v", err)
	}
}
