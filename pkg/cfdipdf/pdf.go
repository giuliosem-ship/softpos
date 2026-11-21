// Package cfdipdf renders the representación impresa (PDF) of a stamped
// CFDI 4.0 in pure Go: a minimal PDF 1.4 writer (standard Helvetica fonts,
// WinAnsi encoding) and the SAT verification QR drawn as vector modules
// (rsc.io/qr). See docs/CFDI.md.
package cfdipdf

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/text/encoding/charmap"
	"rsc.io/qr"

	"softpos/pkg/fiscal"
	"softpos/pkg/pac"
)

// ErrNotStamped: the XML has no TimbreFiscalDigital.
var ErrNotStamped = errors.New("CFDI is not stamped")

// Parsed CFDI (local element names, namespace-agnostic).
type cfdiDoc struct {
	Serie, Folio, Fecha, FormaPago, MetodoPago, NoCertificado, SubTotal, Total, Moneda, LugarExpedicion, Sello string `xml:",attr"`
	TipoDeComprobante                                                                                          string `xml:"TipoDeComprobante,attr"`
	Global                                                                                                     *struct {
		Periodicidad string `xml:"Periodicidad,attr"`
		Meses        string `xml:"Meses,attr"`
		Anio         string `xml:"Año,attr"`
	} `xml:"InformacionGlobal"`
	Relacionados []struct {
		TipoRelacion string `xml:"TipoRelacion,attr"`
		UUIDs        []struct {
			UUID string `xml:"UUID,attr"`
		} `xml:"CfdiRelacionado"`
	} `xml:"CfdiRelacionados"`
	Emisor struct {
		Rfc, Nombre, RegimenFiscal string `xml:",attr"`
	} `xml:"Emisor"`
	Receptor struct {
		Rfc, Nombre, DomicilioFiscalReceptor, RegimenFiscalReceptor, UsoCFDI string `xml:",attr"`
	} `xml:"Receptor"`
	Conceptos []struct {
		ClaveProdServ, NoIdentificacion, Cantidad, ClaveUnidad, Descripcion, ValorUnitario, Importe string `xml:",attr"`
	} `xml:"Conceptos>Concepto"`
	Impuestos *struct {
		TotalImpuestosTrasladados string `xml:"TotalImpuestosTrasladados,attr"`
		TotalImpuestosRetenidos   string `xml:"TotalImpuestosRetenidos,attr"`
	} `xml:"Impuestos"`
	TFD *struct {
		Version, UUID, FechaTimbrado, RfcProvCertif, SelloCFD, NoCertificadoSAT, SelloSAT string `xml:",attr"`
	} `xml:"Complemento>TimbreFiscalDigital"`
}

func centsOf(s string) int64 {
	whole, frac, _ := strings.Cut(strings.TrimSpace(s), ".")
	var w, f int64
	_, _ = fmt.Sscanf(whole, "%d", &w)
	frac = (frac + "00")[:2]
	_, _ = fmt.Sscanf(frac, "%d", &f)
	return w*100 + f
}

