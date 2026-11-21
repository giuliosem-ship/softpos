package fiscal

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrInvalidCFDIData = errors.New("invalid CFDI 4.0 data")
	ErrSigningFailed   = errors.New("failed to digitally sign CFDI with CSD")
)

const (
	cfdi40Namespace      = "http://www.sat.gob.mx/cfd/4"
	xsiNamespace         = "http://www.w3.org/2001/XMLSchema-instance"
	cfdi40SchemaLocation = "http://www.sat.gob.mx/cfd/4 http://www.sat.gob.mx/sitio_internet/cfd/4/cfdv40.xsd"
)

// CFDIComprobante is the CFDI 4.0 comprobante (Anexo 20). Attribute and
// element order follows the XSD; optional attributes use omitempty, so an
// empty string means "absent" (the cadena original skips it).
type CFDIComprobante struct {
	XMLName           xml.Name               `xml:"cfdi:Comprobante"`
	XmlnsCfdi         string                 `xml:"xmlns:cfdi,attr"`
	XmlnsXsi          string                 `xml:"xmlns:xsi,attr"`
	XsiSchemaLocation string                 `xml:"xsi:schemaLocation,attr"`
	Version           string                 `xml:"Version,attr"`
	Serie             string                 `xml:"Serie,attr,omitempty"`
	Folio             string                 `xml:"Folio,attr,omitempty"`
	Fecha             string                 `xml:"Fecha,attr"`
	Sello             string                 `xml:"Sello,attr"`
	FormaPago         string                 `xml:"FormaPago,attr,omitempty"`
	NoCertificado     string                 `xml:"NoCertificado,attr"`
	Certificado       string                 `xml:"Certificado,attr"`
	CondicionesDePago string                 `xml:"CondicionesDePago,attr,omitempty"`
	SubTotal          string                 `xml:"SubTotal,attr"`
	Descuento         string                 `xml:"Descuento,attr,omitempty"`
	Moneda            string                 `xml:"Moneda,attr"`
	TipoCambio        string                 `xml:"TipoCambio,attr,omitempty"`
	Total             string                 `xml:"Total,attr"`
	TipoDeComprobante string                 `xml:"TipoDeComprobante,attr"`
	Exportacion       string                 `xml:"Exportacion,attr"`
	MetodoPago        string                 `xml:"MetodoPago,attr,omitempty"`
	LugarExpedicion   string                 `xml:"LugarExpedicion,attr"`
	Confirmacion      string                 `xml:"Confirmacion,attr,omitempty"`
	InformacionGlobal *CFDIInformacionGlobal `xml:"cfdi:InformacionGlobal,omitempty"`
	CfdiRelacionados  []CFDIRelacionados     `xml:"cfdi:CfdiRelacionados,omitempty"`
	Emisor            CFDIEmisor             `xml:"cfdi:Emisor"`
	Receptor          CFDIReceptor           `xml:"cfdi:Receptor"`
	Conceptos         []CFDIConcepto         `xml:"cfdi:Conceptos>cfdi:Concepto"`
	Impuestos         *CFDIImpuestos         `xml:"cfdi:Impuestos,omitempty"`
}

type CFDIInformacionGlobal struct {
	Periodicidad string `xml:"Periodicidad,attr"`
	Meses        string `xml:"Meses,attr"`
	Anio         int    `xml:"Año,attr"`
}

// CFDIRelacionados relates this CFDI to others (e.g. TipoRelacion 04,
// sustitución de los CFDI previos).
type CFDIRelacionados struct {
	TipoRelacion string                `xml:"TipoRelacion,attr"`
	Relacionados []CFDIRelacionadoUUID `xml:"cfdi:CfdiRelacionado"`
}

type CFDIRelacionadoUUID struct {
	UUID string `xml:"UUID,attr"`
}

type CFDIEmisor struct {
	Rfc              string `xml:"Rfc,attr"`
	Nombre           string `xml:"Nombre,attr"`
	RegimenFiscal    string `xml:"RegimenFiscal,attr"`
	FacAtrAdquirente string `xml:"FacAtrAdquirente,attr,omitempty"`
}

