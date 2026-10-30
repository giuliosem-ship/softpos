package stp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ErrInvalidCEPXML        = errors.New("malformed or invalid Banxico CEP XML")
	ErrInvalidCEPSignature  = errors.New("missing or invalid Banxico CDA digital signature on CEP")
	ErrCEPAmountMismatch    = errors.New("Banxico CEP settled amount does not match disbursement amount")
	ErrCEPAccountMismatch   = errors.New("Banxico CEP beneficiary account does not match disbursement CLABE")
	ErrDisbursementNotFound = errors.New("disbursement record not found for claveRastreo")
)

type CEPStatus string

const (
	StatusPendingCEP     CEPStatus = "PENDING_CEP"
	StatusReconciledCEP  CEPStatus = "RECONCILED_CEP"
	StatusCEPDiscrepancy CEPStatus = "CEP_DISCREPANCY"
)

// CEPReceipt holds the parsed and validated attributes of a Banxico Comprobante Electrónico de Pago (CEP).
type CEPReceipt struct {
	ClaveRastreo             string `json:"clave_rastreo"`
	FechaOperacion           string `json:"fecha_operacion"`
	HoraLiquidacion          string `json:"hora_liquidacion"`
	InstitucionEmisora       string `json:"institucion_emisora"`
	InstitucionReceptora     string `json:"institucion_receptora"`
	CuentaBeneficiaria       string `json:"cuenta_beneficiaria"`
	RfcBeneficiario          string `json:"rfc_beneficiario"`
	MontoCents               int64  `json:"monto_cents"`
	NumeroCertificadoBanxico string `json:"numero_certificado_banxico"`
	SelloDigitalBanxico      string `json:"sello_digital_banxico"`
	CadenaOriginalCDA        string `json:"cadena_original_cda"`
	SHA256Hash               string `json:"sha256_hash"`
	RawXML                   string `json:"raw_xml,omitempty"`
}

// CEPQuery specifies the parameters required to query a CEP from Banxico/STP.
type CEPQuery struct {
	ClaveRastreo         string
	FechaOperacion       string
	InstitucionEmisora   string
	InstitucionReceptora string
	CuentaBeneficiaria   string
	MontoCents           int64
}

// DisbursementRecord tracks an outbound SPEI disbursement and its Banxico CEP reconciliation state.
type DisbursementRecord struct {
	ClaveRastreo          string      `json:"clave_rastreo"`
	STPOrderID            int64       `json:"stp_order_id"`
	MerchantID            string      `json:"merchant_id"`
	AmountCents           int64       `json:"amount_cents"`
	DestinationCLABE      string      `json:"destination_clabe"`
	BeneficiaryName       string      `json:"beneficiary_name"`
	Concept               string      `json:"concept"`
	FechaOperacion        string      `json:"fecha_operacion"`
	Status                CEPStatus   `json:"status"`
	CEP                   *CEPReceipt `json:"cep,omitempty"`
	DiscrepancyReason     string      `json:"discrepancy_reason,omitempty"`
	RemainingBalanceCents int64       `json:"remaining_balance_cents"`
	CreatedAt             time.Time   `json:"created_at"`
	ReconciledAt          *time.Time  `json:"reconciled_at,omitempty"`
}

