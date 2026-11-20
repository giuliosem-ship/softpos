// Package kyb holds the merchant onboarding (KYB/KYC) domain rules of Kerno:
// the application data a merchant owner captures, the documents required,
// the automatic checks run at submission and the identity-verification
// provider port (liveness / INE check). Persistence lives in pkg/store
// (merchant_kyb, kyb_documents) and HTTP in pkg/api; see docs/KYB.md.
package kyb

import (
	"net/mail"
	"regexp"
	"strings"
	"unicode/utf8"

	"softpos/pkg/identity"
)

// Status is the onboarding state of a merchant.
//
//	DRAFT -> SUBMITTED -> IN_REVIEW -> APPROVED | REJECTED
//	REJECTED -> DRAFT (the owner edits data or documents and resubmits)
//	SUBMITTED -> REJECTED (automatic: RFC listed in SAT 69-B)
type Status string

const (
	StatusDraft     Status = "DRAFT"
	StatusSubmitted Status = "SUBMITTED"
	StatusInReview  Status = "IN_REVIEW"
	StatusApproved  Status = "APPROVED"
	StatusRejected  Status = "REJECTED"
)

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	switch s {
	case StatusDraft, StatusSubmitted, StatusInReview, StatusApproved, StatusRejected:
		return true
	}
	return false
}

// Editable reports whether the owner may change data or documents.
func (s Status) Editable() bool { return s == StatusDraft || s == StatusRejected }

// DocumentType is a KYC/KYB document the owner uploads.
type DocumentType string

const (
	DocINEFront       DocumentType = "INE_FRONT"
	DocINEBack        DocumentType = "INE_BACK"
	DocSelfie         DocumentType = "SELFIE"
	DocProofOfAddress DocumentType = "PROOF_OF_ADDRESS"
	DocBankStatement  DocumentType = "BANK_STATEMENT"
	DocCSF            DocumentType = "CSF" // Constancia de Situación Fiscal
)

// AllDocumentTypes lists every accepted document type.
var AllDocumentTypes = []DocumentType{DocINEFront, DocINEBack, DocSelfie, DocProofOfAddress, DocBankStatement, DocCSF}

// ValidDocumentType reports whether t is accepted by the upload endpoint.
func ValidDocumentType(t string) bool {
	for _, d := range AllDocumentTypes {
		if string(d) == t {
			return true
		}
	}
	return false
}

// RequiredDocuments returns the documents a persona física merchant must
// upload before submitting (the only kind supported by the MVP).
func RequiredDocuments() []DocumentType {
	return append([]DocumentType(nil), AllDocumentTypes...)
}

// MissingDocuments returns the required types absent from uploaded.
func MissingDocuments(uploaded []DocumentType) []DocumentType {
	have := map[DocumentType]bool{}
	for _, d := range uploaded {
		have[d] = true
	}
	var out []DocumentType
	for _, d := range RequiredDocuments() {
		if !have[d] {
			out = append(out, d)
		}
	}
	return out
}

// Document limits enforced by the upload endpoint.
const (
	MaxDocumentBytes = 5 << 20 // 5 MiB per file
)

// AllowedContentTypes are the sniffed (not client-declared) content types accepted.
var AllowedContentTypes = map[string]bool{"image/jpeg": true, "image/png": true, "application/pdf": true}

// TaxRegime is the merchant's SAT tax regime as captured in the application.
type TaxRegime string

const (
	RegimeRESICOPF                TaxRegime = "RESICO_PF"                // 626, persona física (supported)
	RegimeRESICOPM                TaxRegime = "RESICO_PM"                // 626, persona moral
	RegimeActividadEmpresarialPF  TaxRegime = "ACTIVIDAD_EMPRESARIAL_PF" // 612
	RegimePlataformasTecnologicas TaxRegime = "PLATAFORMAS_TECNOLOGICAS_PF"
	RegimeGeneralPM               TaxRegime = "GENERAL_PM" // 601
	RegimeOther                   TaxRegime = "OTHER"
)

var knownRegimes = map[TaxRegime]bool{
	RegimeRESICOPF: true, RegimeRESICOPM: true, RegimeActividadEmpresarialPF: true,
	RegimePlataformasTecnologicas: true, RegimeGeneralPM: true, RegimeOther: true,
}

// List69B is the result of the SAT 69-B lookup of the merchant RFC.
type List69B string

const (
	List69BClear   List69B = "CLEAR"
	List69BListed  List69B = "LISTED"  // PRESUNTO or DEFINITIVO
	List69BUnknown List69B = "UNKNOWN" // not checked yet or lookup unavailable
)

// Rejection / check-failure reasons (machine readable).
const (
	ReasonRFCInvalid            = "RFC_INVALID"
	ReasonPersonaMoral          = "PERSONA_MORAL_NOT_SUPPORTED"
	ReasonUnsupportedRegime     = "UNSUPPORTED_TAX_REGIME"
	ReasonCURPMismatch          = "CURP_RFC_MISMATCH"
	ReasonDocumentsIncomplete   = "DOCUMENTS_INCOMPLETE"
	ReasonIncompleteApplication = "APPLICATION_INCOMPLETE"
	ReasonListed69B             = "RFC_LISTED_69B"
)

// Address is the merchant's fiscal / business address.
type Address struct {
	Street       string `json:"street"`
	ExtNumber    string `json:"ext_number"`
	IntNumber    string `json:"int_number,omitempty"`
	Neighborhood string `json:"neighborhood"`
	City         string `json:"city"`
	State        string `json:"state"`
	PostalCode   string `json:"postal_code"`
}

