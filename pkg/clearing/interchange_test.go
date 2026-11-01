package clearing

import (
	"testing"
)

func TestInterchangeCalculator_DebitCategories(t *testing.T) {
	calc := NewInterchangeCalculator(150) // 150 bps acquirer markup

	testCases := []struct {
		name                 string
		mcc                  int
		grossCents           int64
		expectedInterchangeBps int64
	}{
		{"Gas Station MCC 5541", 5541, 100000, 25},      // $1,000.00 MXN -> 25 bps (0.25%)
		{"Gas Station MCC 5542", 5542, 50000, 25},       // $500.00 MXN -> 25 bps (0.25%)
		{"Supermarket MCC 5411", 5411, 250000, 35},      // $2,500.00 MXN -> 35 bps (0.35%)
		{"Education MCC 8211", 8211, 400000, 30},        // $4,000.00 MXN -> 30 bps (0.30%)
		{"Education MCC 8220", 8220, 600000, 30},        // $6,000.00 MXN -> 30 bps (0.30%)
		{"General Retail MCC 5999", 5999, 150000, 55},   // $1,500.00 MXN -> 55 bps (0.55%)
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := calc.CalculateDebit(tc.grossCents, tc.mcc)
			if err != nil {
				t.Fatalf("CalculateDebit failed: %v", err)
			}

			if res.InterchangeBps != tc.expectedInterchangeBps {
				t.Errorf("InterchangeBps = %d, want %d", res.InterchangeBps, tc.expectedInterchangeBps)
			}

			// Invariant: Gross = NetMerchant + TotalFees + TotalIVA
			if tc.grossCents != (res.NetMerchantCents + res.TotalFeeCents + res.TotalIVACents) {
				t.Errorf("Accounting invariant broken: Gross (%d) != Net (%d) + Fees (%d) + IVA (%d)",
					tc.grossCents, res.NetMerchantCents, res.TotalFeeCents, res.TotalIVACents)
			}

			// Invariant: TotalFees = InterchangeFee + AcquirerFee
			if res.TotalFeeCents != (res.InterchangeFeeCents + res.AcquirerFeeCents) {
				t.Errorf("Fee sum broken: TotalFees (%d) != Interchange (%d) + Acquirer (%d)",
					res.TotalFeeCents, res.InterchangeFeeCents, res.AcquirerFeeCents)
			}
		})
	}
}

func TestInterchangeCalculator_CreditTiers(t *testing.T) {
	calc := NewInterchangeCalculator(120) // 120 bps acquirer markup

	testCases := []struct {
		name                 string
		tier                 CardTier
		grossCents           int64
		expectedInterchangeBps int64
	}{
		{"Classic Card", TierClassic, 120000, 110},       // 110 bps (1.10%)
		{"Gold Card", TierGold, 350000, 135},             // 135 bps (1.35%)
		{"Platinum Card", TierPlatinum, 800000, 160},     // 160 bps (1.60%)
		{"Corporate Card", TierCorporate, 1500000, 185},  // 185 bps (1.85%)
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := calc.CalculateCredit(tc.grossCents, tc.tier)
			if err != nil {
				t.Fatalf("CalculateCredit failed: %v", err)
			}

			if res.InterchangeBps != tc.expectedInterchangeBps {
				t.Errorf("InterchangeBps = %d, want %d", res.InterchangeBps, tc.expectedInterchangeBps)
			}

			// Invariant: Gross = NetMerchant + TotalFees + TotalIVA
			if tc.grossCents != (res.NetMerchantCents + res.TotalFeeCents + res.TotalIVACents) {
				t.Errorf("Accounting invariant broken: Gross (%d) != Net (%d) + Fees (%d) + IVA (%d)",
					tc.grossCents, res.NetMerchantCents, res.TotalFeeCents, res.TotalIVACents)
			}
		})
	}
}

func TestInterchangeCalculator_HalfUpRoundingPrecision(t *testing.T) {
	calc := NewInterchangeCalculator(100) // 100 bps

	// Gross = $10.01 MXN = 1001 cents
	// Interchange: 55 bps -> 1001 * 55 = 55,055 / 10000 = 5.5055 -> 6 cents (half-up: (55055 + 5000)/10000 = 6)
	// Acquirer: 100 bps -> 1001 * 100 = 100,100 / 10000 = 10.01 -> 10 cents (half-up: (100100 + 5000)/10000 = 10)
	// Total Fees = 16 cents
	// IVA: 16 * 16% = 2.56 -> 3 cents (half-up: (16*16 + 50)/100 = 306/100 = 3)
	// Net: 1001 - 16 - 3 = 982 cents
	res, err := calc.CalculateDebit(1001, 5999)
	if err != nil {
		t.Fatalf("CalculateDebit failed: %v", err)
	}

	if res.InterchangeFeeCents != 6 {
		t.Errorf("InterchangeFeeCents = %d, want 6", res.InterchangeFeeCents)
	}
	if res.AcquirerFeeCents != 10 {
		t.Errorf("AcquirerFeeCents = %d, want 10", res.AcquirerFeeCents)
	}
	if res.TotalFeeCents != 16 {
		t.Errorf("TotalFeeCents = %d, want 16", res.TotalFeeCents)
	}
	if res.TotalIVACents != 3 {
		t.Errorf("TotalIVACents = %d, want 3", res.TotalIVACents)
	}
	if res.NetMerchantCents != 982 {
		t.Errorf("NetMerchantCents = %d, want 982", res.NetMerchantCents)
	}
	if 1001 != (res.NetMerchantCents + res.TotalFeeCents + res.TotalIVACents) {
		t.Errorf("Rounding broke exact sum: 1001 != %d", res.NetMerchantCents+res.TotalFeeCents+res.TotalIVACents)
	}
}

func TestInterchangeCalculator_InvalidInputs(t *testing.T) {
	calc := NewInterchangeCalculator(100)

	if _, err := calc.CalculateDebit(0, 5999); err != ErrInvalidGrossAmount {
		t.Errorf("Expected ErrInvalidGrossAmount for 0 cents, got: %v", err)
	}

	if _, err := calc.CalculateDebit(-500, 5999); err != ErrInvalidGrossAmount {
		t.Errorf("Expected ErrInvalidGrossAmount for negative cents, got: %v", err)
	}

	if _, err := calc.CalculateCredit(1000, CardTier("UNKNOWN")); err != ErrInvalidCardTier {
		t.Errorf("Expected ErrInvalidCardTier, got: %v", err)
	}
}
