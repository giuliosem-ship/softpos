package fiscal_test

import (
	"testing"

	"softpos/pkg/fiscal"
)

func TestCatalogs_EmbeddedAndConsistent(t *testing.T) {
	c := fiscal.Catalogs()
	if c.Version == "" || len(c.RegimenFiscal) < 19 || len(c.UsoCFDI) < 24 {
		t.Fatalf("catalogs: version=%q regimen=%d uso=%d", c.Version, len(c.RegimenFiscal), len(c.UsoCFDI))
	}
	for _, u := range c.UsoCFDI {
		for _, r := range u.Regimenes {
			if _, ok := fiscal.RegimenFiscal(r); !ok {
				t.Errorf("uso %s references unknown regimen %s", u.Code, r)
			}
		}
	}
	for _, name := range fiscal.CatalogNames {
		if _, ok := fiscal.CatalogByName(name); !ok {
			t.Errorf("catalog %s not served", name)
		}
	}
	if !fiscal.ValidClaveProdServ("01010101") || !fiscal.ValidClaveUnidad("H87") || fiscal.ValidClaveUnidad("XXX") {
		t.Fatal("clave lookups")
	}
}

func TestValidateReceptor(t *testing.T) {
	ok := fiscal.Receptor{RFC: "EKU9003173C9", Nombre: "ESCUELA KEMPER URGATE", RegimenFiscal: "601", CodigoPostal: "26015", UsoCFDI: "G03", Email: "cfdi@example.com"}
	if bad := fiscal.ValidateReceptor(ok); len(bad) != 0 {
		t.Fatalf("valid PM receptor rejected: %v", bad)
	}
	pf := fiscal.Receptor{RFC: "XIQB891116QE4", Nombre: "BERENICE XIMO QUEZADA", RegimenFiscal: "612", CodigoPostal: "88730", UsoCFDI: "D01"}
	if bad := fiscal.ValidateReceptor(pf); len(bad) != 0 {
		t.Fatalf("valid PF receptor rejected: %v", bad)
	}
	cases := []struct {
		name  string
		mut   func(r *fiscal.Receptor)
		field string
		want  string
	}{
		{"generic rfc", func(r *fiscal.Receptor) { r.RFC = "XAXX010101000" }, "rfc", fiscal.ReasonGenericRFC},
		{"bad rfc", func(r *fiscal.Receptor) { r.RFC = "ABC" }, "rfc", fiscal.ReasonInvalidFormat},
		{"check digit", func(r *fiscal.Receptor) { r.RFC = "EKU9003173C1" }, "rfc", fiscal.ReasonInvalidCheckDigit},
		{"PF regimen for PM", func(r *fiscal.Receptor) { r.RegimenFiscal = "612" }, "regimen_fiscal", fiscal.ReasonNotForPersonaType},
		{"unknown regimen", func(r *fiscal.Receptor) { r.RegimenFiscal = "999" }, "regimen_fiscal", fiscal.ReasonUnknownCode},
		{"D01 for PM", func(r *fiscal.Receptor) { r.UsoCFDI = "D01" }, "uso_cfdi", fiscal.ReasonNotForPersonaType},
		{"uso incompatible with regimen", func(r *fiscal.Receptor) { r.RegimenFiscal = "603"; r.UsoCFDI = "CN01" }, "uso_cfdi", fiscal.ReasonNotForPersonaType},
		{"cp format", func(r *fiscal.Receptor) { r.CodigoPostal = "1234" }, "codigo_postal", fiscal.ReasonInvalidFormat},
		{"no name", func(r *fiscal.Receptor) { r.Nombre = "" }, "nombre", fiscal.ReasonRequired},
		{"publico en general", func(r *fiscal.Receptor) { r.Nombre = "PUBLICO EN GENERAL" }, "nombre", fiscal.ReasonGenericNameForbidden},
		{"email", func(r *fiscal.Receptor) { r.Email = "not-an-email" }, "email", fiscal.ReasonInvalidFormat},
	}
	for _, c := range cases {
		r := ok
		c.mut(&r)
		if got := fiscal.ValidateReceptor(r)[c.field]; got != c.want {
			t.Errorf("%s: %s = %q, want %q (%v)", c.name, c.field, got, c.want, fiscal.ValidateReceptor(r))
		}
	}
	// PF with régimen 616 (sin obligaciones) may only use S01/CP01.
	sinOb := pf
	sinOb.RegimenFiscal, sinOb.UsoCFDI = "616", "G03"
	if got := fiscal.ValidateReceptor(sinOb)["uso_cfdi"]; got != fiscal.ReasonIncompatibleRegimen {
		t.Fatalf("616 + G03: %q", got)
	}
	sinOb.UsoCFDI = "S01"
	if bad := fiscal.ValidateReceptor(sinOb); len(bad) != 0 {
		t.Fatalf("616 + S01: %v", bad)
	}
	if n := (fiscal.Receptor{RFC: " eku9003173c9 ", Nombre: "  escuela   kemper "}).Normalize(); n.RFC != "EKU9003173C9" || n.Nombre != "ESCUELA KEMPER" {
		t.Fatalf("normalize: %+v", n)
	}
}
