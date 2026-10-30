package payments

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ErrInvalidTLV            = errors.New("invalid EMV TLV data format")
	ErrUnsupportedCryptogram = errors.New("unsupported cryptogram type")
	ErrExpiredCard           = errors.New("card is expired")
	ErrInvalidCurrency       = errors.New("currency must be MXN (0484)")
	ErrInvalidAmount         = errors.New("amount must be positive")
	ErrInvalidStateTransition = errors.New("invalid payment state transition")
	ErrPaymentNotFound       = errors.New("payment not found")
	ErrDuplicatePayment      = errors.New("payment with this ID already exists")
)

type CryptogramType string

const (
	CryptogramARQC CryptogramType = "ARQC"
	CryptogramTC   CryptogramType = "TC"
	CryptogramAAC  CryptogramType = "AAC"
)

type PaymentStatus string

const (
	PaymentAuthorized PaymentStatus = "AUTHORIZED"
	PaymentSettled    PaymentStatus = "SETTLED"
	PaymentVoided     PaymentStatus = "VOIDED"
	PaymentRefunded   PaymentStatus = "REFUNDED"
)

type EMVTags struct {
	AmountCents    int64
	CurrencyCode   string
	CryptogramType CryptogramType
	CryptogramHex  string
	ATC            int
	PANMasked      string
	ExpirationDate string
	RawTags        map[string][]byte
}