// Render produces the PDF of a stamped CFDI XML.
func Render(stampedXML []byte) ([]byte, error) {
	var d cfdiDoc
	if err := xml.Unmarshal(stampedXML, &d); err != nil {
		return nil, fmt.Errorf("parse CFDI: %w", err)
	}
	if d.TFD == nil || d.TFD.UUID == "" {
		return nil, ErrNotStamped
	}
	p := newDoc()
	p.text(40, 60, 15, true, "Factura electrónica (CFDI 4.0)")
	p.text(40, 76, 8, false, "Este documento es una representación impresa de un CFDI")
	p.text(380, 60, 10, true, fmt.Sprintf("Serie %s  Folio %s", d.Serie, d.Folio))
	p.text(380, 74, 8, false, "Fecha de emisión: "+d.Fecha)
	p.text(380, 86, 8, false, "Lugar de expedición: "+d.LugarExpedicion)

	y := 110.0
	p.text(40, y, 10, true, "Emisor")
	p.text(310, y, 10, true, "Receptor")
	y += 14
	p.text(40, y, 9, false, d.Emisor.Nombre)
	p.text(310, y, 9, false, d.Receptor.Nombre)
	y += 12
	p.text(40, y, 9, false, "RFC: "+d.Emisor.Rfc)
	p.text(310, y, 9, false, "RFC: "+d.Receptor.Rfc)
	y += 12
	p.text(40, y, 9, false, "Régimen fiscal: "+regimen(d.Emisor.RegimenFiscal))
	p.text(310, y, 9, false, "Régimen fiscal: "+regimen(d.Receptor.RegimenFiscalReceptor))
	y += 12
	p.text(310, y, 9, false, "Domicilio fiscal (CP): "+d.Receptor.DomicilioFiscalReceptor+"   Uso CFDI: "+d.Receptor.UsoCFDI)
	y += 12
	if d.Global != nil {
		p.text(40, y, 9, false, fmt.Sprintf("Factura global: periodicidad %s, meses %s, año %s", d.Global.Periodicidad, d.Global.Meses, d.Global.Anio))
		y += 12
	}
	for _, r := range d.Relacionados {
		for _, u := range r.UUIDs {
			p.text(40, y, 8, false, fmt.Sprintf("CFDI relacionado (tipo %s): %s", r.TipoRelacion, u.UUID))
			y += 11
		}
	}
	y += 8
	header := func() {
		p.rect(40, y-9, 532, 13, 0.9)
		for _, c := range []struct {
			x float64
			s string
		}{{44, "Clave"}, {96, "Cant."}, {126, "Unidad"}, {164, "Descripción / No. identificación"}, {430, "Valor unitario"}, {510, "Importe"}} {
			p.text(c.x, y, 8, true, c.s)
		}
		y += 14
	}
	header()
	for _, k := range d.Conceptos {
		if y > 640 {
			p.newPage()
			y = 50
			header()
		}
		p.text(44, y, 8, false, k.ClaveProdServ)
		p.text(96, y, 8, false, k.Cantidad)
		p.text(126, y, 8, false, k.ClaveUnidad)
		p.text(164, y, 8, false, clip(k.Descripcion, 55))
		p.textRight(500, y, 8, false, k.ValorUnitario)
		p.textRight(568, y, 8, false, k.Importe)
		if k.NoIdentificacion != "" {
			y += 10
			p.text(164, y, 7, false, clip(k.NoIdentificacion, 70))
		}
		y += 12
	}
	if y > 560 {
		p.newPage()
		y = 50
	}
	y += 6
	iva, ret := "0.00", ""
	if d.Impuestos != nil {
		iva, ret = orZero(d.Impuestos.TotalImpuestosTrasladados), d.Impuestos.TotalImpuestosRetenidos
	}
	rows := [][2]string{{"Subtotal", d.SubTotal}, {"IVA trasladado 16 %", iva}}
	if ret != "" {
		rows = append(rows, [2]string{"ISR retenido", ret})
	}
	rows = append(rows, [2]string{"Total " + d.Moneda, d.Total})
	for i, r := range rows {
		p.text(400, y, 9, i == len(rows)-1, r[0])
		p.textRight(568, y, 9, i == len(rows)-1, r[1])
		y += 12
	}
	p.text(40, y-36, 8, false, "Forma de pago: "+d.FormaPago+"   Método de pago: "+d.MetodoPago)
	y += 8

	qrTop := y
	url := fiscal.SATVerificationURL(d.TFD.UUID, d.Emisor.Rfc, d.Receptor.Rfc, centsOf(d.Total), d.Sello)
	if code, err := qr.Encode(url, qr.M); err == nil {
		p.qr(40, qrTop, 110, code)
	}
	x := 165.0
	for _, l := range []string{
		"Folio fiscal (UUID): " + d.TFD.UUID,
		"Fecha de certificación: " + d.TFD.FechaTimbrado,
		"No. de certificado del emisor: " + d.NoCertificado,
		"No. de certificado del SAT: " + d.TFD.NoCertificadoSAT,
		"RFC del proveedor de certificación: " + d.TFD.RfcProvCertif,
	} {
		p.text(x, y, 8, false, l)
		y += 11
	}
	y = maxf(y, qrTop+118)
	tfd := pac.TFDCadenaOriginal(&pac.TimbreFiscalDigital{Version: d.TFD.Version, UUID: d.TFD.UUID, FechaTimbrado: parseTime(d.TFD.FechaTimbrado),
		RfcProvCertif: d.TFD.RfcProvCertif, SelloCFD: d.TFD.SelloCFD, NoCertificadoSAT: d.TFD.NoCertificadoSAT})
	for _, block := range []struct{ title, body string }{
		{"Sello digital del CFDI", d.Sello},
		{"Sello digital del SAT", d.TFD.SelloSAT},
		{"Cadena original del complemento de certificación digital del SAT", tfd},
	} {
		if y > 730 {
			p.newPage()
			y = 50
		}
		p.text(40, y, 8, true, block.title)
		y += 10
		for _, line := range wrap(block.body, 120) {
			if y > 760 {
				p.newPage()
				y = 50
			}
			p.text(40, y, 6.5, false, line)
			y += 8
		}
		y += 4
	}
	return p.bytes(), nil
}

