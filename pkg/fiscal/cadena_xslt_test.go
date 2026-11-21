package fiscal_test

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"softpos/pkg/fiscal"
	"softpos/pkg/fiscal/fiscaltest"
)

// Golden tests: the Go cadena original builder must produce exactly what the
// official SAT XSLT (testdata/sat, see README.md) produces with xsltproc for
// the XML that BuildCFDI40XML emits.

type goldenVariant struct {
	name string
	comp *fiscal.CFDIComprobante
}

func tras16(base, iva string) fiscal.CFDITraslado {
	return fiscal.CFDITraslado{Base: base, Impuesto: "002", TipoFactor: "Tasa", TasaOCuota: "0.160000", Importe: iva}
}

func baseComp() *fiscal.CFDIComprobante {
	return &fiscal.CFDIComprobante{
		Version: "4.0", Serie: "K", Folio: "1001", Fecha: "2026-11-20T10:15:00", FormaPago: "04",
		NoCertificado: "30001000000500003416", SubTotal: "100.00", Moneda: "MXN", Total: "116.00",
		TipoDeComprobante: "I", Exportacion: "01", MetodoPago: "PUE", LugarExpedicion: "06000",
		Emisor: fiscal.CFDIEmisor{Rfc: "CACX7605101P8", Nombre: "XOCHILT CASAS CHAVEZ", RegimenFiscal: "626"},
		Receptor: fiscal.CFDIReceptor{Rfc: "EKU9003173C9", Nombre: "ESCUELA KEMPER URGATE",
			DomicilioFiscalReceptor: "26015", RegimenFiscalReceptor: "601", UsoCFDI: "G03"},
		Conceptos: []fiscal.CFDIConcepto{{
			ClaveProdServ: "90101501", Cantidad: "1", ClaveUnidad: "E48", Descripcion: "Consumo en restaurante",
			ValorUnitario: "100.00", Importe: "100.00", ObjetoImp: "02",
			Impuestos: &fiscal.CFDIConceptoImpuestos{Traslados: []fiscal.CFDITraslado{tras16("100.00", "16.00")}},
		}},
		Impuestos: &fiscal.CFDIImpuestos{TotalImpuestosTrasladados: "16.00", Traslados: []fiscal.CFDITraslado{tras16("100.00", "16.00")}},
	}
}