type CFDIReceptor struct {
	Rfc                     string `xml:"Rfc,attr"`
	Nombre                  string `xml:"Nombre,attr"`
	DomicilioFiscalReceptor string `xml:"DomicilioFiscalReceptor,attr"`
	ResidenciaFiscal        string `xml:"ResidenciaFiscal,attr,omitempty"`
	NumRegIdTrib            string `xml:"NumRegIdTrib,attr,omitempty"`
	RegimenFiscalReceptor   string `xml:"RegimenFiscalReceptor,attr"`
	UsoCFDI                 string `xml:"UsoCFDI,attr"`
}

type CFDIConcepto struct {
	ClaveProdServ    string                 `xml:"ClaveProdServ,attr"`
	NoIdentificacion string                 `xml:"NoIdentificacion,attr,omitempty"`
	Cantidad         string                 `xml:"Cantidad,attr"`
	ClaveUnidad      string                 `xml:"ClaveUnidad,attr"`
	Unidad           string                 `xml:"Unidad,attr,omitempty"`
	Descripcion      string                 `xml:"Descripcion,attr"`
	ValorUnitario    string                 `xml:"ValorUnitario,attr"`
	Importe          string                 `xml:"Importe,attr"`
	Descuento        string                 `xml:"Descuento,attr,omitempty"`
	ObjetoImp        string                 `xml:"ObjetoImp,attr"`
	Impuestos        *CFDIConceptoImpuestos `xml:"cfdi:Impuestos,omitempty"`
}

// CFDIConceptoImpuestos are the taxes of one concepto (XSD order:
// Traslados, then Retenciones).
type CFDIConceptoImpuestos struct {
	Traslados   []CFDITraslado          `xml:"cfdi:Traslados>cfdi:Traslado,omitempty"`
	Retenciones []CFDIRetencionConcepto `xml:"cfdi:Retenciones>cfdi:Retencion,omitempty"`
}

// CFDIImpuestos are the comprobante-level tax totals (XSD order:
// Retenciones, then Traslados).
type CFDIImpuestos struct {
	TotalImpuestosRetenidos   string          `xml:"TotalImpuestosRetenidos,attr,omitempty"`
	TotalImpuestosTrasladados string          `xml:"TotalImpuestosTrasladados,attr,omitempty"`
	Retenciones               []CFDIRetencion `xml:"cfdi:Retenciones>cfdi:Retencion,omitempty"`
	Traslados                 []CFDITraslado  `xml:"cfdi:Traslados>cfdi:Traslado,omitempty"`
}

// CFDITraslado is a transferred tax. TasaOCuota and Importe are absent for
// TipoFactor "Exento".
type CFDITraslado struct {
	Base       string `xml:"Base,attr"`
	Impuesto   string `xml:"Impuesto,attr"`
	TipoFactor string `xml:"TipoFactor,attr"`
	TasaOCuota string `xml:"TasaOCuota,attr,omitempty"`
	Importe    string `xml:"Importe,attr,omitempty"`
}

// CFDIRetencionConcepto is a withheld tax of one concepto.
type CFDIRetencionConcepto struct {
	Base       string `xml:"Base,attr"`
	Impuesto   string `xml:"Impuesto,attr"`
	TipoFactor string `xml:"TipoFactor,attr"`
	TasaOCuota string `xml:"TasaOCuota,attr"`
	Importe    string `xml:"Importe,attr"`
}

// CFDIRetencion is a comprobante-level withheld tax total.
type CFDIRetencion struct {
	Impuesto string `xml:"Impuesto,attr"`
	Importe  string `xml:"Importe,attr"`
}

// ValidXMLText reports whether every rune is allowed in XML 1.0 text
// (Go's encoder would silently replace the others with U+FFFD and the
// sello would no longer match the document).
func ValidXMLText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
		case r < 0x20, r == 0xFFFE, r == 0xFFFF:
			return false
		}
	}
	return true
}

