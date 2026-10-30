package stp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

var (
	ErrSTPOrderRejected = errors.New("STP rejected registraOrden request")
	ErrSTPNetworkError  = errors.New("STP outbound HTTP communication failed")
	ErrInvalidSTPOrder  = errors.New("invalid STP order parameters")
)

// STPOrderRequest contains the fields required to dispatch an outbound SPEI payment via STP registraOrden.
type STPOrderRequest struct {
	InstitucionContraparte string
	Empresa                string
	FechaOperacion         string // YYYYMMDD
	ClaveRastreo           string
	InstitucionOperante    string // Default "90646"
	AmountCents            int64
	TipoPago               int // Default 1 (Tercero a Tercero)
	CuentaOrdenante        string
	NombreBeneficiario     string
	CuentaBeneficiario     string
	RfcCurpBeneficiario    string
	ConceptoPago           string
}

// STPOrderResponse represents the result returned by STP's registraOrden endpoint.
type STPOrderResponse struct {
	STPOrderID       int64
	ClaveRastreo     string
	Signature        string
	DescripcionError string
}

// STPClient defines the outbound contract for registering SPEI orders with STP.
type STPClient interface {
	RegisterOrder(ctx context.Context, order STPOrderRequest) (*STPOrderResponse, error)
}

type stpRegistraOrdenWireRequest struct {
	InstitucionContraparte string      `json:"institucionContraparte"`
	Empresa                string      `json:"empresa"`
	FechaOperacion         string      `json:"fechaOperacion"`
	ClaveRastreo           string      `json:"claveRastreo"`
	InstitucionOperante    string      `json:"institucionOperante"`
	Monto                  json.Number `json:"monto"`
	TipoPago               int         `json:"tipoPago"`
	CuentaOrdenante        string      `json:"cuentaOrdenante"`
	NombreBeneficiario     string      `json:"nombreBeneficiario"`
	CuentaBeneficiario     string      `json:"cuentaBeneficiario"`
	RfcCurpBeneficiario    string      `json:"rfcCurpBeneficiario"`
	ConceptoPago           string      `json:"conceptoPago"`
	Firma                  string      `json:"firma"`
}

type stpRegistraOrdenWireResponse struct {
	Resultado struct {
		ID               int64  `json:"id"`
		DescripcionError string `json:"descripcionError"`
	} `json:"resultado"`
}

func normalizeOrderDefaults(order *STPOrderRequest) error {
	if order.AmountCents <= 0 || order.ClaveRastreo == "" || !ValidateCLABE(order.CuentaBeneficiario) {
		return ErrInvalidSTPOrder
	}
	if order.Empresa == "" {
		order.Empresa = "SOFTPOS_MX"
	}
	if order.FechaOperacion == "" {
		order.FechaOperacion = time.Now().UTC().Format("20060102")
	}
	if order.InstitucionOperante == "" {
		order.InstitucionOperante = "90646"
	}
	if order.InstitucionContraparte == "" && len(order.CuentaBeneficiario) >= 3 {
		order.InstitucionContraparte = "40" + order.CuentaBeneficiario[:3]
	}
	if order.TipoPago == 0 {
		order.TipoPago = 1
	}
	if order.CuentaOrdenante == "" {
		order.CuentaOrdenante = "646180000000000007"
	}
	if order.RfcCurpBeneficiario == "" {
		order.RfcCurpBeneficiario = "ND"
	}
	return nil
}

// HTTPSTPClient signs and sends registraOrden requests over HTTP to STP's REST gateway.
type HTTPSTPClient struct {
	baseURL    string
	signer     *STPSigner
	httpClient *http.Client
}

