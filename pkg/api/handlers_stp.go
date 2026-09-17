package api

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"

	"softpos/pkg/ledger"
)

type STPAbonoRequest struct {
	ID                    int64   `json:"id"`
	FechaOperacion        int     `json:"fechaOperacion"`
	InstitucionOrdenante  string  `json:"institucionOrdenante"`
	InstitucionBeneficiaria string `json:"institucionBeneficiaria"`
	ClaveRastreo          string  `json:"claveRastreo"`
	Monto                 float64 `json:"monto"`
	NombreOrdenante       string  `json:"nombreOrdenante"`
	CuentaOrdenante       string  `json:"cuentaOrdenante"`
	RfcCurpOrdenante      string  `json:"rfcCurpOrdenante"`
	NombreBeneficiario    string  `json:"nombreBeneficiario"`
	CuentaBeneficiario    string  `json:"cuentaBeneficiario"`
	ConceptoPago          string  `json:"conceptoPago"`
	Firma                 string  `json:"firma"`
}

type STPAbonoResponse struct {
	ID      int64  `json:"id"`
	Mensaje string `json:"mensaje"`
}

func (s *Server) HandleSTPAbono(w http.ResponseWriter, r *http.Request) {
	var req STPAbonoRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"INVALID_JSON"}`, http.StatusBadRequest)
		return
	}

	montoCents := int64(math.Round(req.Monto * 100.0))
	canonical := s.stpSigner.BuildAbonoCanonicalString(
		req.CuentaBeneficiario,
		montoCents,
		req.ClaveRastreo,
		req.InstitucionOrdenante,
		req.CuentaOrdenante,
	)

	// Verify signature (supports RSA or HMAC)
	validRSA := s.stpSigner.VerifyRSASignature(canonical, req.Firma)
	validHMAC := s.stpSigner.VerifyHMAC(canonical, req.Firma)
	if !validRSA && !validHMAC {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "INVALID_SIGNATURE",
		})
		return
	}

	// Idempotency check on claveRastreo
	if rec, exists := s.idempotency.Get("stp:abono:" + req.ClaveRastreo); exists {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rec.StatusCode)
		_, _ = w.Write(rec.Body)
		return
	}

	merchant, exists := s.GetMerchantByCLABE(req.CuentaBeneficiario)
	if !exists {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "UNKNOWN_VIRTUAL_CLABE",
		})
		return
	}

	// Double-entry posting: Debit STP Concentrator, Credit Merchant Available
	availableAccountID := fmt.Sprintf("LIABILITY:MERCHANT:%s:AVAILABLE", merchant.ID)
	entries := []ledger.Entry{
		{AccountID: "ASSET:STP_CONCENTRATOR", Direction: ledger.Debit, Money: ledger.NewMoney(montoCents, "MXN")},
		{AccountID: availableAccountID, Direction: ledger.Credit, Money: ledger.NewMoney(montoCents, "MXN")},
	}

	journalID := fmt.Sprintf("spei_in_%s_%d", req.ClaveRastreo, req.ID)
	journal, err := ledger.NewJournalEntry(journalID, time.Now(), fmt.Sprintf("SPEI Inbound Abono %s", req.ClaveRastreo), entries, nil)
	if err != nil {
		http.Error(w, `{"error":"LEDGER_JOURNAL_ERROR"}`, http.StatusInternalServerError)
		return
	}

	if err := s.ledger.PostTransaction(journal); err != nil {
		http.Error(w, `{"error":"LEDGER_POSTING_ERROR"}`, http.StatusInternalServerError)
		return
	}

	resp := STPAbonoResponse{
		ID:      req.ID,
		Mensaje: "confirmar",
	}
	respBytes, _ := json.Marshal(resp)

	// Cache idempotency for 24 hours
	s.idempotency.Set("stp:abono:"+req.ClaveRastreo, http.StatusOK, respBytes, 24*time.Hour)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(respBytes)
}
