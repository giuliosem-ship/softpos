package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"softpos/pkg/fiscal"
	"softpos/pkg/pac"
)

type StampIndividualCFDIRequest struct {
	MerchantID       string `json:"merchant_id"`
	ReceptorRFC      string `json:"receptor_rfc"`
	ReceptorNombre   string `json:"receptor_nombre"`
	ReceptorCP       string `json:"receptor_cp"`
	ReceptorRegimen  string `json:"receptor_regimen"`
	UsoCFDI          string `json:"uso_cfdi"`
	GrossAmountCents int64  `json:"gross_amount_cents"`
	FormaPago        string `json:"forma_pago"`
	MetodoPago       string `json:"metodo_pago"`
	LugarExpedicion  string `json:"lugar_expedicion"`
	ClaveProdServ    string `json:"clave_prod_serv"`
	Descripcion      string `json:"descripcion"`
}

type StampCFDIResponse struct {
	UUID             string `json:"uuid"`
	FechaTimbrado    string `json:"fecha_timbrado"`
	RfcProvCertif    string `json:"rfc_prov_certif"`
	NoCertificadoSAT string `json:"no_certificado_sat"`
	SelloCFD         string `json:"sello_cfd"`
	SelloSAT         string `json:"sello_sat"`
	TotalGrossCents  int64  `json:"total_gross_cents"`
	BaseCents        int64  `json:"base_cents"`
	IVACents         int64  `json:"iva_cents"`
	StampedXML       string `json:"stamped_xml"`
}

func formatCents(cents int64) string {
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}

func (s *Server) HandleStampIndividualCFDI(w http.ResponseWriter, r *http.Request) {
	var req StampIndividualCFDIRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"INVALID_JSON"}`, http.StatusBadRequest)
		return
	}
	if req.MerchantID == "" || req.ReceptorRFC == "" || req.GrossAmountCents <= 0 {
		http.Error(w, `{"error":"MISSING_REQUIRED_FIELDS"}`, http.StatusBadRequest)
		return
	}

	merchant, exists := s.GetMerchant(req.MerchantID)
	if !exists {
		http.Error(w, `{"error":"MERCHANT_NOT_FOUND"}`, http.StatusNotFound)
		return
	}

	csd, err := s.getMerchantCSD(req.MerchantID)
	if err != nil {
		http.Error(w, `{"error":"MERCHANT_CSD_NOT_CONFIGURED"}`, http.StatusBadRequest)
		return
	}

	taxSplit, err := fiscal.CalculateResicoTaxSplit(req.GrossAmountCents, s.GetMerchantMonthlySalesCents(req.MerchantID), false)
	if err != nil {
		http.Error(w, `{"error":"TAX_SPLIT_ERROR"}`, http.StatusBadRequest)
		return
	}

	formaPago := req.FormaPago
	if formaPago == "" {
		formaPago = "04"
	}
	metodoPago := req.MetodoPago
	if metodoPago == "" {
		metodoPago = "PUE"
	}
	lugarExp := req.LugarExpedicion
	if lugarExp == "" {
		lugarExp = "06000"
	}
	usoCFDI := req.UsoCFDI
	if usoCFDI == "" {
		usoCFDI = "G03"
	}
	receptorCP := req.ReceptorCP
	if receptorCP == "" {
		receptorCP = lugarExp
	}
	receptorRegimen := req.ReceptorRegimen
	if receptorRegimen == "" {
		receptorRegimen = "601"
	}
	claveProd := req.ClaveProdServ
	if claveProd == "" {
		claveProd = "01010101"
	}
	desc := req.Descripcion
	if desc == "" {
		desc = "Venta en punto de venta SoftPOS"
	}

	baseStr := formatCents(taxSplit.BaseAmountCents)
	ivaStr := formatCents(taxSplit.IVAAmountCents)
	totalStr := formatCents(req.GrossAmountCents)
	now := time.Now().UTC()

	comp := &fiscal.CFDIComprobante{
		Version:           "4.0",
		Serie:             "POS",
		Folio:             fmt.Sprintf("%d", now.UnixNano()%1_000_000),
		Fecha:             now.Format("2006-01-02T15:04:05"),
		FormaPago:         formaPago,
		SubTotal:          baseStr,
		Moneda:            "MXN",
		Total:             totalStr,
		TipoDeComprobante: "I",
		Exportacion:       "01",
		MetodoPago:        metodoPago,
		LugarExpedicion:   lugarExp,
		Emisor: fiscal.CFDIEmisor{
			Rfc:           merchant.RFC,
			Nombre:        merchant.Name,
			RegimenFiscal: "626",
		},
		Receptor: fiscal.CFDIReceptor{
			Rfc:                     req.ReceptorRFC,
			Nombre:                  req.ReceptorNombre,
			DomicilioFiscalReceptor: receptorCP,
			RegimenFiscalReceptor:   receptorRegimen,
			UsoCFDI:                 usoCFDI,
		},
		Conceptos: []fiscal.CFDIConcepto{
			{
				ClaveProdServ: claveProd,
				Cantidad:      "1",
				ClaveUnidad:   "H87",
				Descripcion:   desc,
				ValorUnitario: baseStr,
				Importe:       baseStr,
				ObjetoImp:     "02",
			},
		},
		Impuestos: fiscal.CFDIImpuestos{
			TotalImpuestosTrasladados: ivaStr,
			Traslados: []fiscal.CFDITraslado{
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

	unsignedXML, err := fiscal.BuildCFDI40XML(comp)
	if err != nil {
		http.Error(w, `{"error":"CFDI_BUILD_FAILED"}`, http.StatusBadRequest)
		return
	}

	signedXML, err := fiscal.SignCFDI(unsignedXML, csd.KeyBytes, csd.CERBytes)
	if err != nil {
		http.Error(w, `{"error":"CFDI_SIGNING_FAILED"}`, http.StatusInternalServerError)
		return
	}

	s.stampAndRespond(w, r, signedXML, req.GrossAmountCents, taxSplit.BaseAmountCents, taxSplit.IVAAmountCents)
}

type StampFacturaGlobalRequest struct {
	MerchantID      string `json:"merchant_id"`
	TotalGrossCents int64  `json:"total_gross_cents"`
	BaseCents       int64  `json:"base_cents,omitempty"`
	IVACents        int64  `json:"iva_cents,omitempty"`
}

func (s *Server) HandleStampFacturaGlobal(w http.ResponseWriter, r *http.Request) {
	var req StampFacturaGlobalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"INVALID_JSON"}`, http.StatusBadRequest)
		return
	}
	if req.MerchantID == "" || req.TotalGrossCents <= 0 {
		http.Error(w, `{"error":"MISSING_REQUIRED_FIELDS"}`, http.StatusBadRequest)
		return
	}

	merchant, exists := s.GetMerchant(req.MerchantID)
	if !exists {
		http.Error(w, `{"error":"MERCHANT_NOT_FOUND"}`, http.StatusNotFound)
		return
	}

	csd, err := s.getMerchantCSD(req.MerchantID)
	if err != nil {
		http.Error(w, `{"error":"MERCHANT_CSD_NOT_CONFIGURED"}`, http.StatusBadRequest)
		return
	}

	baseCents := req.BaseCents
	ivaCents := req.IVACents
	if baseCents == 0 && ivaCents == 0 {
		split, sErr := fiscal.CalculateResicoTaxSplit(req.TotalGrossCents, s.GetMerchantMonthlySalesCents(req.MerchantID), false)
		if sErr != nil {
			http.Error(w, `{"error":"TAX_SPLIT_ERROR"}`, http.StatusBadRequest)
			return
		}
		baseCents = split.BaseAmountCents
		ivaCents = split.IVAAmountCents
	}

	emisor := fiscal.CFDIEmisor{
		Rfc:           merchant.RFC,
		Nombre:        merchant.Name,
		RegimenFiscal: "626",
	}

	signedXML, err := fiscal.BuildFacturaGlobalDiaria(
		emisor,
		req.TotalGrossCents,
		baseCents,
		ivaCents,
		csd.KeyBytes,
		csd.CERBytes,
	)
	if err != nil {
		http.Error(w, `{"error":"FACTURA_GLOBAL_BUILD_FAILED"}`, http.StatusBadRequest)
		return
	}

	s.stampAndRespond(w, r, signedXML, req.TotalGrossCents, baseCents, ivaCents)
}

