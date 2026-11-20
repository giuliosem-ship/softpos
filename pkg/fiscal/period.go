package fiscal

import (
	"time"
	// Embedded IANA zoneinfo so the RESICO month is right in minimal
	// containers without a system tzdata package.
	_ "time/tzdata"
)

// MexicoCityZone is the zone in which RESICO monthly periods are computed.
const MexicoCityZone = "America/Mexico_City"

var mexicoCity = mustLoadLocation(MexicoCityZone)

func mustLoadLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		// time/tzdata is embedded, so this only fails on a broken build.
		panic("fiscal: cannot load " + name + ": " + err.Error())
	}
	return loc
}

// MexicoCityLocation returns the America/Mexico_City location.
func MexicoCityLocation() *time.Location { return mexicoCity }

// ResicoYearMonth returns the "YYYY-MM" RESICO accumulation period that t
// falls in, with month boundaries at local midnight in America/Mexico_City
// (a sale at 23:30 local on the last day of the month belongs to that month
// even though it is already the next day in UTC).
func ResicoYearMonth(t time.Time) string {
	return t.In(mexicoCity).Format("2006-01")
}
