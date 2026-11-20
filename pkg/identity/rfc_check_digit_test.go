package identity

import "testing"

func TestRFCCheckDigit_KnownSATRFCs(t *testing.T) {
	// Public SAT test RFCs (CFDI 4.0 sandbox certificates) and a classic example.
	for _, rfc := range []string{"EKU9003173C9", "XIA190128J61", "CACX7605101P8", "GODE561231GR8"} {
		if !ValidateRFCWithCheckDigit(rfc) {
			d, _ := RFCCheckDigit(rfc)
			t.Errorf("%s: expected valid check digit (computed %q)", rfc, d)
		}
	}
}

func TestRFCCheckDigit_RejectsWrongDigitAndGeneric(t *testing.T) {
	for _, rfc := range []string{"EKU9003173C8", "CACX7605101P9", "XAXX010101000", "XEXX010101000", "NOT-AN-RFC", ""} {
		if ValidateRFCWithCheckDigit(rfc) {
			t.Errorf("%s: expected invalid", rfc)
		}
	}
}
