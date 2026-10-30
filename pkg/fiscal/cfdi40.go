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

type CFDIComprobante struct {
	XMLName           xml.Name               `xml:"cfdi:Comprobante"`
	XmlnsCfdi         string                 `xml:"xmlns:cfdi,attr"`
	XmlnsXsi          string                 `xml:"xmlns:xsi,attr"`
	XsiSchemaLocation string                 `xml:"xsi:schemaLocation,attr"`
	Version           string                 `xml:"Version,attr"`
	Serie             string                 `xml:"Serie,attr,omitempty"`
	Folio             string                 `xml:"Folio,attr,omitempty"`
	Fecha             string                 `xml:"Fecha,attr"`
	Sello             string                 `xml:"Sello,attr,omitempty"`
	FormaPago         string                 `xml:"FormaPago,attr,omitempty"`
	NoCertificado     string                 `xml:"NoCertificado,attr,omitempty"`
	Certificado       string                 `xml:"Certificado,attr,omitempty"`
	SubTotal          string                 `xml:"SubTotal,attr"`
	Moneda            string                 `xml:"Moneda,attr"`
	Total             string                 `xml:"Total,attr"`
	TipoDeComprobante string                 `xml:"TipoDeComprobante,attr"`
	Exportacion       string                 `xml:"Exportacion,attr"`
	MetodoPago        string                 `xml:"MetodoPago,attr,omitempty"`
	LugarExpedicion   string                 `xml:"LugarExpedicion,attr"`
	InformacionGlobal *CFDIInformacionGlobal `xml:"cfdi:InformacionGlobal,omitempty"`
	Emisor            CFDIEmisor             `xml:"cfdi:Emisor"`
	Receptor          CFDIReceptor           `xml:"cfdi:Receptor"`
	Conceptos         []CFDIConcepto         `xml:"cfdi:Conceptos>cfdi:Concepto"`
	Impuestos         CFDIImpuestos          `xml:"cfdi:Impuestos"`
}

type CFDIInformacionGlobal struct {
	Periodicidad string `xml:"Periodicidad,attr"`
	Meses        string `xml:"Meses,attr"`
	Anio         int    `xml:"Año,attr"`
}

type CFDIEmisor struct {
	Rfc           string `xml:"Rfc,attr"`
	Nombre        string `xml:"Nombre,attr"`
	RegimenFiscal string `xml:"RegimenFiscal,attr"`
}

type CFDIReceptor struct {
	Rfc                     string `xml:"Rfc,attr"`
	Nombre                  string `xml:"Nombre,attr"`
	DomicilioFiscalReceptor string `xml:"DomicilioFiscalReceptor,attr"`
	RegimenFiscalReceptor   string `xml:"RegimenFiscalReceptor,attr"`
	UsoCFDI                 string `xml:"UsoCFDI,attr"`
}

type CFDIConcepto struct {
	ClaveProdServ string `xml:"ClaveProdServ,attr"`
	Cantidad      string `xml:"Cantidad,attr"`
	ClaveUnidad   string `xml:"ClaveUnidad,attr"`
	Descripcion   string `xml:"Descripcion,attr"`
	ValorUnitario string `xml:"ValorUnitario,attr"`
	Importe       string `xml:"Importe,attr"`
	ObjetoImp     string `xml:"ObjetoImp,attr"`
}

type CFDIImpuestos struct {
	TotalImpuestosTrasladados string          `xml:"TotalImpuestosTrasladados,attr,omitempty"`
	TotalImpuestosRetenidos   string          `xml:"TotalImpuestosRetenidos,attr,omitempty"`
	Retenciones               []CFDIRetencion `xml:"cfdi:Retenciones>cfdi:Retencion,omitempty"`
	Traslados                 []CFDITraslado  `xml:"cfdi:Traslados>cfdi:Traslado,omitempty"`
}

type CFDITraslado struct {
	Base       string `xml:"Base,attr"`
	Impuesto   string `xml:"Impuesto,attr"`
	TipoFactor string `xml:"TipoFactor,attr"`
	TasaOCuota string `xml:"TasaOCuota,attr"`
	Importe    string `xml:"Importe,attr"`
}

