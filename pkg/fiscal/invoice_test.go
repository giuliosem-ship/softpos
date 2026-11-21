package fiscal_test

import (
	"strings"
	"testing"
	"time"

	"softpos/pkg/fiscal"
)

func invoiceSpec() fiscal.InvoiceSpec {
	b1, i1 := fiscal.IVAFromGross(11600)
	b2, i2 := fiscal.IVAFromGross(1)
	return fiscal.InvoiceSpec{
		Serie: "K", Folio: "7", Fecha: time.Date(2026, 11, 20, 18, 30, 0, 0, time.UTC), FormaPago: "04", LugarExpedicion: "06000",
		Emisor:        fiscal.CFDIEmisor{Rfc: "CACX7605101P8", Nombre: "XOCHILT CASAS CHAVEZ", RegimenFiscal: "626"},
		Receptor:      fiscal.CFDIReceptor{Rfc: "EKU9003173C9", Nombre: "ESCUELA KEMPER URGATE", DomicilioFiscalReceptor: "26015", RegimenFiscalReceptor: "601", UsoCFDI: "G03"},
		ClaveProdServ: "90101501", ClaveUnidad: "E48", Descripcion: "Consumo",
		Lines:     []fiscal.InvoiceLine{{SaleID: "cp_1", BaseCents: b1, IVACents: i1}, {SaleID: "spei_40012:X", BaseCents: b2, IVACents: i2}},
		RetainISR: true,
	}
}

func TestBuildInvoiceComprobante_TotalsRetentionAndCadena(t *testing.T) {
	spec := invoiceSpec()
	comp, tot, err := fiscal.BuildInvoiceComprobante(spec)
	if err != nil {
		t.Fatal(err)
	}
	// 116.00 -> 100.00 + 16.00; 0.01 -> 0.01 + 0.00; ISR 1.25 % of 100.00 = 1.25 (+ 0 on 0.01)
	if tot.SubtotalCents != 10001 || tot.IVACents != 1600 || tot.ISRRetenidoCents != 125 || tot.TotalCents != 10001+1600-125 {
		t.Fatalf("totals: %+v", tot)
	}
	if comp.Fecha != "2026-11-20T12:30:00" || comp.Total != "114.76" || comp.Impuestos.TotalImpuestosRetenidos != "1.25" || len(comp.Conceptos) != 2 {
		t.Fatalf("comprobante: fecha=%s total=%s %+v", comp.Fecha, comp.Total, comp.Impuestos)
	}
	if comp.Conceptos[1].NoIdentificacion != "spei_40012:X" || comp.Conceptos[0].ClaveProdServ != "90101501" {
		t.Fatalf("conceptos: %+v", comp.Conceptos)
	}

	g := spec
	g.RetainISR = false
	g.Receptor = fiscal.PublicoEnGeneral("06000")
	g.Global = &fiscal.CFDIInformacionGlobal{Periodicidad: "04", Meses: "11", Anio: 2026}
	g.Replaces = []string{"5FB2822E-396D-4725-8521-CDC4BDD20CCF"}
	gc, gt, err := fiscal.BuildInvoiceComprobante(g)
	if err != nil || gt.ISRRetenidoCents != 0 || gc.Conceptos[0].ClaveProdServ != "01010101" || gc.Conceptos[0].ClaveUnidad != "ACT" || gc.Conceptos[0].Descripcion != "Venta" {
		t.Fatalf("global: %+v %v", gc.Conceptos, err)
	}

	bin := xsltproc(t)
	xslt := satXSLT(t)
	for _, c := range []*fiscal.CFDIComprobante{comp, gc} {
		c.NoCertificado = "30001000000500003416"
		doc, err := fiscal.BuildCFDI40XML(c)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := fiscal.BuildCadenaOriginal(c), runXSLT(t, bin, xslt, doc); got != want {
			t.Fatalf("built invoice cadena differs from the SAT XSLT\n go: %s\nsat: %s", got, want)
		}
	}
}

