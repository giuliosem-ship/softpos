package fiscal

import (
	_ "embed"
	"encoding/json"
	"net/mail"
	"regexp"
	"strings"
	"unicode/utf8"

	"softpos/pkg/identity"
)

// SAT catalogs (MVP-21): a versioned subset of the CFDI 4.0 catalogs
// embedded in the binary (catalogs/sat_catalogs.json). Bump "version" when
// the data changes; GET /api/v1/catalogs/sat serves it.

//go:embed catalogs/sat_catalogs.json
var satCatalogJSON []byte

// CatalogEntry is a code + description (c_ClaveProdServ, c_ClaveUnidad, …).
type CatalogEntry struct {
	Code        string `json:"code"`
	Description string `json:"description"`
}

// RegimenFiscalEntry is one c_RegimenFiscal row.
type RegimenFiscalEntry struct {
	Code          string `json:"code"`
	Description   string `json:"description"`
	PersonaFisica bool   `json:"persona_fisica"`
	PersonaMoral  bool   `json:"persona_moral"`
}

// UsoCFDIEntry is one c_UsoCFDI row with the receptor regimes it allows.
type UsoCFDIEntry struct {
	Code          string   `json:"code"`
	Description   string   `json:"description"`
	PersonaFisica bool     `json:"persona_fisica"`
	PersonaMoral  bool     `json:"persona_moral"`
	Regimenes     []string `json:"regimenes"`
}

// SATCatalogs is the embedded catalog set.
type SATCatalogs struct {
	Version           string               `json:"version"`
	Source            string               `json:"source"`
	RegimenFiscal     []RegimenFiscalEntry `json:"regimen_fiscal"`
	UsoCFDI           []UsoCFDIEntry       `json:"uso_cfdi"`
	ClaveProdServ     []CatalogEntry       `json:"clave_prod_serv"`
	ClaveUnidad       []CatalogEntry       `json:"clave_unidad"`
	MotivoCancelacion []CatalogEntry       `json:"motivo_cancelacion"`
	Periodicidad      []CatalogEntry       `json:"periodicidad"`
}

var catalogs = mustLoadCatalogs()

func mustLoadCatalogs() SATCatalogs {
	var c SATCatalogs
	if err := json.Unmarshal(satCatalogJSON, &c); err != nil || c.Version == "" {
		panic("fiscal: embedded SAT catalogs are invalid")
	}
	return c
}

// Catalogs returns the embedded SAT catalogs (read-only).
func Catalogs() SATCatalogs { return catalogs }

// CatalogNames are the catalogs served by the API.
var CatalogNames = []string{"regimen_fiscal", "uso_cfdi", "clave_prod_serv", "clave_unidad", "motivo_cancelacion", "periodicidad"}

// CatalogByName returns one catalog for the API (nil, false if unknown).
func CatalogByName(name string) (any, bool) {
	switch name {
	case "regimen_fiscal":
		return catalogs.RegimenFiscal, true
	case "uso_cfdi":
		return catalogs.UsoCFDI, true
	case "clave_prod_serv":
		return catalogs.ClaveProdServ, true
	case "clave_unidad":
		return catalogs.ClaveUnidad, true
	case "motivo_cancelacion":
		return catalogs.MotivoCancelacion, true
	case "periodicidad":
		return catalogs.Periodicidad, true
	}
	return nil, false
}

func findEntry(list []CatalogEntry, code string) bool {
	for _, e := range list {
		if e.Code == code {
			return true
		}
	}
	return false
}

// RegimenFiscal looks up a c_RegimenFiscal code.
func RegimenFiscal(code string) (RegimenFiscalEntry, bool) {
	for _, e := range catalogs.RegimenFiscal {
		if e.Code == code {
			return e, true
		}
	}
	return RegimenFiscalEntry{}, false
}

// UsoCFDI looks up a c_UsoCFDI code.
func UsoCFDI(code string) (UsoCFDIEntry, bool) {
	for _, e := range catalogs.UsoCFDI {
		if e.Code == code {
			return e, true
		}
	}
	return UsoCFDIEntry{}, false
}

// ValidClaveProdServ / ValidClaveUnidad check the merchant subset.
func ValidClaveProdServ(code string) bool { return findEntry(catalogs.ClaveProdServ, code) }
func ValidClaveUnidad(code string) bool   { return findEntry(catalogs.ClaveUnidad, code) }

// ValidMotivoCancelacion checks c_MotivoCancelacion.
func ValidMotivoCancelacion(code string) bool { return findEntry(catalogs.MotivoCancelacion, code) }

// Receptor is the customer data of an individual CFDI.
type Receptor struct {
	RFC           string `json:"rfc"`
	Nombre        string `json:"nombre"`
	RegimenFiscal string `json:"regimen_fiscal"`
	CodigoPostal  string `json:"codigo_postal"`
	UsoCFDI       string `json:"uso_cfdi"`
	Email         string `json:"email,omitempty"`
}