type Payment struct {
	ID          string
	MerchantID  string
	AmountCents int64
	Status      PaymentStatus
	MaskedPAN   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ParseTLV decodes a BER-TLV encoded byte stream into a map of uppercase hex tag strings to value slices.
func ParseTLV(data []byte) (map[string][]byte, error) {
	if len(data) == 0 {
		return nil, ErrInvalidTLV
	}

	tags := make(map[string][]byte)
	i := 0
	n := len(data)

	for i < n {
		tagStart := i
		firstTagByte := data[i]
		i++

		// Multi-byte tag if lower 5 bits are all 1s (0x1F)
		if (firstTagByte & 0x1F) == 0x1F {
			if i >= n {
				return nil, ErrInvalidTLV
			}
			for (data[i] & 0x80) != 0 {
				i++
				if i >= n {
					return nil, ErrInvalidTLV
				}
			}
			i++ // consume final tag byte
		}

		tagHex := strings.ToUpper(hex.EncodeToString(data[tagStart:i]))

		// Parse length
		if i >= n {
			return nil, ErrInvalidTLV
		}
		lenByte := data[i]
		i++

		var length int
		if (lenByte & 0x80) == 0 {
			length = int(lenByte)
		} else {
			numLenBytes := int(lenByte & 0x7F)
			if numLenBytes == 0 || numLenBytes > 2 || i+numLenBytes > n {
				return nil, ErrInvalidTLV
			}
			for j := 0; j < numLenBytes; j++ {
				length = (length << 8) | int(data[i+j])
			}
			i += numLenBytes
		}

		if length < 0 || i+length > n {
			return nil, ErrInvalidTLV
		}

		val := make([]byte, length)
		copy(val, data[i:i+length])
		tags[tagHex] = val
		i += length
	}

	return tags, nil
}

// MaskPAN replaces inner digits between the first 6 (BIN) and last 4 digits with asterisks.
func MaskPAN(pan string) string {
	cleaned := strings.TrimSpace(pan)
	if len(cleaned) < 10 {
		return strings.Repeat("*", len(cleaned))
	}
	return cleaned[:6] + strings.Repeat("*", len(cleaned)-10) + cleaned[len(cleaned)-4:]
}

// ParseEMVTags parses a BER-TLV stream and extracts validated EMV L2 contactless fields.
func ParseEMVTags(rawTLV []byte) (EMVTags, error) {
	rawTags, err := ParseTLV(rawTLV)
	if err != nil {
		return EMVTags{}, err
	}

	result := EMVTags{
		RawTags: rawTags,
	}

	// 1. Tag 9F02: Amount, Authorized (Numeric BCD, 6 bytes -> 12 digits in cents)
	rawAmt, ok := rawTags["9F02"]
	if !ok || len(rawAmt) == 0 {
		return EMVTags{}, ErrInvalidAmount
	}
	amtDigits := hex.EncodeToString(rawAmt)
	amtCents, err := strconv.ParseInt(amtDigits, 10, 64)
	if err != nil || amtCents <= 0 {
		return EMVTags{}, ErrInvalidAmount
	}
	result.AmountCents = amtCents

	// 2. Tag 5F2A: Transaction Currency Code (ISO 4217 numeric, MXN = "0484")
	rawCurr, ok := rawTags["5F2A"]
	if !ok {
		return EMVTags{}, ErrInvalidCurrency
	}
	currCode := strings.ToUpper(hex.EncodeToString(rawCurr))
	if currCode != "0484" {
		return EMVTags{}, ErrInvalidCurrency
	}
	result.CurrencyCode = currCode

	// 3. Tag 9F27: Cryptogram Information Data (CID)
	rawCID, ok := rawTags["9F27"]
	if !ok || len(rawCID) != 1 {
		return EMVTags{}, ErrUnsupportedCryptogram
	}
	switch rawCID[0] & 0xC0 {
	case 0x80:
		result.CryptogramType = CryptogramARQC
	case 0x40:
		result.CryptogramType = CryptogramTC
	case 0x00:
		return EMVTags{}, ErrUnsupportedCryptogram
	default:
		return EMVTags{}, ErrUnsupportedCryptogram
	}

	// 4. Tag 9F26: Application Cryptogram (AC)
	if rawAC, ok := rawTags["9F26"]; ok {
		result.CryptogramHex = strings.ToUpper(hex.EncodeToString(rawAC))
	}

	// 5. Tag 9F36: Application Transaction Counter (ATC)
	if rawATC, ok := rawTags["9F36"]; ok {
		if len(rawATC) == 2 {
			result.ATC = int(binary.BigEndian.Uint16(rawATC))
		} else if len(rawATC) == 1 {
			result.ATC = int(rawATC[0])
		}
	}

	// 6. Tag 5A (PAN) or Tag 57 (Track 2 Equivalent Data)
	if rawPAN, ok := rawTags["5A"]; ok {
		panHex := strings.ToUpper(hex.EncodeToString(rawPAN))
		panClean := strings.TrimRight(panHex, "F")
		result.PANMasked = MaskPAN(panClean)
	} else if rawTrack2, ok := rawTags["57"]; ok {
		track2Hex := strings.ToUpper(hex.EncodeToString(rawTrack2))
		parts := strings.SplitN(track2Hex, "D", 2)
		if len(parts) > 0 {
			result.PANMasked = MaskPAN(strings.TrimRight(parts[0], "F"))
		}
	}

	// 7. Tag 5F24: Application Expiration Date (YYMM or YYMMDD BCD)
	if rawExp, ok := rawTags["5F24"]; ok {
		expHex := hex.EncodeToString(rawExp)
		if len(expHex) < 4 {
			return EMVTags{}, ErrInvalidTLV
		}
		yymm := expHex[:4]
		yy, errY := strconv.Atoi(yymm[:2])
		mm, errM := strconv.Atoi(yymm[2:4])
		if errY != nil || errM != nil || mm < 1 || mm > 12 {
			return EMVTags{}, ErrInvalidTLV
		}

		expYear := 2000 + yy
		now := time.Now()
		curYear := now.Year()
		curMonth := int(now.Month())

		if expYear < curYear || (expYear == curYear && mm < curMonth) {
			return EMVTags{}, ErrExpiredCard
		}
		result.ExpirationDate = yymm
	}

	return result, nil
}

// PaymentEngine manages card payment authorization and settlement state transitions safely.
type PaymentEngine struct {
	mu       sync.RWMutex
	payments map[string]*Payment
}

func NewPaymentEngine() *PaymentEngine {
	return &PaymentEngine{
		payments: make(map[string]*Payment),
	}
}

func (pe *PaymentEngine) Authorize(id string, merchantID string, emv EMVTags) (*Payment, error) {
	if id == "" || merchantID == "" {
		return nil, errors.New("payment id and merchant id are required")
	}
	if emv.AmountCents <= 0 {
		return nil, ErrInvalidAmount
	}
	if emv.CurrencyCode != "" && emv.CurrencyCode != "0484" {
		return nil, ErrInvalidCurrency
	}
	if emv.CryptogramType != CryptogramARQC && emv.CryptogramType != CryptogramTC {
		return nil, ErrUnsupportedCryptogram
	}

	pe.mu.Lock()
	defer pe.mu.Unlock()

	if _, exists := pe.payments[id]; exists {
		return nil, ErrDuplicatePayment
	}

	now := time.Now().UTC()
	p := &Payment{
		ID:          id,
		MerchantID:  merchantID,
		AmountCents: emv.AmountCents,
		Status:      PaymentAuthorized,
		MaskedPAN:   emv.PANMasked,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	pe.payments[id] = p

	copyPayment := *p
	return &copyPayment, nil
}

func (pe *PaymentEngine) Settle(id string) (*Payment, error) {
	pe.mu.Lock()
	defer pe.mu.Unlock()

	p, exists := pe.payments[id]
	if !exists {
		return nil, ErrPaymentNotFound
	}
	if p.Status != PaymentAuthorized {
		return nil, ErrInvalidStateTransition
	}

	p.Status = PaymentSettled
	p.UpdatedAt = time.Now().UTC()

	copyPayment := *p
	return &copyPayment, nil
}

func (pe *PaymentEngine) Void(id string) (*Payment, error) {
	pe.mu.Lock()
	defer pe.mu.Unlock()

	p, exists := pe.payments[id]
	if !exists {
		return nil, ErrPaymentNotFound
	}
	if p.Status != PaymentAuthorized {
		return nil, ErrInvalidStateTransition
	}

	p.Status = PaymentVoided
	p.UpdatedAt = time.Now().UTC()

	copyPayment := *p
	return &copyPayment, nil
}

func (pe *PaymentEngine) Refund(id string, refundCents int64) (*Payment, error) {
	pe.mu.Lock()
	defer pe.mu.Unlock()

	p, exists := pe.payments[id]
	if !exists {
		return nil, ErrPaymentNotFound
	}
	if refundCents <= 0 || refundCents > p.AmountCents {
		return nil, ErrInvalidAmount
	}
	if p.Status != PaymentSettled {
		return nil, ErrInvalidStateTransition
	}

	p.Status = PaymentRefunded
	p.UpdatedAt = time.Now().UTC()

	copyPayment := *p
	return &copyPayment, nil
}

func (pe *PaymentEngine) GetPayment(id string) (*Payment, error) {
	pe.mu.RLock()
	defer pe.mu.RUnlock()

	p, exists := pe.payments[id]
	if !exists {
		return nil, ErrPaymentNotFound
	}

	copyPayment := *p
	return &copyPayment, nil
}