// BuildCFDI40XML serializes a CFDIComprobante struct to SAT Anexo 20 compliant UTF-8 XML.
func BuildCFDI40XML(comp *CFDIComprobante) ([]byte, error) {
	if comp == nil || comp.Emisor.Rfc == "" || comp.Receptor.Rfc == "" {
		return nil, ErrInvalidCFDIData
	}
	if comp.Version == "" {
		comp.Version = "4.0"
	}
	if comp.Version != "4.0" {
		return nil, ErrInvalidCFDIData
	}
	for _, f := range cadenaFields(comp) {
		if !ValidXMLText(f.value) {
			return nil, fmt.Errorf("%w: invalid XML character", ErrInvalidCFDIData)
		}
	}

	outComp := *comp
	outComp.XmlnsCfdi = cfdi40Namespace
	outComp.XmlnsXsi = xsiNamespace
	outComp.XsiSchemaLocation = cfdi40SchemaLocation

	body, err := xml.MarshalIndent(&outComp, "", "  ")
	if err != nil {
		return nil, ErrInvalidCFDIData
	}

	fullXML := append([]byte(xml.Header), body...)
	return fullXML, nil
}

// cadenaField is one attribute in cadena original order. required
// attributes are always emitted (an empty one as "|"); optional ones only
// when present (non-empty in this model, like the omitempty XML attribute).
type cadenaField struct {
	value    string
	required bool
}

// cadenaFields lists the comprobante attributes in the order of the SAT
// XSLT cadenaoriginal_4_0.xslt (Requerido / Opcional templates).
func cadenaFields(c *CFDIComprobante) []cadenaField {
	var f []cadenaField
	req := func(v string) { f = append(f, cadenaField{v, true}) }
	opt := func(v string) { f = append(f, cadenaField{v, false}) }

	version := c.Version
	if version == "" {
		version = "4.0"
	}
	req(version)
	opt(c.Serie)
	opt(c.Folio)
	req(c.Fecha)
	opt(c.FormaPago)
	req(c.NoCertificado)
	opt(c.CondicionesDePago)
	req(c.SubTotal)
	opt(c.Descuento)
	req(c.Moneda)
	opt(c.TipoCambio)
	req(c.Total)
	req(c.TipoDeComprobante)
	req(c.Exportacion)
	opt(c.MetodoPago)
	req(c.LugarExpedicion)
	opt(c.Confirmacion)

	if g := c.InformacionGlobal; g != nil {
		req(g.Periodicidad)
		req(g.Meses)
		req(fmt.Sprintf("%d", g.Anio))
	}
	for _, rel := range c.CfdiRelacionados {
		req(rel.TipoRelacion)
		for _, u := range rel.Relacionados {
			req(u.UUID)
		}
	}

	req(c.Emisor.Rfc)
	req(c.Emisor.Nombre)
	req(c.Emisor.RegimenFiscal)
	opt(c.Emisor.FacAtrAdquirente)

	req(c.Receptor.Rfc)
	req(c.Receptor.Nombre)
	req(c.Receptor.DomicilioFiscalReceptor)
	opt(c.Receptor.ResidenciaFiscal)
	opt(c.Receptor.NumRegIdTrib)
	req(c.Receptor.RegimenFiscalReceptor)
	req(c.Receptor.UsoCFDI)

	for _, k := range c.Conceptos {
		req(k.ClaveProdServ)
		opt(k.NoIdentificacion)
		req(k.Cantidad)
		req(k.ClaveUnidad)
		opt(k.Unidad)
		req(k.Descripcion)
		req(k.ValorUnitario)
		req(k.Importe)
		opt(k.Descuento)
		req(k.ObjetoImp)
		if k.Impuestos != nil {
			for _, t := range k.Impuestos.Traslados {
				req(t.Base)
				req(t.Impuesto)
				req(t.TipoFactor)
				opt(t.TasaOCuota)
				opt(t.Importe)
			}
			for _, r := range k.Impuestos.Retenciones {
				req(r.Base)
				req(r.Impuesto)
				req(r.TipoFactor)
				req(r.TasaOCuota)
				req(r.Importe)
			}
		}
	}

	if imp := c.Impuestos; imp != nil {
		for _, r := range imp.Retenciones {
			req(r.Impuesto)
			req(r.Importe)
		}
		opt(imp.TotalImpuestosRetenidos)
		for _, t := range imp.Traslados {
			req(t.Base)
			req(t.Impuesto)
			req(t.TipoFactor)
			opt(t.TasaOCuota)
			opt(t.Importe)
		}
		opt(imp.TotalImpuestosTrasladados)
	}
	return f
}

