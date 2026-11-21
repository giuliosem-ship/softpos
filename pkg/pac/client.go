package pac

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

var (
	ErrPACUnavailable     = errors.New("all configured PAC providers are unavailable")
	ErrSATValidationError = errors.New("SAT rejected CFDI document")
	ErrMalformedXML       = errors.New("invalid or unparseable XML for stamping")
)

type TimbreFiscalDigital struct {
	UUID             string
	FechaTimbrado    time.Time
	RfcProvCertif    string
	SelloCFD         string
	NoCertificadoSAT string
	SelloSAT         string
	Version          string
}

type PACResponse struct {
	Success      bool
	Timbre       *TimbreFiscalDigital
	ErrorCode    string
	ErrorMessage string
}

// PACClient is the port to a Proveedor Autorizado de Certificación. A real
// adapter (docs/CFDI.md, "PAC adapter contract") must implement all three
// operations against its sandbox/production API.
type PACClient interface {
	// Stamp certifies a sealed CFDI 4.0 and returns its TimbreFiscalDigital.
	// It must be idempotent on the sello: a CFDI already stamped returns the
	// existing timbre (SAT/PAC error 307), never a second UUID.
	// ErrSATValidationError = deterministic rejection (do not retry),
	// ErrPACUnavailable = transient (retry / failover).
	Stamp(ctx context.Context, signedXML []byte) (*PACResponse, error)
	// Cancel requests the cancellation of a stamped CFDI (signed with the
	// emisor's CSD).
	Cancel(ctx context.Context, req CancelRequest) (*CancelResponse, error)
	// Status queries the SAT status of a CFDI (estado, cancellation state).
	Status(ctx context.Context, req StatusRequest) (*StatusResponse, error)
}

// CancelRequest is a cancellation (Anexo 20 / Art. 29-A CFF).
type CancelRequest struct {
	EmisorRFC        string
	ReceptorRFC      string
	UUID             string
	TotalCents       int64
	Motivo           string // 01 | 02 | 03 | 04
	FolioSustitucion string // UUID of the replacement (motivo 01)
	CertDER          []byte // CSD certificate
	KeyPKCS8         []byte // CSD private key (plaintext, wiped by the caller)
}

// CancelState is the outcome of a cancellation request.
type CancelState string

const (
	// CancelCanceled: canceled immediately (no receptor acceptance needed).
	CancelCanceled CancelState = "CANCELED"
	// CancelInProcess: waiting for the receptor's acceptance (up to 72 h).
	CancelInProcess CancelState = "IN_PROCESS"
	// CancelRejected: the receptor rejected it, or the CFDI is not cancelable.
	CancelRejected CancelState = "REJECTED"
)

// CancelResponse is the PAC answer to a cancellation.
type CancelResponse struct {
	State     CancelState
	Acuse     string // acknowledgment (XML) when available
	ErrorCode string
}

// StatusRequest queries a CFDI (SAT ConsultaCFDIService parameters).
type StatusRequest struct {
	EmisorRFC   string
	ReceptorRFC string
	UUID        string
	TotalCents  int64
}

// SAT status strings (ConsultaCFDIService).
const (
	EstadoVigente            = "Vigente"
	EstadoCancelado          = "Cancelado"
	EstadoNoEncontrado       = "No Encontrado"
	CancelacionEnProceso     = "En proceso"
	CancelacionRechazada     = "Solicitud rechazada"
	CancelacionConAceptacion = "Cancelado con aceptación"
	CancelacionSinAceptacion = "Cancelado sin aceptación"
	CancelacionPlazoVencido  = "Plazo vencido"
)

// StatusResponse is the SAT status of a CFDI.
type StatusResponse struct {
	Estado             string // Vigente | Cancelado | No Encontrado
	EsCancelable       string
	EstatusCancelacion string // "" | En proceso | Solicitud rechazada | Cancelado con/sin aceptación | Plazo vencido
}

// MockSAT simulates the SAT side of the mock PACs (shared by the primary
// and fallback mocks so status queries see every stamp).
type MockSAT struct {
	mu    sync.Mutex
	cfdis map[string]*mockCFDI
	now   func() time.Time
}

