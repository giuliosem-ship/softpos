package kyb

import (
	"context"
	"errors"
	"testing"

	"softpos/pkg/identity"
)

func validApp() Application {
	return Application{
		LegalName: "Xochilt Casas Chavez", RFC: "cacx7605101p8", CURP: "CACX760510MDFSHC09",
		TaxRegime: RegimeRESICOPF, MCC: "5812",
		Address:      Address{Street: "Av. Reforma", ExtNumber: "10", Neighborhood: "Centro", City: "CDMX", State: "CDMX", PostalCode: "06000"},
		ContactEmail: "dueno@example.com", ContactPhone: "+525512345678",
	}.Normalize()
}

func TestApplication_ValidateFields(t *testing.T) {
	if bad := validApp().Validate(); len(bad) != 0 {
		t.Fatalf("valid app rejected: %v", bad)
	}
	a := validApp()
	a.MCC, a.Address.PostalCode, a.ContactEmail, a.ContactPhone, a.TaxRegime, a.CURP = "58", "6000", "nope", "5512", "FOO", "BAD"
	bad := a.Validate()
	want := map[string]bool{"mcc": true, "address.postal_code": true, "contact_email": true, "contact_phone": true, "tax_regime": true, "curp": true}
	if len(bad) != len(want) {
		t.Fatalf("want %v, got %v", want, bad)
	}
	for _, f := range bad {
		if !want[f] {
			t.Errorf("unexpected invalid field %s", f)
		}
	}
}

func TestPrecheck_Rules(t *testing.T) {
	all := RequiredDocuments()
	cases := []struct {
		name   string
		mod    func(*Application)
		docs   []DocumentType
		reason string
	}{
		{"ok", func(*Application) {}, all, ""},
		{"bad check digit", func(a *Application) { a.RFC = "CACX7605101P9" }, all, ReasonRFCInvalid},
		{"persona moral rfc", func(a *Application) { a.RFC, a.CURP = "EKU9003173C9", "" }, all, ReasonPersonaMoral},
		{"regime general", func(a *Application) { a.TaxRegime = RegimeActividadEmpresarialPF }, all, ReasonUnsupportedRegime},
		{"curp of someone else", func(a *Application) { a.CURP = "GODE561231HDFRRR04" }, all, ReasonCURPMismatch},
		{"missing docs", func(*Application) {}, all[:3], ReasonDocumentsIncomplete},
		{"incomplete", func(a *Application) { a.LegalName = "" }, all, ReasonIncompleteApplication},
	}
	for _, c := range cases {
		a := validApp()
		c.mod(&a)
		checks, reason := Precheck(a, c.docs)
		if reason != c.reason {
			t.Errorf("%s: want reason %q, got %q (%+v)", c.name, c.reason, reason, checks)
		}
	}
	checks, _ := Precheck(validApp(), all)
	if !checks.RFCValid || !checks.DocumentsComplete || checks.List69B != List69BUnknown {
		t.Fatalf("unexpected checks %+v", checks)
	}
}

func TestPersonaMoral_FromRFCLength(t *testing.T) {
	if !PersonaMoral("EKU9003173C9") || PersonaMoral("CACX7605101P8") {
		t.Fatal("persona moral must be derived from the RFC length")
	}
}

func TestClassify69B(t *testing.T) {
	if Classify69B(identity.BlacklistDefinitivo) != List69BListed || Classify69B(identity.BlacklistPresunto) != List69BListed {
		t.Fatal("listed statuses")
	}
	if Classify69B(identity.BlacklistDesvirtuado) != List69BClear || Classify69B(identity.BlacklistClean) != List69BClear {
		t.Fatal("clear statuses")
	}
}

func TestMockKYCProvider(t *testing.T) {
	p := MockKYCProvider{}
	ctx := context.Background()
	base := IdentityCheckRequest{MerchantID: "m", INEFront: []byte("f"), INEBack: []byte("b")}
	for selfie, want := range map[string]IdentityResult{"ok": IdentityPassed, "FAIL x": IdentityFailed, "REVIEW": IdentityReview, "": IdentityFailed} {
		req := base
		req.Selfie = []byte(selfie)
		got, err := p.VerifyIdentity(ctx, req)
		if err != nil || got.Result != want {
			t.Errorf("selfie %q: want %s, got %+v %v", selfie, want, got, err)
		}
	}
	req := base
	req.Selfie = []byte("TIMEOUT")
	if _, err := p.VerifyIdentity(ctx, req); !errors.Is(err, ErrKYCProviderUnavailable) {
		t.Fatalf("want unavailable, got %v", err)
	}
}