// normalizeSpace is XPath normalize-space(): strips leading/trailing and
// collapses inner runs of #x20, #x9, #xD, #xA into one space (other Unicode
// spaces, e.g. U+00A0, are kept).
func normalizeSpace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			space = b.Len() > 0
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// BuildCadenaOriginal builds the CFDI 4.0 cadena original exactly as the SAT
// XSLT cadenaoriginal_4_0.xslt does (validated by golden tests running the
// official stylesheet through xsltproc, see testdata/sat/README.md).
// Complements (Complemento) are not supported: Kerno emits none.
func BuildCadenaOriginal(comp *CFDIComprobante) string {
	if comp == nil {
		return "||||"
	}
	var b strings.Builder
	b.WriteByte('|')
	for _, f := range cadenaFields(comp) {
		if !f.required && f.value == "" {
			continue
		}
		b.WriteByte('|')
		b.WriteString(normalizeSpace(f.value))
	}
	b.WriteString("||")
	return b.String()
}

func parseRSAPrivateKeyDER(privKeyDER []byte) (*rsa.PrivateKey, error) {
	if key, err := x509.ParsePKCS1PrivateKey(privKeyDER); err == nil {
		return key, nil
	}
	keyIfc, err := x509.ParsePKCS8PrivateKey(privKeyDER)
	if err != nil {
		return nil, err
	}
	rsaKey, ok := keyIfc.(*rsa.PrivateKey)
	if !ok {
		return nil, ErrSigningFailed
	}
	return rsaKey, nil
}

// CertificateNumber returns the SAT "NoCertificado" (20 digits) of a CSD:
// SAT serial numbers are the ASCII bytes of the number (e.g. hex
// 3330303031... -> "30001..."). Other serials fall back to their decimal
// form, left-padded to 20 digits.
func CertificateNumber(cert *x509.Certificate) string {
	if cert == nil || cert.SerialNumber == nil {
		return ""
	}
	raw := cert.SerialNumber.Bytes()
	ascii := len(raw) > 0
	for _, c := range raw {
		if c < '0' || c > '9' {
			ascii = false
			break
		}
	}
	if ascii {
		return string(raw)
	}
	n := fmt.Sprintf("%020s", cert.SerialNumber.String())
	if len(n) > 20 {
		n = n[len(n)-20:]
	}
	return n
}

// CSDSigner seals comprobantes with a merchant CSD (plaintext key, held
// only in memory while signing).
type CSDSigner struct {
	cert    *x509.Certificate
	certDER []byte
	key     *rsa.PrivateKey
}

// NewCSDSigner parses a DER certificate and a plaintext PKCS#1/PKCS#8 RSA
// key (use DecryptSATPrivateKey for the SAT .key format first). The key
// must belong to the certificate.
func NewCSDSigner(certDER, keyDER []byte) (*CSDSigner, error) {
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, ErrSigningFailed
	}
	key, err := parseRSAPrivateKeyDER(keyDER)
	if err != nil {
		return nil, ErrSigningFailed
	}
	pub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return nil, fmt.Errorf("%w: key does not match certificate", ErrSigningFailed)
	}
	return &CSDSigner{cert: cert, certDER: append([]byte(nil), certDER...), key: key}, nil
}

// Certificate returns the CSD certificate.
func (s *CSDSigner) Certificate() *x509.Certificate { return s.cert }

// Seal sets NoCertificado and Certificado and signs the cadena original
// (RSA PKCS#1 v1.5 over SHA-256, Anexo 20) into Sello. It returns the
// cadena original that was signed.
func (s *CSDSigner) Seal(comp *CFDIComprobante) (string, error) {
	if s == nil || comp == nil {
		return "", ErrSigningFailed
	}
	comp.NoCertificado = CertificateNumber(s.cert)
	comp.Certificado = base64.StdEncoding.EncodeToString(s.certDER)
	comp.Sello = ""
	cadena := BuildCadenaOriginal(comp)
	hash := sha256.Sum256([]byte(cadena))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, hash[:])
	if err != nil {
		return "", ErrSigningFailed
	}
	comp.Sello = base64.StdEncoding.EncodeToString(sig)
	return cadena, nil
}