func (s *Server) stampAndRespond(
	w http.ResponseWriter,
	r *http.Request,
	signedXML []byte,
	totalGrossCents int64,
	baseCents int64,
	ivaCents int64,
) {
	s.mu.RLock()
	pacClient := s.pacOrchestrator
	s.mu.RUnlock()

	pacResp, err := pacClient.Stamp(r.Context(), signedXML)
	if err != nil {
		if errors.Is(err, pac.ErrSATValidationError) {
			errCode := "SAT_VALIDATION_ERROR"
			if pacResp != nil && pacResp.ErrorCode != "" {
				errCode = pacResp.ErrorCode
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": errCode})
			return
		}
		http.Error(w, `{"error":"PAC_UNAVAILABLE"}`, http.StatusBadGateway)
		return
	}

	stampedXML, err := pac.InjectTimbreFiscal(signedXML, pacResp.Timbre)
	if err != nil {
		http.Error(w, `{"error":"TFD_INJECTION_FAILED"}`, http.StatusInternalServerError)
		return
	}

	resp := StampCFDIResponse{
		UUID:             pacResp.Timbre.UUID,
		FechaTimbrado:    pacResp.Timbre.FechaTimbrado.UTC().Format(time.RFC3339),
		RfcProvCertif:    pacResp.Timbre.RfcProvCertif,
		NoCertificadoSAT: pacResp.Timbre.NoCertificadoSAT,
		SelloCFD:         pacResp.Timbre.SelloCFD,
		SelloSAT:         pacResp.Timbre.SelloSAT,
		TotalGrossCents:  totalGrossCents,
		BaseCents:        baseCents,
		IVACents:         ivaCents,
		StampedXML:       string(stampedXML),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
