package fiscal_test

import (
	"testing"
	"time"

	"softpos/pkg/fiscal"
)

func TestZoneForPostalCode(t *testing.T) {
	cases := map[string]string{
		"06000": "America/Mexico_City", "44100": "America/Mexico_City", "64000": "America/Mexico_City",
		"22000": "America/Tijuana", "21100": "America/Tijuana",
		"23000": "America/Mazatlan", "80000": "America/Mazatlan", "63000": "America/Mazatlan",
		"63732": "America/Bahia_Banderas", "63842": "America/Bahia_Banderas",
		"83000": "America/Hermosillo", "85000": "America/Hermosillo",
		"77500": "America/Cancun",
		"32000": "America/Ciudad_Juarez", "32880": "America/Ojinaga", "31000": "America/Chihuahua",
		"": "America/Mexico_City", "12": "America/Mexico_City", "ABCDE": "America/Mexico_City",
	}
	for cp, want := range cases {
		if got := fiscal.ZoneForPostalCode(cp); got != want {
			t.Errorf("ZoneForPostalCode(%q) = %s, want %s", cp, got, want)
		}
		if got := fiscal.LocationForPostalCode(cp).String(); got != want {
			t.Errorf("LocationForPostalCode(%q) = %s, want %s", cp, got, want)
		}
	}
}

// TestBuildInvoiceComprobante_FechaInLugarExpedicionZone: Fecha is the
// local time of the LugarExpedicion, not always Mexico City.
func TestBuildInvoiceComprobante_FechaInLugarExpedicionZone(t *testing.T) {
	at := time.Date(2026, 7, 15, 18, 30, 0, 0, time.UTC)
	for cp, want := range map[string]string{
		"06000": "2026-07-15T12:30:00", // CDMX UTC-6
		"22000": "2026-07-15T11:30:00", // Tijuana UTC-7 (US DST)
		"83000": "2026-07-15T11:30:00", // Hermosillo UTC-7
		"77500": "2026-07-15T13:30:00", // Cancún UTC-5
	} {
		spec := invoiceSpec()
		spec.Fecha, spec.LugarExpedicion = at, cp
		comp, _, err := fiscal.BuildInvoiceComprobante(spec)
		if err != nil {
			t.Fatal(err)
		}
		if comp.Fecha != want {
			t.Errorf("CP %s: Fecha %s, want %s", cp, comp.Fecha, want)
		}
	}
}
