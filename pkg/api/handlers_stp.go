package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"softpos/pkg/ledger"
)

type STPAbonoRequest struct {
	ID                      int64       `json:"id"`
	FechaOperacion          int         `json:"fechaOperacion"`
	InstitucionOrdenante    string      `json:"institucionOrdenante"`
	InstitucionBeneficiaria string      `json:"institucionBeneficiaria"`
	ClaveRastreo            string      `json:"claveRastreo"`
	Monto                   json.Number `json:"monto"`
	NombreOrdenante         string      `json:"nombreOrdenante"`
	CuentaOrdenante         string      `json:"cuentaOrdenante"`
	RfcCurpOrdenante        string      `json:"rfcCurpOrdenante"`
	NombreBeneficiario      string      `json:"nombreBeneficiario"`
	CuentaBeneficiario      string      `json:"cuentaBeneficiario"`
	ConceptoPago            string      `json:"conceptoPago"`
	Firma                   string      `json:"firma"`
}

type STPAbonoResponse struct {
	ID      int64  `json:"id"`
	Mensaje string `json:"mensaje"`
}

// parseSTPMontoToCents converts a JSON decimal token (e.g. "500.00" or "125.5") directly into integer cents without float64.
func parseSTPMontoToCents(num json.Number) (int64, error) {
	raw := strings.TrimSpace(num.String())
	if raw == "" {
		return 0, errors.New("empty monto")
	}

	parts := strings.Split(raw, ".")
	if len(parts) > 2 {
		return 0, errors.New("invalid decimal format")
	}

	pesos, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || pesos < 0 {
		return 0, errors.New("invalid integer part")
	}

	var centavos int64
	if len(parts) == 2 {
		frac := parts[1]
		if len(frac) == 1 {
			frac += "0"
		} else if len(frac) == 0 {
			frac = "00"
		} else if len(frac) > 2 {
			return 0, errors.New("more than 2 decimal places not allowed")
		}
		centavos, err = strconv.ParseInt(frac, 10, 64)
		if err != nil || centavos < 0 {
			return 0, errors.New("invalid fractional part")
		}
	}

	totalCents := pesos*100 + centavos
	if totalCents <= 0 {
		return 0, errors.New("monto must be positive")
	}
	return totalCents, nil
}

func (s *Server) HandleSTPAbono(w http.ResponseWriter, r *http.Request) {
	var req STPAbonoRequest
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, `{"error":"INVALID_JSON"}`, http.StatusBadRequest)
		return
	}

	montoCents, err := parseSTPMontoToCents(req.Monto)
	if err != nil {
		http.Error(w, `{"error":"INVALID_AMOUNT"}`, http.StatusBadRequest)
		return
	}

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