type mockCFDI struct {
	totalCents  int64
	receptorRFC string
	estado      string
	cancel      string
	requestedAt time.Time
}

// NewMockSAT returns an empty simulated SAT.
func NewMockSAT() *MockSAT { return &MockSAT{cfdis: map[string]*mockCFDI{}, now: time.Now} }

// SetClock overrides the simulated SAT clock (tests).
func (m *MockSAT) SetClock(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
}

// ResolveCancellation simulates the receptor accepting or rejecting a
// pending cancellation.
func (m *MockSAT) ResolveCancellation(uuid string, accept bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.cfdis[uuid]
	if !ok || c.cancel != CancelacionEnProceso {
		return false
	}
	if accept {
		c.estado, c.cancel = EstadoCancelado, CancelacionConAceptacion
	} else {
		c.cancel = CancelacionRechazada
	}
	return true
}

// mockNoAcceptanceLimitCents: CFDIs up to $1,000 MXN (and those to the
// general public) are canceled without the receptor's acceptance.
const mockNoAcceptanceLimitCents = 100000

type MockPACClient struct {
	simulateNetworkFailure bool
	simulatedErrorCode     string
	sat                    *MockSAT
}

func NewMockPACClient(simulateNetworkFailure bool, simulatedErrorCode string) *MockPACClient {
	return NewMockPACClientWithSAT(simulateNetworkFailure, simulatedErrorCode, NewMockSAT())
}

// NewMockPACClientWithSAT builds a mock PAC over a shared simulated SAT.
func NewMockPACClientWithSAT(simulateNetworkFailure bool, simulatedErrorCode string, sat *MockSAT) *MockPACClient {
	if sat == nil {
		sat = NewMockSAT()
	}
	return &MockPACClient{
		simulateNetworkFailure: simulateNetworkFailure,
		simulatedErrorCode:     simulatedErrorCode,
		sat:                    sat,
	}
}

// SAT returns the simulated SAT of the mock.
func (m *MockPACClient) SAT() *MockSAT { return m.sat }

// mockComprobante reads the attributes the mock needs from a sealed CFDI.
type mockComprobante struct {
	Sello    string `xml:"Sello,attr"`
	Total    string `xml:"Total,attr"`
	Receptor struct {
		Rfc string `xml:"Rfc,attr"`
	} `xml:"Receptor"`
}

func parseCents(s string) int64 {
	whole, frac, _ := strings.Cut(strings.TrimSpace(s), ".")
	var w, f int64
	_, _ = fmt.Sscanf(whole, "%d", &w)
	frac = (frac + "00")[:2]
	_, _ = fmt.Sscanf(frac, "%d", &f)
	return w*100 + f
}

func generateUUIDv4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // Version 4
	b[8] = (b[8] & 0x3f) | 0x80 // Variant RFC 4122
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (m *MockPACClient) Stamp(ctx context.Context, signedXML []byte) (*PACResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPACUnavailable, err)
	}

	if len(signedXML) == 0 || !strings.Contains(string(signedXML), "Comprobante") {
		return nil, ErrMalformedXML
	}

	if m.simulateNetworkFailure {
		code := m.simulatedErrorCode
		if code == "" {
			code = "NETWORK_TIMEOUT"
		}
		return &PACResponse{
			Success:      false,
			ErrorCode:    code,
			ErrorMessage: "simulated network outage or PAC 5xx error",
		}, ErrPACUnavailable
	}

	if m.simulatedErrorCode != "" {
		return &PACResponse{
			Success:      false,
			ErrorCode:    m.simulatedErrorCode,
			ErrorMessage: "SAT business validation error: " + m.simulatedErrorCode,
		}, ErrSATValidationError
	}

	var comp mockComprobante
	_ = xml.Unmarshal(signedXML, &comp)
	hash := sha256.Sum256(signedXML)
	selloSAT := base64.StdEncoding.EncodeToString(hash[:])
	selloCFD := comp.Sello
	if selloCFD == "" {
		selloCFD = selloSAT
	}

	timbre := &TimbreFiscalDigital{
		Version:          "1.1",
		UUID:             strings.ToUpper(generateUUIDv4()),
		FechaTimbrado:    time.Now().UTC(),
		RfcProvCertif:    "SAT970701NN3",
		SelloCFD:         selloCFD,
		NoCertificadoSAT: "30001000000400002434",
		SelloSAT:         selloSAT,
	}
	m.sat.mu.Lock()
	m.sat.cfdis[timbre.UUID] = &mockCFDI{totalCents: parseCents(comp.Total), receptorRFC: comp.Receptor.Rfc, estado: EstadoVigente}
	m.sat.mu.Unlock()

	return &PACResponse{
		Success: true,
		Timbre:  timbre,
	}, nil
}