// VerifySello checks a comprobante's Sello against its cadena original and
// the certificate embedded in Certificado.
func VerifySello(comp *CFDIComprobante) error {
	certDER, err := base64.StdEncoding.DecodeString(comp.Certificado)
	if err != nil {
		return ErrSigningFailed
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return ErrSigningFailed
	}
	pub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return ErrSigningFailed
	}
	sig, err := base64.StdEncoding.DecodeString(comp.Sello)
	if err != nil {
		return ErrSigningFailed
	}
	hash := sha256.Sum256([]byte(BuildCadenaOriginal(comp)))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, hash[:], sig); err != nil {
		return fmt.Errorf("%w: sello does not verify", ErrSigningFailed)
	}
	return nil
}

// FormatCents renders int64 cents as a CFDI decimal with 2 places.
func FormatCents(cents int64) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("%s%d.%02d", sign, cents/100, cents%100)
}

// BuildFacturaGlobalDiaria creates and signs a daily CFDI 4.0 Factura Global
// for PUBLICO EN GENERAL (XAXX010101000) with a single concept. Kept for the
// legacy DEV_MODE endpoint; the invoice service builds globals per sale.
func BuildFacturaGlobalDiaria(
	emisor CFDIEmisor,
	totalGrossCents int64,
	baseCents int64,
	ivaCents int64,
	csdKey []byte,
	csdCert []byte,
) ([]byte, error) {
	if emisor.Rfc == "" || totalGrossCents <= 0 || baseCents <= 0 || ivaCents < 0 {
		return nil, ErrInvalidCFDIData
	}
	if totalGrossCents != baseCents+ivaCents {
		return nil, ErrInvalidCFDIData
	}
	signer, err := NewCSDSigner(csdCert, csdKey)
	if err != nil {
		return nil, ErrSigningFailed
	}

	now := time.Now().In(MexicoCityLocation())
	baseStr := FormatCents(baseCents)
	ivaStr := FormatCents(ivaCents)
	totalStr := FormatCents(totalGrossCents)
	const lugar = "06000"

	comp := &CFDIComprobante{
		Version:           "4.0",
		Serie:             "FG",
		Folio:             now.Format("20060102"),
		Fecha:             now.Format("2006-01-02T15:04:05"),
		FormaPago:         "04",
		SubTotal:          baseStr,
		Moneda:            "MXN",
		Total:             totalStr,
		TipoDeComprobante: "I",
		Exportacion:       "01",
		MetodoPago:        "PUE",
		LugarExpedicion:   lugar,
		InformacionGlobal: &CFDIInformacionGlobal{
			Periodicidad: "01", // Diaria
			Meses:        fmt.Sprintf("%02d", int(now.Month())),
			Anio:         now.Year(),
		},
		Emisor:   emisor,
		Receptor: PublicoEnGeneral(lugar),
		Conceptos: []CFDIConcepto{
			{
				ClaveProdServ: "01010101",
				Cantidad:      "1",
				ClaveUnidad:   "ACT",
				Descripcion:   "Venta",
				ValorUnitario: baseStr,
				Importe:       baseStr,
				ObjetoImp:     "02",
				Impuestos: &CFDIConceptoImpuestos{Traslados: []CFDITraslado{
					{Base: baseStr, Impuesto: "002", TipoFactor: "Tasa", TasaOCuota: "0.160000", Importe: ivaStr},
				}},
			},
		},
		Impuestos: &CFDIImpuestos{
			TotalImpuestosTrasladados: ivaStr,
			Traslados: []CFDITraslado{
				{Base: baseStr, Impuesto: "002", TipoFactor: "Tasa", TasaOCuota: "0.160000", Importe: ivaStr},
			},
		},
	}
	if _, err := signer.Seal(comp); err != nil {
		return nil, err
	}
	return BuildCFDI40XML(comp)
}

// PublicoEnGeneral is the receptor of a factura global: generic RFC, régimen
// 616, uso S01 and DomicilioFiscalReceptor equal to the LugarExpedicion.
func PublicoEnGeneral(lugarExpedicion string) CFDIReceptor {
	return CFDIReceptor{
		Rfc:                     RFCPublicoEnGeneral,
		Nombre:                  "PUBLICO EN GENERAL",
		DomicilioFiscalReceptor: lugarExpedicion,
		RegimenFiscalReceptor:   "616",
		UsoCFDI:                 "S01",
	}
}

// Generic RFCs (Anexo 20).
const (
	RFCPublicoEnGeneral = "XAXX010101000"
	RFCExtranjero       = "XEXX010101000"
)