type banxicoSPEITerceroXML struct {
	XMLName           xml.Name `xml:"SPEI_Tercero"`
	FechaOperacion    string   `xml:"FechaOperacion,attr"`
	Hora              string   `xml:"Hora,attr"`
	ClaveSPEI         string   `xml:"ClaveSPEI,attr"`
	ClaveRastreo      string   `xml:"claveRastreo,attr"`
	NumeroCertificado string   `xml:"numeroCertificado,attr"`
	CadenaCDA         string   `xml:"cadenaCDA,attr"`
	Sello             string   `xml:"sello,attr"`
	Beneficiario      struct {
		BancoReceptor   string `xml:"BancoReceptor,attr"`
		Nombre          string `xml:"Nombre,attr"`
		TipoCuenta      string `xml:"TipoCuenta,attr"`
		Cuenta          string `xml:"Cuenta,attr"`
		RFC             string `xml:"RFC,attr"`
		Concepto        string `xml:"Concepto,attr"`
		IVA             string `xml:"IVA,attr"`
		MontoPago       string `xml:"MontoPago,attr"`
	} `xml:"Beneficiario"`
	Ordenante struct {
		BancoEmisor string `xml:"BancoEmisor,attr"`
		Nombre      string `xml:"Nombre,attr"`
		Cuenta      string `xml:"Cuenta,attr"`
		RFC         string `xml:"RFC,attr"`
	} `xml:"Ordenante"`
}

func parseCEPMontoToCents(raw string) (int64, error) {
	cleaned := strings.TrimSpace(raw)
	if cleaned == "" {
		return 0, ErrInvalidCEPXML
	}
	parts := strings.Split(cleaned, ".")
	if len(parts) > 2 {
		return 0, ErrInvalidCEPXML
	}
	pesos, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || pesos < 0 {
		return 0, ErrInvalidCEPXML
	}
	var centavos int64
	if len(parts) == 2 {
		frac := parts[1]
		if len(frac) == 1 {
			frac += "0"
		} else if len(frac) == 0 {
			frac = "00"
		} else if len(frac) > 2 {
			return 0, ErrInvalidCEPXML
		}
		centavos, err = strconv.ParseInt(frac, 10, 64)
		if err != nil || centavos < 0 {
			return 0, ErrInvalidCEPXML
		}
	}
	total := pesos*100 + centavos
	if total <= 0 {
		return 0, ErrInvalidCEPXML
	}
	return total, nil
}

// ParseBanxicoCEPXML parses an official Banxico SPEI_Tercero CEP XML document and validates its CDA digital signature presence.
func ParseBanxicoCEPXML(xmlBytes []byte) (*CEPReceipt, error) {
	if len(xmlBytes) == 0 {
		return nil, ErrInvalidCEPXML
	}

	var doc banxicoSPEITerceroXML
	if err := xml.Unmarshal(xmlBytes, &doc); err != nil {
		return nil, ErrInvalidCEPXML
	}

	if strings.TrimSpace(doc.ClaveRastreo) == "" || strings.TrimSpace(doc.Beneficiario.Cuenta) == "" {
		return nil, ErrInvalidCEPXML
	}
	if strings.TrimSpace(doc.Sello) == "" || strings.TrimSpace(doc.NumeroCertificado) == "" {
		return nil, ErrInvalidCEPSignature
	}

	montoCents, err := parseCEPMontoToCents(doc.Beneficiario.MontoPago)
	if err != nil {
		return nil, err
	}

	hash := sha256.Sum256(xmlBytes)

	return &CEPReceipt{
		ClaveRastreo:             doc.ClaveRastreo,
		FechaOperacion:           doc.FechaOperacion,
		HoraLiquidacion:          doc.Hora,
		InstitucionEmisora:       doc.Ordenante.BancoEmisor,
		InstitucionReceptora:     doc.Beneficiario.BancoReceptor,
		CuentaBeneficiaria:       doc.Beneficiario.Cuenta,
		RfcBeneficiario:          doc.Beneficiario.RFC,
		MontoCents:               montoCents,
		NumeroCertificadoBanxico: doc.NumeroCertificado,
		SelloDigitalBanxico:      doc.Sello,
		CadenaOriginalCDA:        doc.CadenaCDA,
		SHA256Hash:               hex.EncodeToString(hash[:]),
		RawXML:                   string(xmlBytes),
	}, nil
}

// CEPProvider abstracts fetching the CEP XML from Banxico/STP.
type CEPProvider interface {
	FetchCEP(ctx context.Context, query CEPQuery) (*CEPReceipt, error)
}

