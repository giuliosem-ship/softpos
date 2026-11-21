package fiscal

import "time"

// CFDI Fecha time zone (MVP-24). The SAT / PAC validate the comprobante's
// Fecha against the local time of its LugarExpedicion (the postal code), so a
// merchant west of Mexico City would otherwise emit a Fecha 1-2 h "in the
// future" and be rejected while stamping in those minutes.
//
// Source: Mexican postal codes are assigned by state (SEPOMEX: the first two
// digits identify the state), and the time zones follow the Ley del Sistema
// de Horario (2022) and its IANA tz mapping. The SAT's own c_CodigoPostal
// catalog carries the exact huso horario per code; until it is embedded,
// this compact table maps the zones that differ from Mexico City by CP
// range, with the documented limitations in docs/CFDI.md.

type cpZone struct {
	from, to int // inclusive 5-digit postal code range
	zone     string
}

// cpZones is ordered: the first matching range wins (specific sub-ranges
// before their state's range).
var cpZones = []cpZone{
	{21000, 22999, "America/Tijuana"},        // Baja California (UTC-8, US DST)
	{23000, 23999, "America/Mazatlan"},       // Baja California Sur (UTC-7)
	{80000, 82999, "America/Mazatlan"},       // Sinaloa (UTC-7)
	{83000, 85999, "America/Hermosillo"},     // Sonora (UTC-7, no DST)
	{63730, 63739, "America/Bahia_Banderas"}, // Bahía de Banderas, Nayarit (Valle de Banderas, Bucerías, Nuevo Vallarta, Punta de Mita: UTC-6)
	{63842, 63842, "America/Bahia_Banderas"}, // Sayulita (Bahía de Banderas)
	{63000, 63999, "America/Mazatlan"},       // rest of Nayarit (UTC-7)
	{77000, 77999, "America/Cancun"},         // Quintana Roo (UTC-5)
	{32000, 32699, "America/Ciudad_Juarez"},  // Ciudad Juárez, Chihuahua (UTC-7, US DST)
	{32880, 32889, "America/Ojinaga"},        // Ojinaga, Chihuahua (UTC-6, US DST)
	{31000, 33999, "America/Chihuahua"},      // rest of Chihuahua (UTC-6, no DST since 2022)
}

var cpLocations = func() map[string]*time.Location {
	out := map[string]*time.Location{}
	for _, z := range cpZones {
		if _, ok := out[z.zone]; !ok {
			out[z.zone] = mustLoadLocation(z.zone)
		}
	}
	return out
}()

// ZoneForPostalCode returns the IANA zone of a 5-digit postal code
// (America/Mexico_City by default and for malformed codes).
func ZoneForPostalCode(cp string) string {
	if !ValidPostalCode(cp) {
		return MexicoCityZone
	}
	n := 0
	for _, c := range cp {
		n = n*10 + int(c-'0')
	}
	for _, z := range cpZones {
		if n >= z.from && n <= z.to {
			return z.zone
		}
	}
	return MexicoCityZone
}

// LocationForPostalCode returns the time zone of a LugarExpedicion.
func LocationForPostalCode(cp string) *time.Location {
	if loc, ok := cpLocations[ZoneForPostalCode(cp)]; ok {
		return loc
	}
	return mexicoCity
}