func orZero(s string) string {
	if s == "" {
		return "0.00"
	}
	return s
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func parseTime(s string) time.Time {
	t, _ := time.Parse("2006-01-02T15:04:05", s)
	return t
}

func regimen(code string) string {
	if e, ok := fiscal.RegimenFiscal(code); ok {
		return code + " " + e.Description
	}
	return code
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func wrap(s string, n int) []string {
	var out []string
	r := []rune(s)
	for len(r) > n {
		out = append(out, string(r[:n]))
		r = r[n:]
	}
	if len(r) > 0 {
		out = append(out, string(r))
	}
	return out
}

// ---------------------------------------------------------------------------
// Minimal PDF writer: Letter pages (612 x 792 pt), top-left coordinates.

type doc struct {
	pages []*bytes.Buffer
}

func newDoc() *doc { return &doc{pages: []*bytes.Buffer{{}}} }

func (d *doc) cur() *bytes.Buffer { return d.pages[len(d.pages)-1] }
func (d *doc) newPage()           { d.pages = append(d.pages, &bytes.Buffer{}) }

var winAnsi = charmap.Windows1252.NewEncoder()

// pdfString encodes s as a WinAnsi literal string (unencodable runes -> '?').
func pdfString(s string) string {
	var b strings.Builder
	b.WriteByte('(')
	for _, r := range s {
		enc, err := winAnsi.String(string(r))
		if err != nil || len(enc) != 1 {
			enc = "?"
		}
		switch c := enc[0]; c {
		case '(', ')', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			if c < 32 {
				b.WriteByte(' ')
			} else if c > 126 {
				fmt.Fprintf(&b, "\\%03o", c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte(')')
	return b.String()
}

const pageHeight = 792.0

func (d *doc) text(x, y, size float64, bold bool, s string) {
	font := "F1"
	if bold {
		font = "F2"
	}
	fmt.Fprintf(d.cur(), "BT /%s %.1f Tf %.2f %.2f Td %s Tj ET\n", font, size, x, pageHeight-y, pdfString(s))
}

// textRight right-aligns s at x using Helvetica's average digit width.
func (d *doc) textRight(x, y, size float64, bold bool, s string) {
	d.text(x-float64(len([]rune(s)))*size*0.556, y, size, bold, s)
}

func (d *doc) rect(x, y, w, h, gray float64) {
	fmt.Fprintf(d.cur(), "%.2f g %.2f %.2f %.2f %.2f re f 0 g\n", gray, x, pageHeight-y-h, w, h)
}

func (d *doc) qr(x, y, size float64, code *qr.Code) {
	n := code.Size
	m := size / float64(n+8) // 4-module quiet zone
	buf := d.cur()
	buf.WriteString("0 g\n")
	for row := 0; row < n; row++ {
		for col := 0; col < n; col++ {
			if code.Black(col, row) {
				fmt.Fprintf(buf, "%.3f %.3f %.3f %.3f re\n", x+float64(col+4)*m, pageHeight-y-float64(row+5)*m, m, m)
			}
		}
	}
	buf.WriteString("f\n")
}

func (d *doc) bytes() []byte {
	var out bytes.Buffer
	var offsets []int
	obj := func(body string) {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}
	out.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
	n := len(d.pages)
	// 1 catalog, 2 pages, 3 F1, 4 F2, then (page, content) pairs.
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	kids := make([]string, n)
	for i := range kids {
		kids[i] = fmt.Sprintf("%d 0 R", 5+2*i)
	}
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), n))
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>")
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica-Bold /Encoding /WinAnsiEncoding >>")
	for i, pg := range d.pages {
		obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R /F2 4 0 R >> >> /Contents %d 0 R >>", 6+2*i))
		obj(fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", pg.Len(), pg.String()))
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, o := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return out.Bytes()
}
