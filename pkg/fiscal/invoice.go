package fiscal

import (
	"fmt"
	"time"
)

// Invoice building from sales (MVP-22): one concepto per sale, amounts in
// int64 cents taken from the posted tax split (base + IVA 16 %), never from
// the client.

// InvoiceLine is one sale of an invoice.
type InvoiceLine struct {
	SaleID    string
	BaseCents int64 // valor unitario / importe (sin IVA)
	IVACents  int64 // IVA trasladado 16 %
}

// InvoiceSpec describes a CFDI de ingreso to build.
type InvoiceSpec struct {
	Serie, Folio    string
	Fecha           time.Time // rendered in America/Mexico_City
	FormaPago       string    // c_FormaPago (04 tarjeta, 03 transferencia, …)
	LugarExpedicion string
	Emisor          CFDIEmisor
	Receptor        CFDIReceptor
	// Global marks a factura global (InformacionGlobal; conceptos 01010101 /
	// ACT / "Venta" as the SAT requires).
	Global        *CFDIInformacionGlobal
	ClaveProdServ string
	ClaveUnidad   string
	Descripcion   string
	Lines         []InvoiceLine
	// RetainISR adds the 1.25 % ISR retention a persona moral withholds
	// from a RESICO persona física (Art. 113-J LISR).
	RetainISR bool
	// Replaces are UUIDs this CFDI substitutes (TipoRelacion 04).
	Replaces []string
}

// InvoiceTotals are the computed amounts.
type InvoiceTotals struct {
	SubtotalCents    int64
	IVACents         int64
	ISRRetenidoCents int64
	TotalCents       int64
}

// ISRRetentionBps is the Art. 113-J LISR rate (1.25 %).
const ISRRetentionBps = 125

const (
	tasaIVA16 = "0.160000"
	tasaISR   = "0.012500"
)

// BuildInvoiceComprobante builds the (unsealed) comprobante for spec.
func BuildInvoiceComprobante(spec InvoiceSpec) (*CFDIComprobante, InvoiceTotals, error) {
	var tot InvoiceTotals
	if len(spec.Lines) == 0 || spec.Emisor.Rfc == "" || spec.Receptor.Rfc == "" || !ValidPostalCode(spec.LugarExpedicion) {
		return nil, tot, ErrInvalidCFDIData
	}
	clave, unidad, desc := spec.ClaveProdServ, spec.ClaveUnidad, spec.Descripcion
	if spec.Global != nil {
		clave, unidad, desc = "01010101", "ACT", "Venta"
	}
	if clave == "" || unidad == "" || desc == "" {
		return nil, tot, ErrInvalidCFDIData
	}
	var conceptos []CFDIConcepto
	for _, l := range spec.Lines {
		if l.BaseCents <= 0 || l.IVACents < 0 {
			return nil, tot, fmt.Errorf("%w: sale %s has no taxable base", ErrInvalidCFDIData, l.SaleID)
		}
		base, iva := FormatCents(l.BaseCents), FormatCents(l.IVACents)
		imp := &CFDIConceptoImpuestos{Traslados: []CFDITraslado{{Base: base, Impuesto: "002", TipoFactor: "Tasa", TasaOCuota: tasaIVA16, Importe: iva}}}
		if spec.RetainISR {
			isr := (l.BaseCents*ISRRetentionBps + 5000) / 10000
			tot.ISRRetenidoCents += isr
			imp.Retenciones = []CFDIRetencionConcepto{{Base: base, Impuesto: "001", TipoFactor: "Tasa", TasaOCuota: tasaISR, Importe: FormatCents(isr)}}
		}
		tot.SubtotalCents += l.BaseCents
		tot.IVACents += l.IVACents
		conceptos = append(conceptos, CFDIConcepto{ClaveProdServ: clave, NoIdentificacion: truncateString(l.SaleID, 100), Cantidad: "1",
			ClaveUnidad: unidad, Descripcion: desc, ValorUnitario: base, Importe: base, ObjetoImp: "02", Impuestos: imp})
	}
	tot.TotalCents = tot.SubtotalCents + tot.IVACents - tot.ISRRetenidoCents
	impuestos := &CFDIImpuestos{
		TotalImpuestosTrasladados: FormatCents(tot.IVACents),
		Traslados: []CFDITraslado{{Base: FormatCents(tot.SubtotalCents), Impuesto: "002", TipoFactor: "Tasa",
			TasaOCuota: tasaIVA16, Importe: FormatCents(tot.IVACents)}},
	}
	if spec.RetainISR {
		impuestos.TotalImpuestosRetenidos = FormatCents(tot.ISRRetenidoCents)
		impuestos.Retenciones = []CFDIRetencion{{Impuesto: "001", Importe: FormatCents(tot.ISRRetenidoCents)}}
	}
	formaPago := spec.FormaPago
	if formaPago == "" {
		formaPago = "99"
	}
	comp := &CFDIComprobante{
		Version: "4.0", Serie: spec.Serie, Folio: spec.Folio,
		Fecha:     spec.Fecha.In(MexicoCityLocation()).Format("2006-01-02T15:04:05"),
		FormaPago: formaPago, SubTotal: FormatCents(tot.SubtotalCents), Moneda: "MXN", Total: FormatCents(tot.TotalCents),
		TipoDeComprobante: "I", Exportacion: "01", MetodoPago: "PUE", LugarExpedicion: spec.LugarExpedicion,
		InformacionGlobal: spec.Global, Emisor: spec.Emisor, Receptor: spec.Receptor, Conceptos: conceptos, Impuestos: impuestos,
	}
	if len(spec.Replaces) > 0 {
		rel := CFDIRelacionados{TipoRelacion: "04"}
		for _, u := range spec.Replaces {
			rel.Relacionados = append(rel.Relacionados, CFDIRelacionadoUUID{UUID: u})
		}
		comp.CfdiRelacionados = []CFDIRelacionados{rel}
	}
	return comp, tot, nil
}

