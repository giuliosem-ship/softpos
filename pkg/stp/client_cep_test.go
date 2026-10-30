package stp_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"softpos/pkg/stp"
)

func TestHTTPSTPClient_RegistraOrden_SuccessAndRejection(t *testing.T) {
	signer := stp.NewSTPSigner(nil, nil, "stp_hmac_secret_2026")

	var capturedBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /speiws/rest/ordenPago/registra", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"resultado":{"id":987654,"descripcionError":""}}`))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := stp.NewHTTPSTPClient(srv.URL, signer, 2*time.Second)

	order := stp.STPOrderRequest{
		ClaveRastreo:       "SPEI202610200001",
		AmountCents:        150075, // $1,500.75 MXN
		NombreBeneficiario: "Taqueria El Paisa",
		CuentaBeneficiario: "032180000118359719",
		ConceptoPago:       "Liquidacion Ventas",
	}

	resp, err := client.RegisterOrder(context.Background(), order)
	if err != nil {
		t.Fatalf("RegisterOrder failed: %v", err)
	}
	if resp.STPOrderID != 987654 {
		t.Errorf("STPOrderID = %d; want 987654", resp.STPOrderID)
	}
	if resp.Signature == "" {
		t.Error("expected non-empty cryptographic signature on order")
	}
	if capturedBody["firma"] != resp.Signature {
		t.Errorf("wire firma %v != resp.Signature %s", capturedBody["firma"], resp.Signature)
	}
}

func TestCEPReconciler_SuccessAndDiscrepancyDetection(t *testing.T) {
	mockProvider := stp.NewMockCEPProvider()
	reconciler := stp.NewCEPReconciler(mockProvider)

	validCLABE := "032180000118359719"

	// 1. Track normal disbursement and reconcile successfully
	reconciler.TrackDisbursement(stp.DisbursementRecord{
		ClaveRastreo:     "SPEI_CEP_OK_01",
		STPOrderID:       700001,
		MerchantID:       "merch_01",
		AmountCents:      50000,
		DestinationCLABE: validCLABE,
		BeneficiaryName:  "Juan Perez",
		Concept:          "Retiro",
	})

	rec, err := reconciler.ReconcileByClaveRastreo(context.Background(), "SPEI_CEP_OK_01")
	if err != nil {
		t.Fatalf("ReconcileByClaveRastreo failed: %v", err)
	}
	if rec.Status != stp.StatusReconciledCEP {
		t.Errorf("Status = %s; want RECONCILED_CEP", rec.Status)
	}
	if rec.CEP == nil || rec.CEP.SHA256Hash == "" || rec.CEP.SelloDigitalBanxico == "" {
		t.Errorf("expected populated CEP receipt with SHA256Hash and SelloDigitalBanxico")
	}

	// 2. Track disbursement where Banxico CEP reports a different amount -> must flag CEP_DISCREPANCY
	reconciler.TrackDisbursement(stp.DisbursementRecord{
		ClaveRastreo:     "SPEI_CEP_MISMATCH_02",
		STPOrderID:       700002,
		MerchantID:       "merch_01",
		AmountCents:      50000,
		DestinationCLABE: validCLABE,
	})

	tamperedXML := `<SPEI_Tercero FechaOperacion="2026-10-20" Hora="12:05:00" ClaveSPEI="1" claveRastreo="SPEI_CEP_MISMATCH_02" numeroCertificado="00001000000500009999" cadenaCDA="||test||" sello="sello_valido_banxico"><Ordenante BancoEmisor="90646" Nombre="SOFTPOS MX" Cuenta="646180000000000007" RFC="SPM260101AAA"/><Beneficiario BancoReceptor="40032" Nombre="JUAN" TipoCuenta="40" Cuenta="032180000118359719" RFC="XAXX010101000" Concepto="RETIRO" IVA="0.00" MontoPago="400.00"/></SPEI_Tercero>`
	mockProvider.SetCustomCEPXML("SPEI_CEP_MISMATCH_02", []byte(tamperedXML))

	badRec, err := reconciler.ReconcileByClaveRastreo(context.Background(), "SPEI_CEP_MISMATCH_02")
	if !errors.Is(err, stp.ErrCEPAmountMismatch) {
		t.Fatalf("expected ErrCEPAmountMismatch, got %v", err)
	}
	if badRec == nil || badRec.Status != stp.StatusCEPDiscrepancy {
		t.Errorf("expected status CEP_DISCREPANCY, got %+v", badRec)
	}
}