// Normalize trims fields, upper-cases the RFC and the name (the SAT name of
// the CSF is upper case) and collapses inner whitespace of the name.
func (r Receptor) Normalize() Receptor {
	r.RFC = strings.ToUpper(strings.TrimSpace(r.RFC))
	r.Nombre = strings.ToUpper(normalizeSpace(r.Nombre))
	r.RegimenFiscal = strings.TrimSpace(r.RegimenFiscal)
	r.CodigoPostal = strings.TrimSpace(r.CodigoPostal)
	r.UsoCFDI = strings.ToUpper(strings.TrimSpace(r.UsoCFDI))
	r.Email = strings.TrimSpace(r.Email)
	return r
}

// PersonaMoral reports whether the receptor RFC is a persona moral (12 chars).
func (r Receptor) PersonaMoral() bool { return utf8.RuneCountInString(r.RFC) == 12 }

// Receptor validation reasons (422 RECEPTOR_INVALID "fields").
const (
	ReasonRequired             = "REQUIRED"
	ReasonInvalidFormat        = "INVALID_FORMAT"
	ReasonInvalidCheckDigit    = "INVALID_CHECK_DIGIT"
	ReasonGenericRFC           = "GENERIC_RFC_NOT_ALLOWED"
	ReasonUnknownCode          = "NOT_IN_CATALOG"
	ReasonNotForPersonaType    = "NOT_ALLOWED_FOR_PERSONA_TYPE"
	ReasonIncompatibleRegimen  = "INCOMPATIBLE_WITH_REGIMEN"
	ReasonTooLong              = "TOO_LONG"
	ReasonGenericNameForbidden = "PUBLICO_EN_GENERAL_NOT_ALLOWED"
)

var cpRe = regexp.MustCompile(`^[0-9]{5}$`)

// ValidPostalCode checks the 5-digit CP format (the c_CodigoPostal
// existence check is left to the PAC).
func ValidPostalCode(cp string) bool { return cpRe.MatchString(cp) && cp != "00000" }

// ValidateReceptor validates a normalized individual-invoice receptor
// against the SAT catalog rules: RFC format and check digit (12 = persona
// moral, 13 = persona física; the generic RFCs are for the factura global),
// régimen applicable to the persona type, uso CFDI applicable to the persona
// type and compatible with the régimen, CP format, name and email. It
// returns field -> reason (empty = valid).
func ValidateReceptor(r Receptor) map[string]string {
	bad := map[string]string{}
	switch {
	case r.RFC == "":
		bad["rfc"] = ReasonRequired
	case r.RFC == RFCPublicoEnGeneral || r.RFC == RFCExtranjero:
		bad["rfc"] = ReasonGenericRFC
	case !identity.ValidateRFC(r.RFC):
		bad["rfc"] = ReasonInvalidFormat
	case !identity.ValidateRFCWithCheckDigit(r.RFC):
		bad["rfc"] = ReasonInvalidCheckDigit
	}
	pm := r.PersonaMoral()
	switch n := utf8.RuneCountInString(r.Nombre); {
	case n == 0:
		bad["nombre"] = ReasonRequired
	case n > 254:
		bad["nombre"] = ReasonTooLong
	case !ValidXMLText(r.Nombre):
		bad["nombre"] = ReasonInvalidFormat
	case r.Nombre == "PUBLICO EN GENERAL":
		bad["nombre"] = ReasonGenericNameForbidden
	}
	reg, regOK := RegimenFiscal(r.RegimenFiscal)
	switch {
	case r.RegimenFiscal == "":
		bad["regimen_fiscal"] = ReasonRequired
	case !regOK:
		bad["regimen_fiscal"] = ReasonUnknownCode
	case bad["rfc"] == "" && ((pm && !reg.PersonaMoral) || (!pm && !reg.PersonaFisica)):
		bad["regimen_fiscal"] = ReasonNotForPersonaType
	}
	uso, usoOK := UsoCFDI(r.UsoCFDI)
	switch {
	case r.UsoCFDI == "":
		bad["uso_cfdi"] = ReasonRequired
	case !usoOK:
		bad["uso_cfdi"] = ReasonUnknownCode
	case bad["rfc"] == "" && ((pm && !uso.PersonaMoral) || (!pm && !uso.PersonaFisica)):
		bad["uso_cfdi"] = ReasonNotForPersonaType
	case regOK && !containsString(uso.Regimenes, r.RegimenFiscal):
		bad["uso_cfdi"] = ReasonIncompatibleRegimen
	}
	switch {
	case r.CodigoPostal == "":
		bad["codigo_postal"] = ReasonRequired
	case !ValidPostalCode(r.CodigoPostal):
		bad["codigo_postal"] = ReasonInvalidFormat
	}
	if r.Email != "" {
		if a, err := mail.ParseAddress(r.Email); err != nil || a.Address != r.Email || len(r.Email) > 254 {
			bad["email"] = ReasonInvalidFormat
		}
	}
	return bad
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