// Cancel simulates the SAT rules: up to $1,000 MXN or to the general public
// -> canceled at once; otherwise the receptor has 72 h to accept/reject.
func (m *MockPACClient) Cancel(ctx context.Context, req CancelRequest) (*CancelResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPACUnavailable, err)
	}
	if m.simulateNetworkFailure {
		return nil, ErrPACUnavailable
	}
	if len(req.CertDER) == 0 || len(req.KeyPKCS8) == 0 {
		return &CancelResponse{State: CancelRejected, ErrorCode: "CSD_REQUIRED"}, ErrSATValidationError
	}
	m.sat.mu.Lock()
	defer m.sat.mu.Unlock()
	c, ok := m.sat.cfdis[req.UUID]
	switch {
	case !ok:
		return &CancelResponse{State: CancelRejected, ErrorCode: "UUID_NOT_FOUND"}, ErrSATValidationError
	case c.estado == EstadoCancelado:
		return &CancelResponse{State: CancelCanceled}, nil
	case c.cancel == CancelacionEnProceso:
		return &CancelResponse{State: CancelInProcess}, nil
	}
	c.requestedAt = m.sat.now()
	if c.totalCents <= mockNoAcceptanceLimitCents || c.receptorRFC == "XAXX010101000" || c.receptorRFC == "XEXX010101000" {
		c.estado, c.cancel = EstadoCancelado, CancelacionSinAceptacion
		return &CancelResponse{State: CancelCanceled, Acuse: "<Acuse UUID=\"" + req.UUID + "\"/>"}, nil
	}
	c.cancel = CancelacionEnProceso
	return &CancelResponse{State: CancelInProcess, Acuse: "<Acuse UUID=\"" + req.UUID + "\"/>"}, nil
}

// Status reports the simulated SAT state (a pending request older than
// 72 h is canceled by "plazo vencido").
func (m *MockPACClient) Status(ctx context.Context, req StatusRequest) (*StatusResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPACUnavailable, err)
	}
	if m.simulateNetworkFailure {
		return nil, ErrPACUnavailable
	}
	m.sat.mu.Lock()
	defer m.sat.mu.Unlock()
	c, ok := m.sat.cfdis[req.UUID]
	if !ok {
		return &StatusResponse{Estado: EstadoNoEncontrado}, nil
	}
	if c.cancel == CancelacionEnProceso && m.sat.now().Sub(c.requestedAt) > 72*time.Hour {
		c.estado, c.cancel = EstadoCancelado, CancelacionPlazoVencido
	}
	return &StatusResponse{Estado: c.estado, EsCancelable: "Cancelable con aceptación", EstatusCancelacion: c.cancel}, nil
}

type MultiPACOrchestrator struct {
	primary  PACClient
	fallback PACClient
	timeout  time.Duration
}

func NewMultiPACOrchestrator(primary PACClient, fallback PACClient, timeout time.Duration) *MultiPACOrchestrator {
	if timeout <= 0 {
		timeout = 1500 * time.Millisecond
	}
	return &MultiPACOrchestrator{
		primary:  primary,
		fallback: fallback,
		timeout:  timeout,
	}
}

