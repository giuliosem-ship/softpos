package api

import (
	"testing"

	"softpos/pkg/fiscal"
	"softpos/pkg/store"
)

// satIVALimits are the CFDI 4.0 limits of a 16 % traslado Importe for a
// base in cents: [(base-0.005)*0.16 truncated, (base+0.005)*0.16 rounded up].
func satIVALimits(base int64) (lo, hi int64) {
	return (2*base - 1) * 8 / 100, ((2*base+1)*8 + 99) / 100
}

func TestSettlementInvoiceSplit_PartialRefundStaysWithinSATLimits(t *testing.T) {
	gross := int64(1515384)
	base, iva := fiscal.IVAFromGross(gross)
	st := store.SettlementRecord{GrossCents: gross, BaseCents: base, IVACents: iva,
		RefundedGrossCents: 67661, RefundedBaseCents: ratioHalfUp(base, 67661, gross)}
	g, b, i := settlementInvoiceSplit(st)
	if g != gross-67661 || b+i != g {
		t.Fatalf("invoice must total the net amount paid: gross %d base %d iva %d", g, b, i)
	}
	if lo, hi := satIVALimits(b); i < lo || i > hi {
		t.Fatalf("IVA %d out of the SAT limits [%d, %d] for base %d (CFDI40 rejection)", i, lo, hi, b)
	}
	// Without refunds the posted split is used as is.
	if g, b, i := settlementInvoiceSplit(store.SettlementRecord{GrossCents: 11600, BaseCents: 10000, IVACents: 1600}); g != 11600 || b != 10000 || i != 1600 {
		t.Fatalf("unrefunded split: %d %d %d", g, b, i)
	}
}
