package pac

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
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

type PACClient interface {
	Stamp(ctx context.Context, signedXML []byte) (*PACResponse, error)
}

type MockPACClient struct {
	simulateNetworkFailure bool
	simulatedErrorCode     string
}

func NewMockPACClient(simulateNetworkFailure bool, simulatedErrorCode string) *MockPACClient {
	return &MockPACClient{
		simulateNetworkFailure: simulateNetworkFailure,
		simulatedErrorCode:     simulatedErrorCode,
	}
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

	hash := sha256.Sum256(signedXML)
	selloSAT := base64.StdEncoding.EncodeToString(hash[:])

	timbre := &TimbreFiscalDigital{
		Version:          "1.1",
		UUID:             generateUUIDv4(),
		FechaTimbrado:    time.Now().UTC(),
		RfcProvCertif:    "SAT970701NN3",
		SelloCFD:         selloSAT,
		NoCertificadoSAT: "30001000000400002434",
		SelloSAT:         selloSAT,
	}

	return &PACResponse{
		Success: true,
		Timbre:  timbre,
	}, nil
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