func (o *MultiPACOrchestrator) Stamp(ctx context.Context, signedXML []byte) (*PACResponse, error) {
	if o.primary == nil && o.fallback == nil {
		return nil, ErrPACUnavailable
	}

	if o.primary != nil {
		pCtx, cancel := context.WithTimeout(ctx, o.timeout)
		resp, err := o.primary.Stamp(pCtx, signedXML)
		cancel()

		if err == nil && resp != nil && resp.Success {
			return resp, nil
		}

		// Business rule: Do NOT failover on deterministic SAT validation or malformed XML errors
		if errors.Is(err, ErrSATValidationError) || errors.Is(err, ErrMalformedXML) {
			return resp, err
		}
	}

	if o.fallback != nil {
		fCtx, cancel := context.WithTimeout(ctx, o.timeout)
		resp, err := o.fallback.Stamp(fCtx, signedXML)
		cancel()

		if err == nil && resp != nil && resp.Success {
			return resp, nil
		}
		if errors.Is(err, ErrSATValidationError) || errors.Is(err, ErrMalformedXML) {
			return resp, err
		}
	}

	return nil, ErrPACUnavailable
}

// Cancel tries the primary PAC, then the fallback on transient errors.
func (o *MultiPACOrchestrator) Cancel(ctx context.Context, req CancelRequest) (*CancelResponse, error) {
	var lastErr error = ErrPACUnavailable
	for _, c := range []PACClient{o.primary, o.fallback} {
		if c == nil {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, o.timeout)
		resp, err := c.Cancel(cctx, req)
		cancel()
		if err == nil || errors.Is(err, ErrSATValidationError) {
			return resp, err
		}
		lastErr = err
	}
	return nil, lastErr
}

// Status tries the primary PAC, then the fallback; a CFDI unknown to the
// primary is looked up on the fallback too.
func (o *MultiPACOrchestrator) Status(ctx context.Context, req StatusRequest) (*StatusResponse, error) {
	var last *StatusResponse
	var lastErr error = ErrPACUnavailable
	for _, c := range []PACClient{o.primary, o.fallback} {
		if c == nil {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, o.timeout)
		resp, err := c.Status(cctx, req)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.Estado != EstadoNoEncontrado {
			return resp, nil
		}
		last, lastErr = resp, nil
	}
	return last, lastErr
}

// InjectTimbreFiscal injects the <cfdi:Complemento><tfd:TimbreFiscalDigital .../></cfdi:Complemento> node into a signed CFDI 4.0 XML.
func InjectTimbreFiscal(signedXML []byte, timbre *TimbreFiscalDigital) ([]byte, error) {
	if len(signedXML) == 0 || timbre == nil || timbre.UUID == "" {
		return nil, ErrMalformedXML
	}

	xmlStr := string(signedXML)
	closingTag := "</cfdi:Comprobante>"
	idx := strings.LastIndex(xmlStr, closingTag)
	if idx == -1 {
		return nil, ErrMalformedXML
	}

	version := timbre.Version
	if version == "" {
		version = "1.1"
	}
	fechaStr := timbre.FechaTimbrado.UTC().Format("2006-01-02T15:04:05")

	complementoXML := fmt.Sprintf(
		`<cfdi:Complemento><tfd:TimbreFiscalDigital xmlns:tfd="http://www.sat.gob.mx/TimbreFiscalDigital" xsi:schemaLocation="http://www.sat.gob.mx/TimbreFiscalDigital http://www.sat.gob.mx/sitio_internet/cfd/TimbreFiscalDigital/TimbreFiscalDigitalv11.xsd" Version="%s" UUID="%s" FechaTimbrado="%s" RfcProvCertif="%s" SelloCFD="%s" NoCertificadoSAT="%s" SelloSAT="%s"/></cfdi:Complemento>`,
		version,
		timbre.UUID,
		fechaStr,
		timbre.RfcProvCertif,
		timbre.SelloCFD,
		timbre.NoCertificadoSAT,
		timbre.SelloSAT,
	)

	stamped := xmlStr[:idx] + complementoXML + xmlStr[idx:]
	return []byte(stamped), nil
}

// TFDCadenaOriginal is the cadena original del complemento de certificación
// digital del SAT (TimbreFiscalDigital 1.1), printed on the representación
// impresa.
func TFDCadenaOriginal(t *TimbreFiscalDigital) string {
	if t == nil {
		return ""
	}
	version := t.Version
	if version == "" {
		version = "1.1"
	}
	return "||" + strings.Join([]string{version, t.UUID, t.FechaTimbrado.UTC().Format("2006-01-02T15:04:05"),
		t.RfcProvCertif, t.SelloCFD, t.NoCertificadoSAT}, "|") + "||"
}
