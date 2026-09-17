package identity

import (
	"testing"
)

func TestValidateRFC(t *testing.T) {
	// Persona Fisica (13 chars)
	if !ValidateRFC("GARM850101ABC") {
		t.Errorf("expected GARM850101ABC to be valid Persona Fisica")
	}
	rfType, err := ParseRFCType("GARM850101ABC")
	if err != nil || rfType != RFCPersonaFisica {
		t.Errorf("expected RFCPersonaFisica, got %v, err: %v", rfType, err)
	}

	// Persona Moral (12 chars)
	if !ValidateRFC("ABC850101XYZ") {
		t.Errorf("expected ABC850101XYZ to be valid Persona Moral")
	}
	rfType, err = ParseRFCType("ABC850101XYZ")
	if err != nil || rfType != RFCPersonaMoral {
		t.Errorf("expected RFCPersonaMoral, got %v, err: %v", rfType, err)
	}

	// Genericos
	if !ValidateRFC("XAXX010101000") {
		t.Errorf("expected XAXX010101000 to be valid generic")
	}
	rfType, err = ParseRFCType("XAXX010101000")
	if err != nil || rfType != RFCGenerico {
		t.Errorf("expected RFCGenerico, got %v, err: %v", rfType, err)
	}

	if !ValidateRFC("XEXX010101000") {
		t.Errorf("expected XEXX010101000 to be valid generic")
	}
	rfType, err = ParseRFCType("XEXX010101000")
	if err != nil || rfType != RFCGenerico {
		t.Errorf("expected RFCGenerico, got %v, err: %v", rfType, err)
	}

	// Invalids
	if ValidateRFC("INVALID_RFC_123") {
		t.Errorf("expected invalid RFC to fail validation")
	}
	_, err = ParseRFCType("INVALID_RFC")
	if err != ErrInvalidRFC {
		t.Errorf("expected ErrInvalidRFC, got %v", err)
	}
}

func TestValidateAndParseCURP(t *testing.T) {
	// Hombre, DF, 1985-01-01
	curp := "GARM850101HDFRRN09"
	if !ValidateCURP(curp) {
		t.Fatalf("expected CURP %s to be valid", curp)
	}
	demographics, err := ParseCURP(curp)
	if err != nil {
		t.Fatalf("unexpected error parsing CURP: %v", err)
	}
	if demographics.BirthDate != "1985-01-01" {
		t.Errorf("expected birth date 1985-01-01, got %s", demographics.BirthDate)
	}
	if demographics.Gender != "HOMBRE" {
		t.Errorf("expected gender HOMBRE, got %s", demographics.Gender)
	}
	if demographics.StateCode != "DF" {
		t.Errorf("expected state code DF, got %s", demographics.StateCode)
	}

	// Mujer, NL, 1992-05-12
	curpMujer := "GARM920512MNLRRN01"
	if !ValidateCURP(curpMujer) {
		t.Fatalf("expected CURP %s to be valid", curpMujer)
	}
	demographicsMujer, err := ParseCURP(curpMujer)
	if err != nil {
		t.Fatalf("unexpected error parsing CURP: %v", err)
	}
	if demographicsMujer.BirthDate != "1992-05-12" {
		t.Errorf("expected birth date 1992-05-12, got %s", demographicsMujer.BirthDate)
	}
	if demographicsMujer.Gender != "MUJER" {
		t.Errorf("expected gender MUJER, got %s", demographicsMujer.Gender)
	}
	if demographicsMujer.StateCode != "NL" {
		t.Errorf("expected state code NL, got %s", demographicsMujer.StateCode)
	}
}

func TestInvalidCURPFails(t *testing.T) {
	if ValidateCURP("12345") {
		t.Errorf("expected short CURP to fail")
	}
	_, err := ParseCURP("INVALID_CURP_FORMAT")
	if err != ErrInvalidCURP {
		t.Errorf("expected ErrInvalidCURP, got %v", err)
	}
}