type CFDIRetencion struct {
	Impuesto string `xml:"Impuesto,attr"`
	Importe  string `xml:"Importe,attr"`
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

// BuildCadenaOriginal constructs the SAT Anexo 20 pipe-delimited Cadena Original string.
func BuildCadenaOriginal(comp *CFDIComprobante) string {
	if comp == nil {
		return "||||"
	}

	var fields []string
	add := func(val string) {
		trimmed := strings.TrimSpace(val)
		if trimmed != "" {
			fields = append(fields, trimmed)
		}
	}

	version := comp.Version
	if version == "" {
		version = "4.0"
	}
	add(version)
	add(comp.Serie)
	add(comp.Folio)
	add(comp.Fecha)
	add(comp.FormaPago)
	add(comp.NoCertificado)
	add(comp.SubTotal)
	add(comp.Moneda)
	add(comp.Total)
	add(comp.TipoDeComprobante)
	add(comp.Exportacion)
	add(comp.MetodoPago)
	add(comp.LugarExpedicion)

	if comp.InformacionGlobal != nil {
		add(comp.InformacionGlobal.Periodicidad)
		add(comp.InformacionGlobal.Meses)
		if comp.InformacionGlobal.Anio > 0 {
			add(fmt.Sprintf("%d", comp.InformacionGlobal.Anio))
		}
	}

	add(comp.Emisor.Rfc)
	add(comp.Emisor.Nombre)
	add(comp.Emisor.RegimenFiscal)

	add(comp.Receptor.Rfc)
	add(comp.Receptor.Nombre)
	add(comp.Receptor.DomicilioFiscalReceptor)
	add(comp.Receptor.RegimenFiscalReceptor)
	add(comp.Receptor.UsoCFDI)

	for _, c := range comp.Conceptos {
		add(c.ClaveProdServ)
		add(c.Cantidad)
		add(c.ClaveUnidad)
		add(c.Descripcion)
		add(c.ValorUnitario)
		add(c.Importe)
		add(c.ObjetoImp)
	}

	for _, r := range comp.Impuestos.Retenciones {
		add(r.Impuesto)
		add(r.Importe)
	}
	add(comp.Impuestos.TotalImpuestosRetenidos)

	for _, t := range comp.Impuestos.Traslados {
		add(t.Base)
		add(t.Impuesto)
		add(t.TipoFactor)
		add(t.TasaOCuota)
		add(t.Importe)
	}
	add(comp.Impuestos.TotalImpuestosTrasladados)

	return "||" + strings.Join(fields, "|") + "||"
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

// SignCFDI digitally signs a CFDI 4.0 XML payload using a CSD RSA private key and X.509 certificate.
func SignCFDI(xmlBytes []byte, privKeyDER []byte, certDER []byte) ([]byte, error) {
	if len(xmlBytes) == 0 || len(privKeyDER) == 0 || len(certDER) == 0 {
		return nil, ErrSigningFailed
	}

	rsaKey, err := parseRSAPrivateKeyDER(privKeyDER)
	if err != nil {
		return nil, ErrSigningFailed
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, ErrSigningFailed
	}

	noCertificado := fmt.Sprintf("%020s", cert.SerialNumber.String())
	if len(noCertificado) > 20 {
		noCertificado = noCertificado[len(noCertificado)-20:]
	}
	certBase64 := base64.StdEncoding.EncodeToString(certDER)

	xmlStr := string(xmlBytes)
	if !strings.Contains(xmlStr, "<cfdi:Comprobante") {
		return nil, ErrInvalidCFDIData
	}

	hash := sha256.Sum256(xmlBytes)
	signature, err := rsa.SignPKCS1v15(rand.Reader, rsaKey, crypto.SHA256, hash[:])
	if err != nil {
		return nil, ErrSigningFailed
	}
	selloBase64 := base64.StdEncoding.EncodeToString(signature)

	signedAttrs := fmt.Sprintf(` NoCertificado="%s" Certificado="%s" Sello="%s"`, noCertificado, certBase64, selloBase64)
	signedXML := strings.Replace(xmlStr, "<cfdi:Comprobante", "<cfdi:Comprobante"+signedAttrs, 1)

	return []byte(signedXML), nil
}

func formatCentsDecimal(cents int64) string {
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}

// BuildFacturaGlobalDiaria creates and signs a daily CFDI 4.0 Factura Global for PUBLICO EN GENERAL (XAXX010101000).
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

	rsaKey, err := parseRSAPrivateKeyDER(csdKey)
	if err != nil {
		return nil, ErrSigningFailed
	}
	cert, err := x509.ParseCertificate(csdCert)
	if err != nil {
		return nil, ErrSigningFailed
	}

	noCertificado := fmt.Sprintf("%020s", cert.SerialNumber.String())
	if len(noCertificado) > 20 {
		noCertificado = noCertificado[len(noCertificado)-20:]
	}
	certBase64 := base64.StdEncoding.EncodeToString(csdCert)

	now := time.Now().UTC()
	baseStr := formatCentsDecimal(baseCents)
	ivaStr := formatCentsDecimal(ivaCents)
	totalStr := formatCentsDecimal(totalGrossCents)

	comp := &CFDIComprobante{
		Version:           "4.0",
		Serie:             "FG",
		Folio:             fmt.Sprintf("FG-%s", now.Format("20060102")),
		Fecha:             now.Format("2006-01-02T15:04:05"),
		FormaPago:         "04",
		NoCertificado:     noCertificado,
		Certificado:       certBase64,
		SubTotal:          baseStr,
		Moneda:            "MXN",
		Total:             totalStr,
		TipoDeComprobante: "I",
		Exportacion:       "01",
		MetodoPago:        "PUE",
		LugarExpedicion:   "06000",
		InformacionGlobal: &CFDIInformacionGlobal{
			Periodicidad: "01", // Diaria
			Meses:        fmt.Sprintf("%02d", int(now.Month())),
			Anio:         now.Year(),
		},
		Emisor: emisor,
		Receptor: CFDIReceptor{
			Rfc:                     "XAXX010101000",
			Nombre:                  "PUBLICO EN GENERAL",
			DomicilioFiscalReceptor: "06000",
			RegimenFiscalReceptor:   "616",
			UsoCFDI:                 "S01",
		},
		Conceptos: []CFDIConcepto{
			{
				ClaveProdServ: "01010101",
				Cantidad:      "1",
				ClaveUnidad:   "ACT",
				Descripcion:   "Venta global diaria al público en general",
				ValorUnitario: baseStr,
				Importe:       baseStr,
				ObjetoImp:     "02",
			},
		},
		Impuestos: CFDIImpuestos{
			TotalImpuestosTrasladados: ivaStr,
			Traslados: []CFDITraslado{
				{
					Base:       baseStr,
					Impuesto:   "002",
					TipoFactor: "Tasa",
					TasaOCuota: "0.160000",
					Importe:    ivaStr,
				},
			},
		},
	}

	cadena := BuildCadenaOriginal(comp)
	hash := sha256.Sum256([]byte(cadena))
	sigBytes, err := rsa.SignPKCS1v15(rand.Reader, rsaKey, crypto.SHA256, hash[:])
	if err != nil {
		return nil, ErrSigningFailed
	}
	comp.Sello = base64.StdEncoding.EncodeToString(sigBytes)

	return BuildCFDI40XML(comp)
}