func goldenVariants() []goldenVariant {
	var out []goldenVariant
	add := func(name string, mut func(c *fiscal.CFDIComprobante)) {
		c := baseComp()
		mut(c)
		out = append(out, goldenVariant{name, c})
	}
	add("receptor persona moral, IVA 16%", func(c *fiscal.CFDIComprobante) {})
	add("receptor persona fisica, no serie/folio", func(c *fiscal.CFDIComprobante) {
		c.Serie, c.Folio = "", ""
		c.Receptor = fiscal.CFDIReceptor{Rfc: "XIQB891116QE4", Nombre: "BERENICE XIMO QUEZADA",
			DomicilioFiscalReceptor: "88730", RegimenFiscalReceptor: "612", UsoCFDI: "D01"}
	})
	add("factura global with InformacionGlobal and many conceptos", func(c *fiscal.CFDIComprobante) {
		c.Serie, c.Folio = "G", "7"
		c.InformacionGlobal = &fiscal.CFDIInformacionGlobal{Periodicidad: "04", Meses: "11", Anio: 2026}
		c.Receptor = fiscal.PublicoEnGeneral("06000")
		c.Conceptos = nil
		for i, amt := range []string{"10.00", "25.50", "64.50"} {
			iva := []string{"1.60", "4.08", "10.32"}[i]
			c.Conceptos = append(c.Conceptos, fiscal.CFDIConcepto{ClaveProdServ: "01010101", NoIdentificacion: "spei_40012:TICKET" + amt,
				Cantidad: "1", ClaveUnidad: "ACT", Descripcion: "Venta", ValorUnitario: amt, Importe: amt, ObjetoImp: "02",
				Impuestos: &fiscal.CFDIConceptoImpuestos{Traslados: []fiscal.CFDITraslado{tras16(amt, iva)}}})
		}
	})
	add("IVA 0%", func(c *fiscal.CFDIComprobante) {
		c.Total = "100.00"
		t0 := fiscal.CFDITraslado{Base: "100.00", Impuesto: "002", TipoFactor: "Tasa", TasaOCuota: "0.000000", Importe: "0.00"}
		c.Conceptos[0].Impuestos.Traslados = []fiscal.CFDITraslado{t0}
		c.Impuestos = &fiscal.CFDIImpuestos{TotalImpuestosTrasladados: "0.00", Traslados: []fiscal.CFDITraslado{t0}}
	})
	add("IVA exento (no TasaOCuota/Importe, no total trasladados)", func(c *fiscal.CFDIComprobante) {
		c.Total = "100.00"
		ex := fiscal.CFDITraslado{Base: "100.00", Impuesto: "002", TipoFactor: "Exento"}
		c.Conceptos[0].Impuestos.Traslados = []fiscal.CFDITraslado{ex}
		c.Impuestos = &fiscal.CFDIImpuestos{Traslados: []fiscal.CFDITraslado{ex}}
	})
	add("retenciones ISR 1.25% (113-J) and IVA", func(c *fiscal.CFDIComprobante) {
		c.Total = "104.08"
		c.Conceptos[0].Impuestos.Retenciones = []fiscal.CFDIRetencionConcepto{
			{Base: "100.00", Impuesto: "001", TipoFactor: "Tasa", TasaOCuota: "0.012500", Importe: "1.25"},
			{Base: "100.00", Impuesto: "002", TipoFactor: "Tasa", TasaOCuota: "0.106667", Importe: "10.67"},
		}
		c.Impuestos.TotalImpuestosRetenidos = "11.92"
		c.Impuestos.Retenciones = []fiscal.CFDIRetencion{{Impuesto: "001", Importe: "1.25"}, {Impuesto: "002", Importe: "10.67"}}
	})
	add("special characters & < > \" ' and whitespace collapsing", func(c *fiscal.CFDIComprobante) {
		c.Receptor.Nombre = "  PERSONAS & \"SOCIOS\" <UNIDOS>  'LA ÑOÑA'  "
		c.Conceptos[0].Descripcion = "Taco\tal   pastor\n con \r\n piña | doble queso  "
		c.Emisor.Nombre = "ÁNGELES   MÜLLER\tÑUÑEZ"
		c.Conceptos[0].NoIdentificacion = " SKU-001 "
	})
	add("multiple conceptos, ObjetoImp 01 without impuestos", func(c *fiscal.CFDIComprobante) {
		c.SubTotal, c.Total = "150.00", "166.00"
		c.Conceptos = append(c.Conceptos, fiscal.CFDIConcepto{ClaveProdServ: "84111506", Cantidad: "2.5", ClaveUnidad: "H87",
			Unidad: "Pieza", Descripcion: "Propina (no objeto)", ValorUnitario: "20.00", Importe: "50.00", ObjetoImp: "01"})
	})
	add("CfdiRelacionados (sustitucion 04)", func(c *fiscal.CFDIComprobante) {
		c.CfdiRelacionados = []fiscal.CFDIRelacionados{{TipoRelacion: "04", Relacionados: []fiscal.CFDIRelacionadoUUID{
			{UUID: "5FB2822E-396D-4725-8521-CDC4BDD20CCF"}, {UUID: "1C2E1E4A-0B39-4F2B-8C55-5A0B9D1E7A10"}}}}
	})
	add("required attribute empty, optional absent", func(c *fiscal.CFDIComprobante) {
		c.NoCertificado = ""
		c.FormaPago, c.MetodoPago = "", ""
	})
	add("optional attribute present but blank", func(c *fiscal.CFDIComprobante) {
		c.Serie = "   "
		c.CondicionesDePago = "Contado"
		c.Descuento = "0.00"
		c.Conceptos[0].Descuento = "0.00"
	})
	add("moneda USD with TipoCambio and Confirmacion", func(c *fiscal.CFDIComprobante) {
		c.Moneda, c.TipoCambio, c.Confirmacion = "USD", "17.2512", "A1b2C"
		c.Receptor.ResidenciaFiscal, c.Receptor.NumRegIdTrib = "USA", "123456789"
		c.Receptor.Rfc = fiscal.RFCExtranjero
		c.Exportacion = "02"
	})
	return out
}

var includeRe = regexp.MustCompile(`<xsl:include href="[^"]*"\s*/>`)

