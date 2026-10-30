package fiscal_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"strings"
	"testing"
	"time"

	"softpos/pkg/fiscal"
)

func generateTestCSDKeys(t *testing.T) ([]byte, []byte) {
	t.Helper()
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate rsa key: %v", err)
	}

	privBytes, err := x509.MarshalPKCS8PrivateKey(privKey)
	if err != nil {
		t.Fatalf("failed to marshal pkcs8: %v", err)
	}

	serial, _ := new(big.Int).SetString("30001000000400002434", 10)
	template := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: "CSD DE PRUEBA SOFT POS",
		},
		NotBefore: time.Now().Add(-1 * time.Hour),
		NotAfter:  time.Now().Add(24 * time.Hour),
	}

	certBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &privKey.PublicKey, privKey)
	if err != nil {
		t.Fatalf("failed to create certificate: %v", err)
	}

	return privBytes, certBytes
}

func TestBuildCFDI40XML_Individual(t *testing.T) {
	comp := &fiscal.CFDIComprobante{
		Version:           "4.0",
		Serie:             "POS",
		Folio:             "1001",
		Fecha:             "2026-09-19T10:00:00",
		FormaPago:         "04", // Tarjeta de crédito
		SubTotal:          "100.00",
		Moneda:            "MXN",
		Total:             "116.00",
		TipoDeComprobante: "I",
		Exportacion:       "01",
		MetodoPago:        "PUE",
		LugarExpedicion:   "06000",
		Emisor: fiscal.CFDIEmisor{
			Rfc:           "XAXX010101000",
			Nombre:        "COMERCIO SOFT POS",
			RegimenFiscal: "626", // RESICO
		},
		Receptor: fiscal.CFDIReceptor{
			Rfc:                     "GARM850101ABC",
			Nombre:                  "JUAN PEREZ GARCIA",
			DomicilioFiscalReceptor: "06000",
			RegimenFiscalReceptor:   "605",
			UsoCFDI:                 "G03",
		},
		Conceptos: []fiscal.CFDIConcepto{
			{
				ClaveProdServ: "50201706",
				Cantidad:      "1",
				ClaveUnidad:   "H87",
				Descripcion:   "Consumo en restaurante",
				ValorUnitario: "100.00",
				Importe:       "100.00",
				ObjetoImp:     "02",
			},
		},
		Impuestos: fiscal.CFDIImpuestos{
			TotalImpuestosTrasladados: "16.00",
			Traslados: []fiscal.CFDITraslado{
				{
					Base:       "100.00",
					Impuesto:   "002",
					TipoFactor: "Tasa",
					TasaOCuota: "0.160000",
					Importe:    "16.00",
				},
			},
		},
	}

	xmlBytes, err := fiscal.BuildCFDI40XML(comp)
	if err != nil {
		t.Fatalf("BuildCFDI40XML failed: %v", err)
	}

	xmlStr := string(xmlBytes)
	if !strings.Contains(xmlStr, "Version=\"4.0\"") {
		t.Errorf("missing Version 4.0 in XML")
	}
	if !strings.Contains(xmlStr, "Rfc=\"GARM850101ABC\"") {
		t.Errorf("missing Receptor RFC in XML")
	}
	if !strings.Contains(xmlStr, "http://www.sat.gob.mx/cfd/4") {
		t.Errorf("missing SAT namespace in XML")
	}
}

func TestBuildFacturaGlobalDiaria_RESICO(t *testing.T) {
	privKey, cert := generateTestCSDKeys(t)

	emisor := fiscal.CFDIEmisor{
		Rfc:           "GARM850101ABC",
		Nombre:        "TAQUERIA EL TACO RESICO",
		RegimenFiscal: "626",
	}

	// 1000.00 gross -> 862.07 base + 137.93 IVA (in cents: 100000, 86207, 13793)
	xmlBytes, err := fiscal.BuildFacturaGlobalDiaria(
		emisor,
		100000,
		86207,
		13793,
		privKey,
		cert,
	)
	if err != nil {
		t.Fatalf("BuildFacturaGlobalDiaria failed: %v", err)
	}

	xmlStr := string(xmlBytes)
	if !strings.Contains(xmlStr, "Rfc=\"XAXX010101000\"") {
		t.Errorf("expected Generic RFC XAXX010101000 for Factura Global")
	}
	if !strings.Contains(xmlStr, "Periodicidad=\"01\"") {
		t.Errorf("expected Periodicidad 01 (Diaria) for Factura Global")
	}
	if !strings.Contains(xmlStr, "Sello=\"") {
		t.Errorf("expected Sello attribute in signed CFDI XML")
	}
	if !strings.Contains(xmlStr, "Certificado=\"") {
		t.Errorf("expected Certificado attribute in signed CFDI XML")
	}
}

func TestBuildCadenaOriginal(t *testing.T) {
	comp := &fiscal.CFDIComprobante{
		Version:           "4.0",
		Serie:             "A",
		Folio:             "1",
		Fecha:             "2026-09-19T10:00:00",
		FormaPago:         "01",
		SubTotal:          "100.00",
		Moneda:            "MXN",
		Total:             "116.00",
		TipoDeComprobante: "I",
		Exportacion:       "01",
		MetodoPago:        "PUE",
		LugarExpedicion:   "06000",
		Emisor: fiscal.CFDIEmisor{
			Rfc:           "AAA010101AAA",
			Nombre:        "EMISOR PRUEBA",
			RegimenFiscal: "601",
		},
		Receptor: fiscal.CFDIReceptor{
			Rfc:                     "XAXX010101000",
			Nombre:                  "PUBLICO EN GENERAL",
			DomicilioFiscalReceptor: "06000",
			RegimenFiscalReceptor:   "616",
			UsoCFDI:                 "S01",
		},
	}

	cadena := fiscal.BuildCadenaOriginal(comp)
	if !strings.HasPrefix(cadena, "||4.0|") {
		t.Errorf("Cadena original must start with ||4.0|, got: %s", cadena)
	}
	if !strings.HasSuffix(cadena, "||") {
		t.Errorf("Cadena original must end with ||, got: %s", cadena)
	}
}