func NewHTTPSTPClient(baseURL string, signer *STPSigner, timeout time.Duration) *HTTPSTPClient {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &HTTPSTPClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		signer:  signer,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

func (c *HTTPSTPClient) RegisterOrder(ctx context.Context, order STPOrderRequest) (*STPOrderResponse, error) {
	if err := normalizeOrderDefaults(&order); err != nil {
		return nil, err
	}
	if c.signer == nil {
		return nil, errors.New("STP signer is required")
	}

	canonical := c.signer.BuildRegistraOrdenCanonicalString(
		order.InstitucionContraparte,
		order.Empresa,
		order.FechaOperacion,
		order.ClaveRastreo,
		order.InstitucionOperante,
		order.AmountCents,
		order.TipoPago,
		order.CuentaOrdenante,
		order.CuentaBeneficiario,
		order.RfcCurpBeneficiario,
		order.ConceptoPago,
	)

	firma, err := c.signer.SignCanonical(canonical)
	if err != nil {
		return nil, err
	}

	montoStr := fmt.Sprintf("%d.%02d", order.AmountCents/100, order.AmountCents%100)
	wireReq := stpRegistraOrdenWireRequest{
		InstitucionContraparte: order.InstitucionContraparte,
		Empresa:                order.Empresa,
		FechaOperacion:         order.FechaOperacion,
		ClaveRastreo:           order.ClaveRastreo,
		InstitucionOperante:    order.InstitucionOperante,
		Monto:                  json.Number(montoStr),
		TipoPago:               order.TipoPago,
		CuentaOrdenante:        order.CuentaOrdenante,
		NombreBeneficiario:     order.NombreBeneficiario,
		CuentaBeneficiario:     order.CuentaBeneficiario,
		RfcCurpBeneficiario:    order.RfcCurpBeneficiario,
		ConceptoPago:           order.ConceptoPago,
		Firma:                  firma,
	}

	bodyBytes, err := json.Marshal(wireReq)
	if err != nil {
		return nil, err
	}

	endpoint := c.baseURL + "/speiws/rest/ordenPago/registra"
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSTPNetworkError, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSTPNetworkError, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("%w: HTTP %d", ErrSTPNetworkError, resp.StatusCode)
	}

	var wireResp stpRegistraOrdenWireResponse
	if err := json.NewDecoder(resp.Body).Decode(&wireResp); err != nil {
		return nil, fmt.Errorf("%w: invalid response JSON: %v", ErrSTPNetworkError, err)
	}

	if resp.StatusCode >= 400 || wireResp.Resultado.ID <= 0 || wireResp.Resultado.DescripcionError != "" {
		return &STPOrderResponse{
			STPOrderID:       wireResp.Resultado.ID,
			ClaveRastreo:     order.ClaveRastreo,
			Signature:        firma,
			DescripcionError: wireResp.Resultado.DescripcionError,
		}, fmt.Errorf("%w: %s", ErrSTPOrderRejected, wireResp.Resultado.DescripcionError)
	}

	return &STPOrderResponse{
		STPOrderID:   wireResp.Resultado.ID,
		ClaveRastreo: order.ClaveRastreo,
		Signature:    firma,
	}, nil
}

// MockSTPClient provides deterministic local signing and validation for tests and default Server initialization.
type MockSTPClient struct {
	signer        *STPSigner
	seq           atomic.Int64
	rejectMessage string
}

func NewMockSTPClient(signer *STPSigner, rejectMessage string) *MockSTPClient {
	m := &MockSTPClient{
		signer:        signer,
		rejectMessage: rejectMessage,
	}
	m.seq.Store(700000)
	return m
}

func (m *MockSTPClient) RegisterOrder(ctx context.Context, order STPOrderRequest) (*STPOrderResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSTPNetworkError, err)
	}
	if err := normalizeOrderDefaults(&order); err != nil {
		return nil, err
	}

	var sig string
	if m.signer != nil {
		canonical := m.signer.BuildRegistraOrdenCanonicalString(
			order.InstitucionContraparte,
			order.Empresa,
			order.FechaOperacion,
			order.ClaveRastreo,
			order.InstitucionOperante,
			order.AmountCents,
			order.TipoPago,
			order.CuentaOrdenante,
			order.CuentaBeneficiario,
			order.RfcCurpBeneficiario,
			order.ConceptoPago,
		)
		var err error
		sig, err = m.signer.SignCanonical(canonical)
		if err != nil {
			return nil, err
		}
	}

	if m.rejectMessage != "" {
		return &STPOrderResponse{
			STPOrderID:       -1,
			ClaveRastreo:     order.ClaveRastreo,
			Signature:        sig,
			DescripcionError: m.rejectMessage,
		}, fmt.Errorf("%w: %s", ErrSTPOrderRejected, m.rejectMessage)
	}

	id := m.seq.Add(1)
	return &STPOrderResponse{
		STPOrderID:   id,
		ClaveRastreo: order.ClaveRastreo,
		Signature:    sig,
	}, nil
}
