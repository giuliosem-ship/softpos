package fiscal

import (
	"errors"
)

var ErrInvalidTaxAmount = errors.New("tax amounts cannot be negative or invalid")

type ResicoSplitResult struct {
	BaseAmountCents           int64
	IVAAmountCents            int64
	ISRRate                   float64
	ISREscrowCents            int64
	TotalTaxEscrowCents       int64
	NetMerchantCents          int64
	IsPersonaMoralWithholding bool
}

// getResicoMonthlyRateBps returns the RESICO monthly rate in basis points (1 bp = 0.01% = 1/10000).
func getResicoMonthlyRateBps(accumulatedSalesCents int64) int64 {
	switch {
	case accumulatedSalesCents <= 2500000:
		return 100 // 1.00%
	case accumulatedSalesCents <= 5000000:
		return 110 // 1.10%
	case accumulatedSalesCents <= 8333333:
		return 150 // 1.50%
	case accumulatedSalesCents <= 20833333:
		return 200 // 2.00%
	default:
		return 250 // 2.50%
	}
}

func GetResicoMonthlyRate(accumulatedSalesCents int64) float64 {
	return float64(getResicoMonthlyRateBps(accumulatedSalesCents)) / 10000.0
}

// CalculateResicoTaxSplit computes IVA and ISR RESICO splits using pure int64 arithmetic (zero float64 division/rounding).
func CalculateResicoTaxSplit(grossAmountCents int64, accumulatedMonthlySalesCents int64, customerIsPersonaMoral bool) (ResicoSplitResult, error) {
	if grossAmountCents <= 0 || accumulatedMonthlySalesCents < 0 {
		return ResicoSplitResult{}, ErrInvalidTaxAmount
	}

	// Base = round(gross / 1.16) = round(gross * 100 / 116) using integer half-up rounding (+58 / 116)
	baseAmountCents := (grossAmountCents*100 + 58) / 116
	ivaAmountCents := grossAmountCents - baseAmountCents

	var isrRateBps int64
	var isrEscrowCents int64
	var totalTaxEscrowCents int64
	var netMerchantCents int64
	var isPM bool

	if customerIsPersonaMoral {
		// Art. 113-J LISR: 1.25% = 125 basis points (125 / 10000)
		isrRateBps = 125
		isrEscrowCents = (baseAmountCents*isrRateBps + 5000) / 10000
		totalTaxEscrowCents = ivaAmountCents
		netMerchantCents = baseAmountCents - isrEscrowCents
		isPM = true
	} else {
		isrRateBps = getResicoMonthlyRateBps(accumulatedMonthlySalesCents + baseAmountCents)
		isrEscrowCents = (baseAmountCents*isrRateBps + 5000) / 10000
		totalTaxEscrowCents = ivaAmountCents + isrEscrowCents
		netMerchantCents = grossAmountCents - totalTaxEscrowCents
		isPM = false
	}

	return ResicoSplitResult{
		BaseAmountCents:           baseAmountCents,
		IVAAmountCents:            ivaAmountCents,
		ISRRate:                   float64(isrRateBps) / 10000.0,
		ISREscrowCents:            isrEscrowCents,
		TotalTaxEscrowCents:       totalTaxEscrowCents,
		NetMerchantCents:          netMerchantCents,
		IsPersonaMoralWithholding: isPM,
	}, nil
}
