package fiscal

import (
	"testing"
)

func TestMonthlyBrackets(t *testing.T) {
	// <= 25,000 MXN (2,500,000 cents): 1.0%
	if r := GetResicoMonthlyRateBps(1000000); r != 100 {
		t.Errorf("expected 0.010, got %d", r)
	}
	if r := GetResicoMonthlyRateBps(2500000); r != 100 {
		t.Errorf("expected 0.010, got %d", r)
	}

	// <= 50,000 MXN (5,000,000 cents): 1.1%
	if r := GetResicoMonthlyRateBps(3500000); r != 110 {
		t.Errorf("expected 0.011, got %d", r)
	}
	if r := GetResicoMonthlyRateBps(5000000); r != 110 {
		t.Errorf("expected 0.011, got %d", r)
	}

	// <= 83,333.33 MXN: 1.5%
	if r := GetResicoMonthlyRateBps(6000000); r != 150 {
		t.Errorf("expected 0.015, got %d", r)
	}

	// <= 208,333.33 MXN: 2.0%
	if r := GetResicoMonthlyRateBps(10000000); r != 200 {
		t.Errorf("expected 0.020, got %d", r)
	}

	// > 208,333.33 MXN: 2.5%
	if r := GetResicoMonthlyRateBps(25000000); r != 250 {
		t.Errorf("expected 0.025, got %d", r)
	}
}

func TestB2CSaleTaxSplit(t *testing.T) {
	result, err := CalculateResicoTaxSplit(116000, 2000000, false)
	if err != nil {
		t.Fatalf("unexpected error calculating tax split: %v", err)
	}

	if result.BaseAmountCents != 100000 {
		t.Errorf("expected base 100000, got %d", result.BaseAmountCents)
	}
	if result.IVAAmountCents != 16000 {
		t.Errorf("expected IVA 16000, got %d", result.IVAAmountCents)
	}
	if result.ISRRateBps != 100 {
		t.Errorf("expected ISR rate 100 bps, got %d", result.ISRRateBps)
	}
	if result.ISREscrowCents != 1000 {
		t.Errorf("expected ISR escrow 1000, got %d", result.ISREscrowCents)
	}
	if result.TotalTaxEscrowCents != 17000 {
		t.Errorf("expected total escrow 17000, got %d", result.TotalTaxEscrowCents)
	}
	if result.NetMerchantCents != 99000 {
		t.Errorf("expected net merchant 99000, got %d", result.NetMerchantCents)
	}
	if result.IsPersonaMoralWithholding {
		t.Errorf("expected IsPersonaMoralWithholding false")
	}
}

func TestB2BPersonaMoralWithholding(t *testing.T) {
	// Art. 113-J LISR: 1.25% ISR withholding by Persona Moral
	result, err := CalculateResicoTaxSplit(116000, 2000000, true)
	if err != nil {
		t.Fatalf("unexpected error calculating tax split: %v", err)
	}

	if result.BaseAmountCents != 100000 {
		t.Errorf("expected base 100000, got %d", result.BaseAmountCents)
	}
	if result.IVAAmountCents != 16000 {
		t.Errorf("expected IVA 16000, got %d", result.IVAAmountCents)
	}
	if result.ISRRateBps != 125 {
		t.Errorf("expected ISR rate 125 bps, got %d", result.ISRRateBps)
	}
	if result.ISREscrowCents != 1250 {
		t.Errorf("expected ISR escrow 1250, got %d", result.ISREscrowCents)
	}
	if !result.IsPersonaMoralWithholding {
		t.Errorf("expected IsPersonaMoralWithholding true")
	}
	if result.TotalTaxEscrowCents != 16000 {
		t.Errorf("expected total tax escrow 16000 (IVA only), got %d", result.TotalTaxEscrowCents)
	}
	if result.NetMerchantCents != 98750 {
		t.Errorf("expected net merchant 98750 (100000 - 1250), got %d", result.NetMerchantCents)
	}
}

func TestInvalidAmounts(t *testing.T) {
	_, err := CalculateResicoTaxSplit(-5000, 100000, false)
	if err != ErrInvalidTaxAmount {
		t.Errorf("expected ErrInvalidTaxAmount for negative gross, got %v", err)
	}

	_, err = CalculateResicoTaxSplit(1000, -1, false)
	if err != ErrInvalidTaxAmount {
		t.Errorf("expected ErrInvalidTaxAmount for negative accumulated sales, got %v", err)
	}
}
