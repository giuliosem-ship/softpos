package payments

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"softpos/pkg/stp"
)

var (
	ErrInvalidCoDiAmount  = errors.New("invalid CoDi payment amount")
	ErrInvalidCoDiPayload = errors.New("malformed or invalid CoDi payload")
	ErrExpiredCoDiRequest = errors.New("CoDi payment request has expired")
	ErrInvalidCLABE       = errors.New("invalid destination CLABE for CoDi")
)

var trackingKeyRegex = regexp.MustCompile(`^[A-Za-z0-9]{1,30}$`)

type CoDiRequest struct {
	Version         int
	SellerCLABE     string
	AmountCents     int64
	Concept         string
	Reference       string
	Expiration      time.Time
	InstitutionCode string
}

type CoDiConfirmation struct {
	TrackingKey         string    `json:"tracking_key"`
	SellerCLABE         string    `json:"seller_clabe"`
	BuyerCLABE          string    `json:"buyer_clabe"`
	AmountCents         int64     `json:"amount_cents"`
	Timestamp           time.Time `json:"timestamp"`
	Status              string    `json:"status"`
	BanxicoApprovalCode string    `json:"banxico_approval_code"`
}

type codiWirePayload struct {
	Version         int    `json:"v"`
	InstitutionCode string `json:"ic"`
	SellerCLABE     string `json:"c"`
	Amount          string `json:"m"`
	Concept         string `json:"cp"`
	Reference       string `json:"rf"`
	ExpirationUnix  int64  `json:"exp"`
}

// GenerateCoDiString validates and encodes a Banxico CoDi QR request using integer-only currency formatting.
func GenerateCoDiString(req CoDiRequest) (string, error) {
	if req.AmountCents <= 0 {
		return "", ErrInvalidCoDiAmount
	}
	if !stp.ValidateCLABE(req.SellerCLABE) {
		return "", ErrInvalidCLABE
	}
	if req.Expiration.IsZero() || !req.Expiration.After(time.Now()) {
		return "", ErrExpiredCoDiRequest
	}

	version := req.Version
	if version == 0 {
		version = 1
	}
	instCode := req.InstitutionCode
	if instCode == "" {
		instCode = "646"
	}

	amountStr := fmt.Sprintf("%d.%02d", req.AmountCents/100, req.AmountCents%100)

	wire := codiWirePayload{
		Version:         version,
		InstitutionCode: instCode,
		SellerCLABE:     req.SellerCLABE,
		Amount:          amountStr,
		Concept:         req.Concept,
		Reference:       req.Reference,
		ExpirationUnix:  req.Expiration.Unix(),
	}

	bytes, err := json.Marshal(wire)
	if err != nil {
		return "", ErrInvalidCoDiPayload
	}
	return string(bytes), nil
}

// ParseCoDiString decodes and validates a CoDi QR string back into a CoDiRequest without float conversion.
func ParseCoDiString(raw string) (CoDiRequest, error) {
	var wire codiWirePayload
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		return CoDiRequest{}, ErrInvalidCoDiPayload
	}

	if !stp.ValidateCLABE(wire.SellerCLABE) {
		return CoDiRequest{}, ErrInvalidCLABE
	}

	amountCents, err := parseDecimalAmountToCents(wire.Amount)
	if err != nil || amountCents <= 0 {
		return CoDiRequest{}, ErrInvalidCoDiAmount
	}

	expTime := time.Unix(wire.ExpirationUnix, 0)
	if !expTime.After(time.Now()) {
		return CoDiRequest{}, ErrExpiredCoDiRequest
	}

	return CoDiRequest{
		Version:         wire.Version,
		SellerCLABE:     wire.SellerCLABE,
		AmountCents:     amountCents,
		Concept:         wire.Concept,
		Reference:       wire.Reference,
		Expiration:      expTime,
		InstitutionCode: wire.InstitutionCode,
	}, nil
}

func parseDecimalAmountToCents(amountStr string) (int64, error) {
	parts := strings.Split(strings.TrimSpace(amountStr), ".")
	if len(parts) == 0 || len(parts) > 2 {
		return 0, ErrInvalidCoDiAmount
	}

	pesos, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || pesos < 0 {
		return 0, ErrInvalidCoDiAmount
	}

	var centavos int64
	if len(parts) == 2 {
		frac := parts[1]
		if len(frac) == 1 {
			frac += "0"
		} else if len(frac) != 2 {
			return 0, ErrInvalidCoDiAmount
		}
		centavos, err = strconv.ParseInt(frac, 10, 64)
		if err != nil || centavos < 0 {
			return 0, ErrInvalidCoDiAmount
		}
	}

	return pesos*100 + centavos, nil
}

// ValidateCoDiConfirmation parses and verifies an inbound CoDi confirmation notification.
func ValidateCoDiConfirmation(data []byte) (*CoDiConfirmation, error) {
	var conf CoDiConfirmation
	if err := json.Unmarshal(data, &conf); err != nil {
		return nil, ErrInvalidCoDiPayload
	}

	if conf.Status != "APROBADO" {
		return nil, ErrInvalidCoDiPayload
	}

	if !trackingKeyRegex.MatchString(conf.TrackingKey) {
		return nil, ErrInvalidCoDiPayload
	}

	if conf.SellerCLABE != "" && !stp.ValidateCLABE(conf.SellerCLABE) {
		return nil, ErrInvalidCLABE
	}

	if conf.AmountCents <= 0 {
		return nil, ErrInvalidCoDiAmount
	}

	return &conf, nil
}

// CoDiService manages CoDi QR payment requests and confirmations safely across goroutines.
type CoDiService struct {
	mu            sync.RWMutex
	requests      map[string]CoDiRequest
	confirmations map[string]CoDiConfirmation
}

func NewCoDiService() *CoDiService {
	return &CoDiService{
		requests:      make(map[string]CoDiRequest),
		confirmations: make(map[string]CoDiConfirmation),
	}
}

func (s *CoDiService) CreatePaymentRequest(clabe string, amountCents int64, concept string, ref string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	req := CoDiRequest{
		Version:         1,
		SellerCLABE:     clabe,
		AmountCents:     amountCents,
		Concept:         concept,
		Reference:       ref,
		Expiration:      time.Now().Add(ttl),
		InstitutionCode: "646",
	}

	qrStr, err := GenerateCoDiString(req)
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests[ref] = req

	return qrStr, nil
}

func (s *CoDiService) ConfirmPayment(conf *CoDiConfirmation) error {
	if conf == nil {
		return ErrInvalidCoDiPayload
	}
	if conf.Status != "APROBADO" || !trackingKeyRegex.MatchString(conf.TrackingKey) {
		return ErrInvalidCoDiPayload
	}
	if conf.AmountCents <= 0 {
		return ErrInvalidCoDiAmount
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.confirmations[conf.TrackingKey] = *conf
	return nil
}
