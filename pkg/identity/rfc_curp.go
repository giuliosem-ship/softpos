package identity

import (
	"errors"
	"regexp"
	"strings"
)

var (
	ErrInvalidRFC  = errors.New("invalid Mexican RFC")
	ErrInvalidCURP = errors.New("invalid Mexican CURP")

	rfcPFRegex = regexp.MustCompile(`^[A-Z&Ñ]{4}\d{6}[A-Z0-9]{3}$`)
	rfcPMRegex = regexp.MustCompile(`^[A-Z&Ñ]{3}\d{6}[A-Z0-9]{3}$`)
	curpRegex  = regexp.MustCompile(`^[A-Z][AEIOUX][A-Z]{2}\d{2}(0[1-9]|1[0-2])(0[1-9]|[12]\d|3[01])[HM](AS|BC|BS|CC|CL|CM|CS|CH|DF|DG|GT|GR|HG|JC|MC|MN|MS|NT|NL|OC|PL|QT|QR|SP|SL|SR|TC|TS|TL|VZ|YN|ZS|NE)[B-DF-HJ-NP-TV-Z]{3}[0-9A-Z]\d$`)
)

type RFCType string

const (
	RFCPersonaFisica RFCType = "PERSONA_FISICA"
	RFCPersonaMoral  RFCType = "PERSONA_MORAL"
	RFCGenerico      RFCType = "GENERICO"
)

type CURPDemographics struct {
	BirthDate string
	Gender    string
	StateCode string
}

func ValidateRFC(rfc string) bool {
	rfc = strings.ToUpper(strings.TrimSpace(rfc))

	// Generic RFCs
	if rfc == "XAXX010101000" || rfc == "XEXX010101000" {
		return true
	}

	// Persona Fisica
	if len(rfc) == 13 {
		return rfcPFRegex.MatchString(rfc)
	}

	// Persona Moral
	if len(rfc) == 12 {
		return rfcPMRegex.MatchString(rfc)
	}

	return false
}

func ParseRFCType(rfc string) (RFCType, error) {
	rfc = strings.ToUpper(strings.TrimSpace(rfc))
	if !ValidateRFC(rfc) {
		return "", ErrInvalidRFC
	}

	if rfc == "XAXX010101000" || rfc == "XEXX010101000" {
		return RFCGenerico, nil
	}

	switch len(rfc) {
	case 13:
		return RFCPersonaFisica, nil
	case 12:
		return RFCPersonaMoral, nil
	default:
		return "", ErrInvalidRFC
	}
}

func ValidateCURP(curp string) bool {
	curp = strings.ToUpper(strings.TrimSpace(curp))
	return curpRegex.MatchString(curp)
}

func ParseCURP(curp string) (CURPDemographics, error) {
	if !ValidateCURP(curp) {
		return CURPDemographics{}, ErrInvalidCURP
	}

	var demographics CURPDemographics

	demographics.Gender = "HOMBRE"
	if curp[10] == 'M' {
		demographics.Gender = "MUJER"
	}

	demographics.StateCode = curp[11:13]

	year := curp[4:6]
	month := curp[6:8]
	day := curp[8:10]

	if curp[16] >= 'A' && curp[16] <= 'Z' {
		year = "20" + year
	} else {
		year = "19" + year
	}

	demographics.BirthDate = year + "-" + month + "-" + day

	return demographics, nil
}