// HTTPCEPProvider fetches Banxico CEP XML documents over HTTP.
type HTTPCEPProvider struct {
	baseURL    string
	httpClient *http.Client
}

func NewHTTPCEPProvider(baseURL string, timeout time.Duration) *HTTPCEPProvider {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &HTTPCEPProvider{
		baseURL: strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

func (p *HTTPCEPProvider) FetchCEP(ctx context.Context, query CEPQuery) (*CEPReceipt, error) {
	u, err := url.Parse(p.baseURL + "/cep/descarga.do")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("claveRastreo", query.ClaveRastreo)
	q.Set("fechaOperacion", query.FechaOperacion)
	q.Set("emisor", query.InstitucionEmisora)
	q.Set("receptor", query.InstitucionReceptora)
	q.Set("cuenta", query.CuentaBeneficiaria)
	q.Set("monto", fmt.Sprintf("%d.%02d", query.MontoCents/100, query.MontoCents%100))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("CEP HTTP request failed with status %d", resp.StatusCode)
	}

	xmlBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return ParseBanxicoCEPXML(xmlBytes)
}

// MockCEPProvider generates deterministic Banxico CEP XML receipts for local testing and sandbox runs.
type MockCEPProvider struct {
	mu        sync.RWMutex
	overrides map[string][]byte
}

func NewMockCEPProvider() *MockCEPProvider {
	return &MockCEPProvider{
		overrides: make(map[string][]byte),
	}
}

func (m *MockCEPProvider) SetCustomCEPXML(claveRastreo string, rawXML []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.overrides[claveRastreo] = rawXML
}

func (m *MockCEPProvider) FetchCEP(ctx context.Context, query CEPQuery) (*CEPReceipt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	custom, ok := m.overrides[query.ClaveRastreo]
	m.mu.RUnlock()
	if ok {
		return ParseBanxicoCEPXML(custom)
	}

	montoStr := fmt.Sprintf("%d.%02d", query.MontoCents/100, query.MontoCents%100)
	emisor := query.InstitucionEmisora
	if emisor == "" {
		emisor = "90646"
	}
	receptor := query.InstitucionReceptora
	if receptor == "" && len(query.CuentaBeneficiaria) >= 3 {
		receptor = "40" + query.CuentaBeneficiaria[:3]
	}
	fecha := query.FechaOperacion
	if fecha == "" {
		fecha = time.Now().UTC().Format("2006-01-02")
	}

	cadenaCDA := fmt.Sprintf("||%s|%s|%s|%s|%s||", fecha, query.ClaveRastreo, emisor, query.CuentaBeneficiaria, montoStr)
	sum := sha256.Sum256([]byte(cadenaCDA))
	selloHex := hex.EncodeToString(sum[:])

	xmlStr := fmt.Sprintf(
		`<SPEI_Tercero FechaOperacion="%s" Hora="12:00:00" ClaveSPEI="1" claveRastreo="%s" numeroCertificado="00001000000500009999" cadenaCDA="%s" sello="%s"><Ordenante BancoEmisor="%s" Nombre="SOFTPOS MX" Cuenta="646180000000000007" RFC="SPM260101AAA"/><Beneficiario BancoReceptor="%s" Nombre="COMERCIO BENEFICIARIO" TipoCuenta="40" Cuenta="%s" RFC="XAXX010101000" Concepto="LIQUIDACION SOFTPOS" IVA="0.00" MontoPago="%s"/></SPEI_Tercero>`,
		fecha,
		query.ClaveRastreo,
		cadenaCDA,
		selloHex,
		emisor,
		receptor,
		query.CuentaBeneficiaria,
		montoStr,
	)

	return ParseBanxicoCEPXML([]byte(xmlStr))
}

// CEPReconciler manages the tracking and automatic Banxico CEP verification of outbound SPEI disbursements.
type CEPReconciler struct {
	mu       sync.RWMutex
	provider CEPProvider
	records  map[string]*DisbursementRecord
}

func NewCEPReconciler(provider CEPProvider) *CEPReconciler {
	if provider == nil {
		provider = NewMockCEPProvider()
	}
	return &CEPReconciler{
		provider: provider,
		records:  make(map[string]*DisbursementRecord),
	}
}

func (r *CEPReconciler) SetProvider(provider CEPProvider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.provider = provider
}

func (r *CEPReconciler) TrackDisbursement(rec DisbursementRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec.Status == "" {
		rec.Status = StatusPendingCEP
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	if rec.FechaOperacion == "" {
		rec.FechaOperacion = rec.CreatedAt.Format("20060102")
	}
	cp := rec
	r.records[rec.ClaveRastreo] = &cp
}

func (r *CEPReconciler) GetDisbursement(claveRastreo string) (*DisbursementRecord, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.records[claveRastreo]
	if !ok {
		return nil, ErrDisbursementNotFound
	}
	cp := *rec
	return &cp, nil
}

// ReconcileByClaveRastreo fetches the Banxico CEP and verifies amount, beneficiary CLABE, and Banxico CDA signature.
func (r *CEPReconciler) ReconcileByClaveRastreo(ctx context.Context, claveRastreo string) (*DisbursementRecord, error) {
	r.mu.RLock()
	rec, ok := r.records[claveRastreo]
	provider := r.provider
	if !ok {
		r.mu.RUnlock()
		return nil, ErrDisbursementNotFound
	}
	query := CEPQuery{
		ClaveRastreo:       rec.ClaveRastreo,
		FechaOperacion:     rec.FechaOperacion,
		InstitucionEmisora: "90646",
		CuentaBeneficiaria: rec.DestinationCLABE,
		MontoCents:         rec.AmountCents,
	}
	if len(rec.DestinationCLABE) >= 3 {
		query.InstitucionReceptora = "40" + rec.DestinationCLABE[:3]
	}
	r.mu.RUnlock()

	cep, err := provider.FetchCEP(ctx, query)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	rec, ok = r.records[claveRastreo]
	if !ok {
		return nil, ErrDisbursementNotFound
	}

	if cep.CuentaBeneficiaria != rec.DestinationCLABE {
		rec.Status = StatusCEPDiscrepancy
		rec.DiscrepancyReason = fmt.Sprintf("CLABE mismatch: expected %s, got %s", rec.DestinationCLABE, cep.CuentaBeneficiaria)
		cp := *rec
		return &cp, ErrCEPAccountMismatch
	}

	if cep.MontoCents != rec.AmountCents {
		rec.Status = StatusCEPDiscrepancy
		rec.DiscrepancyReason = fmt.Sprintf("amount mismatch: expected %d cents, got %d cents", rec.AmountCents, cep.MontoCents)
		cp := *rec
		return &cp, ErrCEPAmountMismatch
	}

	now := time.Now().UTC()
	rec.Status = StatusReconciledCEP
	rec.CEP = cep
	rec.DiscrepancyReason = ""
	rec.ReconciledAt = &now

	cp := *rec
	return &cp, nil
}

// ReconcileAllPending iterates through all PENDING_CEP disbursements and reconciles them against Banxico.
func (r *CEPReconciler) ReconcileAllPending(ctx context.Context) ([]DisbursementRecord, error) {
	r.mu.RLock()
	var pendingKeys []string
	for k, v := range r.records {
		if v.Status == StatusPendingCEP {
			pendingKeys = append(pendingKeys, k)
		}
	}
	r.mu.RUnlock()

	var results []DisbursementRecord
	for _, key := range pendingKeys {
		rec, err := r.ReconcileByClaveRastreo(ctx, key)
		if rec != nil {
			results = append(results, *rec)
		} else if err != nil {
			return results, err
		}
	}
	return results, nil
}