// satXSLT writes the official stylesheet with the utilerias include pointed
// at the vendored copy and the complement includes removed (offline run).
func satXSLT(t *testing.T) string {
	t.Helper()
	main, err := os.ReadFile(filepath.Join("testdata", "sat", "cadenaoriginal_4_0.xslt"))
	if err != nil {
		t.Fatal(err)
	}
	util, err := os.ReadFile(filepath.Join("testdata", "sat", "utilerias.xslt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(main, []byte("cadenaoriginal_2_0/utilerias.xslt")) {
		t.Fatal("vendored XSLT does not include utilerias.xslt as expected")
	}
	dir := t.TempDir()
	patched := includeRe.ReplaceAllFunc(main, func(m []byte) []byte {
		if bytes.Contains(m, []byte("utilerias.xslt")) {
			return []byte(`<xsl:include href="utilerias.xslt"/>`)
		}
		return nil
	})
	if err := os.WriteFile(filepath.Join(dir, "utilerias.xslt"), util, 0o600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "cadenaoriginal_4_0.xslt")
	if err := os.WriteFile(p, patched, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func xsltproc(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("xsltproc")
	if err != nil {
		t.Skip("xsltproc not installed: SAT XSLT golden tests skipped (install libxslt: brew install libxslt / apt-get install xsltproc)")
	}
	return bin
}

func runXSLT(t *testing.T, bin, xslt string, xmlDoc []byte) string {
	t.Helper()
	in := filepath.Join(t.TempDir(), "cfdi.xml")
	if err := os.WriteFile(in, xmlDoc, 0o600); err != nil {
		t.Fatal(err)
	}
	// stdout only: xsltproc warns on stderr that the SAT stylesheets declare
	// version 2.0 (they only use XSLT 1.0 features).
	var stderr bytes.Buffer
	cmd := exec.Command(bin, "--nonet", xslt, in)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("xsltproc: %v\n%s", err, stderr.String())
	}
	return string(out)
}

func TestCadenaOriginal_MatchesSATXSLT(t *testing.T) {
	bin := xsltproc(t)
	xslt := satXSLT(t)
	variants := goldenVariants()
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			doc, err := fiscal.BuildCFDI40XML(v.comp)
			if err != nil {
				t.Fatal(err)
			}
			want := runXSLT(t, bin, xslt, doc)
			if got := fiscal.BuildCadenaOriginal(v.comp); got != want {
				t.Fatalf("cadena differs from the SAT XSLT\n go: %q\nsat: %q", got, want)
			}
		})
	}
	t.Logf("%d CFDI variants match the official SAT XSLT", len(variants))
}

// TestSello_VerifiesAgainstXSLTCadena: a sealed XML's Sello verifies over
// the cadena the SAT XSLT computes from that very XML (what the SAT/PAC do).
func TestSello_VerifiesAgainstXSLTCadena(t *testing.T) {
	bin := xsltproc(t)
	xslt := satXSLT(t)
	c, err := fiscaltest.NewCSD(fiscaltest.CSDOptions{RFC: "CACX7605101P8"})
	if err != nil {
		t.Fatal(err)
	}
	signer, err := fiscal.NewCSDSignerFromSAT(c.CertDER, c.EncryptedKey, c.Password)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range goldenVariants() {
		if _, err := signer.Seal(v.comp); err != nil {
			t.Fatal(err)
		}
		doc, err := fiscal.BuildCFDI40XML(v.comp)
		if err != nil {
			t.Fatal(err)
		}
		cadena := runXSLT(t, bin, xslt, doc)
		sig, _ := base64.StdEncoding.DecodeString(v.comp.Sello)
		h := sha256.Sum256([]byte(cadena))
		if err := rsa.VerifyPKCS1v15(&c.Key.PublicKey, crypto.SHA256, h[:], sig); err != nil {
			t.Fatalf("%s: sello does not verify over the SAT cadena: %v", v.name, err)
		}
		if !strings.Contains(string(doc), `NoCertificado="30001000000500003416"`) {
			t.Fatalf("%s: NoCertificado missing", v.name)
		}
	}
}

func TestBuildCFDI40XML_RejectsInvalidXMLCharacters(t *testing.T) {
	c := baseComp()
	c.Receptor.Nombre = "BAD\x01NAME"
	if _, err := fiscal.BuildCFDI40XML(c); err == nil {
		t.Fatal("control characters must be rejected (the sello would not match)")
	}
}
