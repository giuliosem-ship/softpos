package api

import (
	"math/rand"
	"testing"

	"softpos/pkg/store"
)

// Property: any sequence of partial refunds reverses every settlement
// component exactly (no cent drift), each step's journal legs are
// non-negative and balanced, and this holds up to the largest EMV amount
// (tag 9F02: 12 digits of cents).
func TestProportionalRefund_PartialRefundsSumExactlyToSettlement(t *testing.T) {
	const maxEMVCents = int64(999_999_999_999)
	r := rand.New(rand.NewSource(42))
	for i := 0; i < 20000; i++ {
		var g int64
		switch i % 3 {
		case 0:
			g = r.Int63n(60) + 1
		case 1:
			g = r.Int63n(10_000_000) + 1
		default:
			g = maxEMVCents - r.Int63n(1_000_000)
		}
		base := (g*100 + 58) / 116
		tax := g - base + bpsHalfUp(base, int64(r.Intn(250)))
		if tax > g {
			tax = g
		}
		fee := bpsHalfUp(g, int64(r.Intn(10001)))
		if fee > g-tax {
			fee = g - tax
		}
		rec := store.SettlementRecord{GrossCents: g, BaseCents: base, TaxEscrowCents: tax, PlatformFeeCents: fee}
		for steps := 0; rec.RefundedGrossCents < g; steps++ {
			rem := g - rec.RefundedGrossCents
			amt := r.Int63n(rem) + 1
			if r.Intn(3) == 0 && rem > 3 {
				amt = r.Int63n(3) + 1
			}
			if steps > 20 {
				amt = rem
			}
			sp := proportionalRefund(rec, amt)
			if sp.base < 0 || sp.taxEscrow < 0 || sp.fee < 0 || sp.available < 0 || sp.available+sp.taxEscrow+sp.fee != amt {
				t.Fatalf("g=%d base=%d tax=%d fee=%d refunded=%+v amt=%d: invalid split %+v", g, base, tax, fee, rec, amt, sp)
			}
			rec.RefundedGrossCents += sp.amount
			rec.RefundedBaseCents += sp.base
			rec.RefundedTaxEscrowCents += sp.taxEscrow
			rec.RefundedFeeCents += sp.fee
		}
		if rec.RefundedBaseCents != base || rec.RefundedTaxEscrowCents != tax || rec.RefundedFeeCents != fee {
			t.Fatalf("g=%d: refunds drifted from the settlement: base %d/%d tax %d/%d fee %d/%d",
				g, rec.RefundedBaseCents, base, rec.RefundedTaxEscrowCents, tax, rec.RefundedFeeCents, fee)
		}
	}
}

func TestRatioHalfUp_NoOverflowOnLargeAmounts(t *testing.T) {
	const g = int64(999_999_999_999)
	if got := ratioHalfUp(g, g/2, g); got != g/2 {
		t.Fatalf("ratioHalfUp overflowed: got %d", got)
	}
	if got := ratioHalfUp(5, 1, 2); got != 3 { // 2.5 -> 3 (half-up)
		t.Fatalf("half-up: got %d", got)
	}
}
