package fiscal

import (
	"testing"
	"time"
)

func TestResicoYearMonth_UsesMexicoCityBoundaries(t *testing.T) {
	cases := []struct {
		utc  string
		want string
	}{
		// Mexico City is UTC-6 all year (no DST since 2022).
		{"2026-12-01T05:59:59Z", "2026-11"}, // 23:59:59 local, Nov 30
		{"2026-12-01T06:00:00Z", "2026-12"}, // 00:00 local, Dec 1
		{"2027-01-01T03:00:00Z", "2026-12"}, // New Year's Eve 21:00 local
		{"2027-01-01T06:00:00Z", "2027-01"},
		{"2026-11-15T12:00:00Z", "2026-11"},
	}
	for _, c := range cases {
		ts, err := time.Parse(time.RFC3339, c.utc)
		if err != nil {
			t.Fatal(err)
		}
		if got := ResicoYearMonth(ts); got != c.want {
			t.Errorf("ResicoYearMonth(%s) = %s, want %s", c.utc, got, c.want)
		}
	}
	if MexicoCityLocation().String() != MexicoCityZone {
		t.Fatalf("location %s", MexicoCityLocation())
	}
}
