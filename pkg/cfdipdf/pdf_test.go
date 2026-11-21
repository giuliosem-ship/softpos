package cfdipdf_test

import (
	"bytes"
	"context"
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"

	"softpos/pkg/cfdipdf"
	"softpos/pkg/fiscal"
	"softpos/pkg/fiscal/fiscaltest"
	"softpos/pkg/pac"
)

func stampedXML(t *testing.T, lines int) ([]byte, *pac.TimbreFiscalDigital) {
	t.Helper()
	c, err := fiscaltest.NewCSD(fiscaltest.CSDOptions{RFC: "CACX7605101P8"})
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := fiscal.NewCSDSigner(c.CertDER, c.PlainKey)
	spec := fiscal.InvoiceSpec{Serie: "K", Folio: "12", Fecha: time.Now(), FormaPago: "04", LugarExpedicion: "06000",
		Emisor:        fiscal.CFDIEmisor{Rfc: "CACX7605101P8", Nombre: "XOCHILT CASAS CHÁVEZ (PIÑATAS)", RegimenFiscal: "626"},
		Receptor:      fiscal.CFDIReceptor{Rfc: "EKU9003173C9", Nombre: "ESCUELA KEMPER URGATE", DomicilioFiscalReceptor: "26015", RegimenFiscalReceptor: "601", UsoCFDI: "G03"},
		ClaveProdServ: "90101501", ClaveUnidad: "E48", Descripcion: "Consumo en restaurante", RetainISR: true}
	for i := 0; i < lines; i++ {
		b, v := fiscal.IVAFromGross(int64(11600 + i))
		spec.Lines = append(spec.Lines, fiscal.InvoiceLine{SaleID: "cp_" + strconv.Itoa(i), BaseCents: b, IVACents: v})
	}
	comp, _, err := fiscal.BuildInvoiceComprobante(spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := signer.Seal(comp); err != nil {
		t.Fatal(err)
	}
	x, _ := fiscal.BuildCFDI40XML(comp)
	resp, err := pac.NewMockPACClient(false, "").Stamp(context.Background(), x)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pac.InjectTimbreFiscal(x, resp.Timbre)
	if err != nil {
		t.Fatal(err)
	}
	return out, resp.Timbre
}

func TestRender_StructureAndContent(t *testing.T) {
	x, tfd := stampedXML(t, 3)
	pdf, err := cfdipdf.Render(x)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF-1.4")) || !bytes.HasSuffix(pdf, []byte("%%EOF\n")) {
		t.Fatal("not a PDF")
	}
	for _, want := range []string{tfd.UUID, "CACX7605101P8", "EKU9003173C9", "Total MXN", "ISR retenido", "Sello digital del SAT", "||1.1|" + tfd.UUID} {
		if !bytes.Contains(pdf, []byte(want)) {
			t.Errorf("PDF misses %q", want)
		}
	}
	if !bytes.Contains(pdf, []byte(`CH\301VEZ`)) { // Á in WinAnsi (octal escape)
		t.Error("accents must be WinAnsi-encoded")
	}
	if n := bytes.Count(pdf, []byte(" re\n")); n < 100 {
		t.Errorf("QR modules not drawn (%d rectangles)", n)
	}
	// xref offsets point at "N 0 obj".
	m := regexp.MustCompile(`startxref\n(\d+)`).FindSubmatch(pdf)
	off, _ := strconv.Atoi(string(m[1]))
	if !bytes.HasPrefix(pdf[off:], []byte("xref")) {
		t.Fatal("bad startxref")
	}
	entries := regexp.MustCompile(`(\d{10}) 00000 n `).FindAllSubmatch(pdf[off:], -1)
	for i, e := range entries {
		o, _ := strconv.Atoi(string(e[1]))
		if !bytes.HasPrefix(pdf[o:], []byte(strconv.Itoa(i+1)+" 0 obj")) {
			t.Fatalf("xref entry %d points at %q", i+1, pdf[o:o+10])
		}
	}
	if p := os.Getenv("CFDI_PDF_OUT"); p != "" {
		_ = os.WriteFile(p, pdf, 0o600)
	}
}

func TestRender_PaginatesManyConceptosAndRejectsUnstamped(t *testing.T) {
	x, _ := stampedXML(t, 120)
	pdf, err := cfdipdf.Render(x)
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(pdf, []byte("/Type /Page ")); n < 3 {
		t.Fatalf("120 conceptos must span several pages, got %d", n)
	}
	if _, err := cfdipdf.Render([]byte(`<cfdi:Comprobante xmlns:cfdi="http://www.sat.gob.mx/cfd/4"/>`)); err != cfdipdf.ErrNotStamped {
		t.Fatalf("unstamped: %v", err)
	}
}
