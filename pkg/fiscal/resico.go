package fiscal

import (
	"errors"
	"math"
)

var ErrInvalidTaxAmount = errors.New("tax amounts cannot be negative or invalid")

type ResicoSplitResult struct {
	BaseAmountCents      int64
	IVAAmountCents       int64
	ISRRate              float64
	ISREscrowCents       int64
	TotalTaxEscrowCents  int64
	NetMerchantCents     int64
	IsPersonaMoralWithholding bool
}

func GetResicoMonthlyRate(accumulatedSalesCents int64) float64 {
	switch {
	case accumulatedSalesCents <= 2500000:
		return 0.010
	case accumulatedSalesCents <= 5000000:
		return 0.011
	case accumulatedSalesCents <= 8333333:
		return 0.015
	case accumulatedSalesCents <= 20833333:
		return 0.020
	default:
		return 0.025
	}
}

func CalculateResicoTaxSplit(grossAmountCents int64, accumulatedMonthlySalesCents int64, customerIsPersonaMoral bool) (ResicoSplitResult, error) {
	if grossAmountCents <= 0 || accumulatedMonthlySalesCents < 0 {
		return ResicoSplitResult{}, ErrInvalidTaxAmount
	}

	baseAmountCents := int64(math.Round(float64(grossAmountCents) / 1.16))
	ivaAmountCents := grossAmountCents - baseAmountCents

	var isrRate float64
	var isrEscrowCents int64
	var totalTaxEscrowCents int64
	var netMerchantCents int64
	var isPM bool

	if customerIsPersonaMoral {
		isrRate = 0.0125
		isrEscrowCents = int64(math.Round(float64(baseAmountCents) * isrRate))
		totalTaxEscrowCents = ivaAmountCents
		netMerchantCents = baseAmountCents - isrEscrowCents
		isPM = true
	} else {
		isrRate = GetResicoMonthlyRate(accumulatedMonthlySalesCents + baseAmountCents)
		isrEscrowCents = int64(math.Round(float64(baseAmountCents) * isrRate))
		totalTaxEscrowCents = ivaAmountCents + isrEscrowCents
		netMerchantCents = grossAmountCents - totalTaxEscrowCents
		isPM = false
	}

	return ResicoSplitResult{
		BaseAmountCents:      baseAmountCents,
		IVAAmountCents:       ivaAmountCents,
		ISRRate:              isrRate,
		ISREscrowCents:       isrEscrowCents,
		TotalTaxEscrowCents:  totalTaxEscrowCents,
		NetMerchantCents:     netMerchantCents,
		IsPersonaMoralWithholding: isPM,
	}, nil
}
