package clearing

import (
	"errors"
)

var (
	ErrInvalidGrossAmount = errors.New("gross transaction amount must be greater than zero")
	ErrInvalidCardTier    = errors.New("invalid card tier specified")
	ErrInvalidMCC         = errors.New("invalid merchant category code")
)

type CardTier string

const (
	TierClassic   CardTier = "CLASSIC"
	TierGold      CardTier = "GOLD"
	TierPlatinum  CardTier = "PLATINUM"
	TierCorporate CardTier = "CORPORATE"
)

// Regulated Banxico Debit Interchange Caps (in Basis Points, 1 bps = 0.01%)
const (
	DebitBpsFuel        int64 = 25 // MCC 5541, 5542
	DebitBpsSupermarket int64 = 35 // MCC 5411
	DebitBpsEducation   int64 = 30 // MCC 8211, 8220
	DebitBpsGeneral     int64 = 55 // Standard retail limit
)

// Credit Interchange Rates by Card Tier (in Basis Points)
const (
	CreditBpsClassic   int64 = 110
	CreditBpsGold      int64 = 135
	CreditBpsPlatinum  int64 = 160
	CreditBpsCorporate int64 = 185
)

const (
	IVAPercentage int64 = 16 // Mexican Standard VAT rate
)

// SettlementBreakdown details the exact monetary distribution in integer cents.
type SettlementBreakdown struct {
	GrossCents          int64
	InterchangeBps      int64
	InterchangeFeeCents int64
	AcquirerBps         int64
	AcquirerFeeCents    int64
	TotalFeeCents       int64
	TotalIVACents       int64
	NetMerchantCents    int64
}

// InterchangeCalculator calculates MIF and acquirer discount rates with strict integer half-up rounding.
type InterchangeCalculator struct {
	AcquirerMarkupBps int64
}

// NewInterchangeCalculator creates an instance with a given acquirer markup in basis points.
func NewInterchangeCalculator(acquirerMarkupBps int64) *InterchangeCalculator {
	return &InterchangeCalculator{
		AcquirerMarkupBps: acquirerMarkupBps,
	}
}

// roundHalfUp performs pure 64-bit integer division with round-half-up semantics: (n + d/2) / d.
func roundHalfUp(numerator, denominator int64) int64 {
	if numerator < 0 {
		return (numerator - denominator/2) / denominator
	}
	return (numerator + denominator/2) / denominator
}

// CalculateDebit calculates debit interchange according to Banxico Circular 3/2012 by MCC.
func (c *InterchangeCalculator) CalculateDebit(grossCents int64, mcc int) (*SettlementBreakdown, error) {
	if grossCents <= 0 {
		return nil, ErrInvalidGrossAmount
	}

	var interchangeBps int64
	switch mcc {
	case 5541, 5542:
		interchangeBps = DebitBpsFuel
	case 5411:
		interchangeBps = DebitBpsSupermarket
	case 8211, 8220:
		interchangeBps = DebitBpsEducation
	default:
		interchangeBps = DebitBpsGeneral
	}

	return c.buildBreakdown(grossCents, interchangeBps)
}

// CalculateCredit calculates credit interchange by segment/tier (Classic, Gold, Platinum, Corporate).
func (c *InterchangeCalculator) CalculateCredit(grossCents int64, tier CardTier) (*SettlementBreakdown, error) {
	if grossCents <= 0 {
		return nil, ErrInvalidGrossAmount
	}

	var interchangeBps int64
	switch tier {
	case TierClassic:
		interchangeBps = CreditBpsClassic
	case TierGold:
		interchangeBps = CreditBpsGold
	case TierPlatinum:
		interchangeBps = CreditBpsPlatinum
	case TierCorporate:
		interchangeBps = CreditBpsCorporate
	default:
		return nil, ErrInvalidCardTier
	}

	return c.buildBreakdown(grossCents, interchangeBps)
}

func (c *InterchangeCalculator) buildBreakdown(grossCents, interchangeBps int64) (*SettlementBreakdown, error) {
	// 1. Interchange Fee: (Gross * bps + 5000) / 10000
	interchangeFeeCents := roundHalfUp(grossCents*interchangeBps, 10000)

	// 2. Acquirer Markup Fee: (Gross * bps + 5000) / 10000
	acquirerFeeCents := roundHalfUp(grossCents*c.AcquirerMarkupBps, 10000)

	// 3. Total Fee: Interchange + Acquirer
	totalFeeCents := interchangeFeeCents + acquirerFeeCents

	// 4. Mexican IVA (16%): (TotalFee * 16 + 50) / 100
	totalIVACents := roundHalfUp(totalFeeCents*IVAPercentage, 100)

	// 5. Net Merchant Payout: Gross - TotalFee - TotalIVA
	netMerchantCents := grossCents - totalFeeCents - totalIVACents

	return &SettlementBreakdown{
		GrossCents:          grossCents,
		InterchangeBps:      interchangeBps,
		InterchangeFeeCents: interchangeFeeCents,
		AcquirerBps:         c.AcquirerMarkupBps,
		AcquirerFeeCents:    acquirerFeeCents,
		TotalFeeCents:       totalFeeCents,
		TotalIVACents:       totalIVACents,
		NetMerchantCents:    netMerchantCents,
	}, nil
}