// Application is the data the owner captures (PUT /api/v1/merchants/{id}/kyb).
type Application struct {
	LegalName    string    `json:"legal_name"`
	RFC          string    `json:"rfc"`
	CURP         string    `json:"curp,omitempty"`
	TaxRegime    TaxRegime `json:"tax_regime"`
	MCC          string    `json:"mcc"`
	Address      Address   `json:"address"`
	ContactEmail string    `json:"contact_email"`
	ContactPhone string    `json:"contact_phone"`
}

var (
	mccRe        = regexp.MustCompile(`^[0-9]{4}$`)
	postalCodeRe = regexp.MustCompile(`^[0-9]{5}$`)
	phoneRe      = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)
	rfcShapeRe   = regexp.MustCompile(`^[A-Z&Ñ0-9]{12,13}$`)
)

// Normalize trims every field and upper-cases RFC and CURP.
func (a Application) Normalize() Application {
	t := strings.TrimSpace
	a.LegalName = t(a.LegalName)
	a.RFC = strings.ToUpper(t(a.RFC))
	a.CURP = strings.ToUpper(t(a.CURP))
	a.TaxRegime = TaxRegime(strings.ToUpper(t(string(a.TaxRegime))))
	a.MCC = t(a.MCC)
	a.ContactEmail = t(a.ContactEmail)
	a.ContactPhone = t(a.ContactPhone)
	ad := &a.Address
	ad.Street, ad.ExtNumber, ad.IntNumber = t(ad.Street), t(ad.ExtNumber), t(ad.IntNumber)
	ad.Neighborhood, ad.City, ad.State, ad.PostalCode = t(ad.Neighborhood), t(ad.City), t(ad.State), t(ad.PostalCode)
	return a
}

func badLen(s string, max int) bool { return s == "" || utf8.RuneCountInString(s) > max }

// Validate checks the shape of a normalized application and returns the
// names of the invalid fields (nil when valid). Substantive checks (RFC
// verification digit, regime support, 69-B, documents) run at submission.
func (a Application) Validate() []string {
	var bad []string
	add := func(cond bool, field string) {
		if cond {
			bad = append(bad, field)
		}
	}
	add(badLen(a.LegalName, 255), "legal_name")
	add(!rfcShapeRe.MatchString(a.RFC), "rfc")
	add(a.CURP != "" && !identity.ValidateCURP(a.CURP), "curp")
	add(!knownRegimes[a.TaxRegime], "tax_regime")
	add(!mccRe.MatchString(a.MCC), "mcc")
	ad := a.Address
	add(badLen(ad.Street, 255), "address.street")
	add(badLen(ad.ExtNumber, 32), "address.ext_number")
	add(utf8.RuneCountInString(ad.IntNumber) > 32, "address.int_number")
	add(badLen(ad.Neighborhood, 128), "address.neighborhood")
	add(badLen(ad.City, 128), "address.city")
	add(badLen(ad.State, 64), "address.state")
	add(!postalCodeRe.MatchString(ad.PostalCode), "address.postal_code")
	if _, err := mail.ParseAddress(a.ContactEmail); err != nil || badLen(a.ContactEmail, 254) || strings.ContainsAny(a.ContactEmail, "<> ") {
		bad = append(bad, "contact_email")
	}
	add(!phoneRe.MatchString(a.ContactPhone), "contact_phone")
	return bad
}

// PersonaMoral derives the merchant's legal person type from its RFC
// length (12 characters = persona moral, 13 = persona física). It is never
// taken from a client flag.
func PersonaMoral(rfc string) bool {
	return utf8.RuneCountInString(strings.TrimSpace(rfc)) == 12
}

// Checks are the automatic verifications shown to the owner and operator.
type Checks struct {
	RFCValid          bool    `json:"rfc_valid"`
	List69B           List69B `json:"list_69b"`
	DocumentsComplete bool    `json:"documents_complete"`
}

// Precheck evaluates the deterministic submission rules of a normalized
// application. It returns "" when the application may be submitted, or the
// reason it may not (the status stays DRAFT and the owner fixes it).
func Precheck(a Application, uploaded []DocumentType) (Checks, string) {
	c := Checks{
		RFCValid:          identity.ValidateRFCWithCheckDigit(a.RFC),
		List69B:           List69BUnknown,
		DocumentsComplete: len(MissingDocuments(uploaded)) == 0,
	}
	switch {
	case len(a.Validate()) > 0:
		return c, ReasonIncompleteApplication
	case !c.RFCValid:
		return c, ReasonRFCInvalid
	case PersonaMoral(a.RFC):
		return c, ReasonPersonaMoral
	case a.TaxRegime != RegimeRESICOPF:
		return c, ReasonUnsupportedRegime
	case a.CURP != "" && !strings.HasPrefix(a.CURP, a.RFC[:10]):
		return c, ReasonCURPMismatch
	case !c.DocumentsComplete:
		return c, ReasonDocumentsIncomplete
	}
	return c, ""
}

// Classify69B maps a SAT 69-B status to the KYB check value.
func Classify69B(status identity.BlacklistStatus) List69B {
	switch status {
	case identity.BlacklistPresunto, identity.BlacklistDefinitivo:
		return List69BListed
	case identity.BlacklistClean, identity.BlacklistDesvirtuado, identity.BlacklistSentenciaFavorable, "":
		return List69BClear
	}
	return List69BUnknown
}