func truncateString(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// IVAFromGross splits a 16 %-IVA-inclusive gross amount exactly as the
// RESICO split posts it (CalculateResicoTaxSplit): base = round(gross/1.16).
func IVAFromGross(grossCents int64) (baseCents, ivaCents int64) {
	baseCents = (grossCents*100 + 58) / 116
	return baseCents, grossCents - baseCents
}

// IVAWithinSATLimits reports whether a 16 % traslado of ivaCents is within
// the CFDI 4.0 limits for baseCents: from (base - 0.005) * 0.16 truncated to
// (base + 0.005) * 0.16 rounded up, at 2 decimals.
func IVAWithinSATLimits(baseCents, ivaCents int64) bool {
	lo := (2*baseCents - 1) * 8 / 100
	hi := ((2*baseCents+1)*8 + 99) / 100
	return ivaCents >= lo && ivaCents <= hi
}

// SATVerificationURL is the QR content of the representación impresa
// (Anexo 20): id = UUID, re/rr = emisor/receptor RFC, tt = total with 10
// integer and 6 decimal digits, fe = last 8 characters of the sello.
func SATVerificationURL(uuid, emisorRFC, receptorRFC string, totalCents int64, sello string) string {
	fe := sello
	if len(fe) > 8 {
		fe = fe[len(fe)-8:]
	}
	return fmt.Sprintf("https://verificacfdi.facturaelectronica.sat.gob.mx/default.aspx?id=%s&re=%s&rr=%s&tt=%010d.%02d0000&fe=%s",
		uuid, escapeQRParam(emisorRFC), escapeQRParam(receptorRFC), totalCents/100, totalCents%100, escapeQRParam(fe))
}

// escapeQRParam escapes only the characters that would break the query
// (the SAT examples keep '/' and '=' of the sello as is).
func escapeQRParam(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '&':
			out = append(out, "%26"...)
		case '+':
			out = append(out, "%2B"...)
		case '#':
			out = append(out, "%23"...)
		default:
			out = append(out, c)
		}
	}
	return string(out)
}
