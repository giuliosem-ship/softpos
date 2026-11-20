package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"softpos/pkg/ledger"
	"softpos/pkg/store"
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

	if strings.TrimSpace(req.ClaveRastreo) == "" {
		writeError(w, http.StatusBadRequest, "MISSING_CLAVE_RASTREO")
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

	merchant, exists := s.GetMerchantByCLABE(req.CuentaBeneficiario)
	if !exists {
		writeError(w, http.StatusNotFound, "UNKNOWN_VIRTUAL_CLABE")
		return
	}

	// Exactly-once on (institucionOrdenante, claveRastreo): a clave de rastreo
	// is only unique per sending institution, and both fields are covered by
	// the signature (fechaOperacion is not, so it cannot be part of the key
	// without letting a replay with another date credit twice). The key
	// claim, the journal and the outbox event commit in one transaction; a
	// duplicate replays the first response.
	ctx := r.Context()
	abonoRef := strings.TrimSpace(req.InstitucionOrdenante) + ":" + req.ClaveRastreo
	opKey := "stp:abono:" + abonoRef
	var respBytes []byte
	err = s.moneyStore().InTx(ctx, func(tx store.MoneyTx) error {
		claimed, _, prior, err := tx.ClaimOperation(ctx, opKey)
		if err != nil {
			return err
		}
		if !claimed {
			respBytes = prior
			return nil
		}
		// Double-entry posting: Debit STP Concentrator, Credit Merchant Available
		availableAccountID := fmt.Sprintf("LIABILITY:MERCHANT:%s:AVAILABLE", merchant.ID)
		journal, err := ledger.NewJournalEntry("spei_in_"+abonoRef, nowUTC(), "SPEI Inbound Abono "+req.ClaveRastreo, []ledger.Entry{
			{AccountID: "ASSET:STP_CONCENTRATOR", Direction: ledger.Debit, Money: ledger.NewMoney(montoCents, "MXN")},
			{AccountID: availableAccountID, Direction: ledger.Credit, Money: ledger.NewMoney(montoCents, "MXN")},
		}, map[string]string{"clave_rastreo": req.ClaveRastreo, "institucion_ordenante": req.InstitucionOrdenante, "stp_id": strconv.FormatInt(req.ID, 10)})
		if err != nil {
			return err
		}
		if err := tx.PostJournal(ctx, journal); err != nil {
			return err
		}
		if err := tx.RecordSale(ctx, store.SaleRecord{ID: "spei_" + abonoRef, MerchantID: merchant.ID, Kind: store.SaleSPEI,
			Status: "RECEIVED", AmountCents: montoCents, Reference: req.ClaveRastreo, CreatedAt: journal.Timestamp}); err != nil {
			return err
		}
		if err := tx.EnqueueEvent(ctx, "spei.abono_received", req.ClaveRastreo, map[string]any{
			"clave_rastreo": req.ClaveRastreo,
			"merchant_id":   merchant.ID,
			"amount_cents":  montoCents,
			"stp_id":        req.ID,
		}); err != nil {
			return err
		}
		if respBytes, err = json.Marshal(STPAbonoResponse{ID: req.ID, Mensaje: "confirmar"}); err != nil {
			return err
		}
		return tx.CompleteOperation(ctx, opKey, http.StatusOK, respBytes)
	})
	if err != nil {
		writeMoneyError(w, "stp_abono", err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(respBytes)
}