func TestSATVerificationURL(t *testing.T) {
	u := fiscal.SATVerificationURL("5FB2822E-396D-4725-8521-CDC4BDD20CCF", "CACX7605101P8", "EKU9003173C9", 1430050, "abc+/rH8/bw==")
	want := "https://verificacfdi.facturaelectronica.sat.gob.mx/default.aspx?id=5FB2822E-396D-4725-8521-CDC4BDD20CCF&re=CACX7605101P8&rr=EKU9003173C9&tt=0000014300.500000&fe=rH8/bw=="
	if u != want {
		t.Fatalf("url\n got %s\nwant %s", u, want)
	}
	if !strings.Contains(fiscal.SATVerificationURL("U", "M&S850101AB1", "X", 1, "12345678"), "re=M%26S850101AB1") {
		t.Fatal("& must be escaped")
	}
}

// TestBuildInvoiceComprobante_Egreso: a nota de crédito is a CFDI de egreso
// (TipoDeComprobante E) related to the credited CFDI with TipoRelacion 01,
// concepto 84111506 / ACT, proportional ISR retention; to the general
// public it keeps the InformacionGlobal of the credited factura global.
func TestBuildInvoiceComprobante_Egreso(t *testing.T) {
	spec := invoiceSpec()
	spec.Serie, spec.Folio, spec.Egreso = "E", "1", true
	spec.Descripcion = "Devolucion de venta"
	spec.Receptor.UsoCFDI = "G02"
	spec.Lines = []fiscal.InvoiceLine{{SaleID: "cp_1", BaseCents: 5000, IVACents: 800}}
	spec.CreditNoteOf = []string{"5FB2822E-396D-4725-8521-CDC4BDD20CCF"}
	comp, tot, err := fiscal.BuildInvoiceComprobante(spec)
	if err != nil {
		t.Fatal(err)
	}
	if comp.TipoDeComprobante != "E" || comp.MetodoPago != "PUE" || len(comp.CfdiRelacionados) != 1 || comp.CfdiRelacionados[0].TipoRelacion != "01" ||
		comp.CfdiRelacionados[0].Relacionados[0].UUID != spec.CreditNoteOf[0] {
		t.Fatalf("egreso header: %+v", comp)
	}
	c := comp.Conceptos[0]
	if c.ClaveProdServ != "84111506" || c.ClaveUnidad != "ACT" || c.Descripcion != "Devolucion de venta" || c.NoIdentificacion != "cp_1" {
		t.Fatalf("egreso concepto: %+v", c)
	}
	// ISR 1.25 % of 50.00 = 0.625 -> 0.63 half-up.
	if tot.SubtotalCents != 5000 || tot.IVACents != 800 || tot.ISRRetenidoCents != 63 || tot.TotalCents != 5800-63 {
		t.Fatalf("egreso totals: %+v", tot)
	}
	noRel := spec
	noRel.CreditNoteOf = nil
	if _, _, err := fiscal.BuildInvoiceComprobante(noRel); err == nil {
		t.Fatal("an egreso must relate the credited CFDI")
	}

	g := spec
	g.RetainISR = false
	g.Receptor = fiscal.PublicoEnGeneral("06000")
	g.Global = &fiscal.CFDIInformacionGlobal{Periodicidad: "04", Meses: "10", Anio: 2026}
	gc, gt, err := fiscal.BuildInvoiceComprobante(g)
	if err != nil || gt.ISRRetenidoCents != 0 || gc.InformacionGlobal == nil || gc.Conceptos[0].ClaveProdServ != "84111506" || gc.TipoDeComprobante != "E" {
		t.Fatalf("egreso to the general public: %+v %v", gc, err)
	}

	bin := xsltproc(t)
	xslt := satXSLT(t)
	for _, c := range []*fiscal.CFDIComprobante{comp, gc} {
		c.NoCertificado = "30001000000500003416"
		doc, err := fiscal.BuildCFDI40XML(c)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(doc), `TipoDeComprobante="E"`) || !strings.Contains(string(doc), `TipoRelacion="01"`) {
			t.Fatalf("egreso XML: %s", doc)
		}
		if got, want := fiscal.BuildCadenaOriginal(c), runXSLT(t, bin, xslt, doc); got != want {
			t.Fatalf("egreso cadena differs from the SAT XSLT\n go: %s\nsat: %s", got, want)
		}
	}
}
