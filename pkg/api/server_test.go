package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"softpos/pkg/identity"
	"softpos/pkg/ledger"
	"softpos/pkg/stp"
)

func setupTestServer(t *testing.T) (*Server, *stp.STPSigner) {
	ledgerEngine := ledger.NewLedgerEngine()
	blacklistService := identity.NewBlacklistService()
	stpSigner := stp.NewSTPSigner(nil, nil, "test_hmac_secret_key")

	server := NewServer(ledgerEngine, blacklistService, stpSigner)

	// Pre-create global platform accounts in ledger
	_, _ = ledgerEngine.CreateAccount("ASSET:STP_CONCENTRATOR", ledger.AccountAsset, "STP Concentrator")
	_, _ = ledgerEngine.CreateAccount("ASSET:ACQUIRER_RECEIVABLE", ledger.AccountAsset, "Acquirer Receivable")
	_, _ = ledgerEngine.CreateAccount("REVENUE:FEES", ledger.AccountRevenue, "Platform Fee Revenue")
	_, _ = ledgerEngine.CreateAccount("ASSET:RBF_LOANS_RECEIVABLE", ledger.AccountAsset, "RBF Loans")

	return server, stpSigner
}

func TestSTPAbonoWebhook_SuccessAndIdempotency(t *testing.T) {
	server, signer := setupTestServer(t)

	merchantID := "merch_oxxo_01"
	virtualCLABE := "646180000000000014"
	server.RegisterMerchantCLABE(virtualCLABE, merchantID, "GARM850101ABC")

	// Ensure merchant accounts exist
	server.ledger.CreateAccount("LIABILITY:MERCHANT:"+merchantID+":AVAILABLE", ledger.AccountLiability, "Merchant Available")
	server.ledger.CreateAccount("LIABILITY:MERCHANT:"+merchantID+":TAX_ESCROW", ledger.AccountLiability, "Merchant Tax Escrow")

	payload := STPAbonoRequest{
		ID:                    12345,
		FechaOperacion:        20260917,
		InstitucionOrdenante:  "40012",
		InstitucionBeneficiaria: "646",
		ClaveRastreo:          "SPEI_TEST_001",
		Monto:                 500.00, // $500.00 MXN -> 50000 cents
		NombreOrdenante:       "Cliente Comprador",
		CuentaOrdenante:       "012180001234567890",
		RfcCurpOrdenante:      "XAXX010101000",
		NombreBeneficiario:    "Tienda Oxxo",
		CuentaBeneficiario:    virtualCLABE,
		ConceptoPago:          "Pago de compra",
	}

	canonical := signer.BuildAbonoCanonicalString(
		payload.CuentaBeneficiario,
		50000,
		payload.ClaveRastreo,
		payload.InstitucionOrdenante,
		payload.CuentaOrdenante,
	)
	payload.Firma = signer.SignHMAC(canonical)

	body, _ := json.Marshal(payload)

	// 1. Send first webhook
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/stp/abono", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	start := time.Now()
	server.Router().ServeHTTP(rec, req)
	duration := time.Since(start)

	if duration > 300*time.Millisecond {
		t.Errorf("latency exceeded 300ms SLA: took %v", duration)
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d. Body: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["mensaje"] != "confirmar" {
		t.Errorf("expected mensaje 'confirmar', got %v", resp["mensaje"])
	}

	// Verify ledger updated
	bal, err := server.ledger.GetBalance("LIABILITY:MERCHANT:" + merchantID + ":AVAILABLE")
	if err != nil {
		t.Fatalf("failed to get balance: %v", err)
	}
	if bal.AmountCents != 50000 {
		t.Errorf("expected merchant available 50000 cents, got %d", bal.AmountCents)
	}

	// 2. Send duplicate webhook with identical ClaveRastreo (Idempotency test)
	recDup := httptest.NewRecorder()
	reqDup := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/stp/abono", bytes.NewReader(body))
	reqDup.Header.Set("Content-Type", "application/json")

	server.Router().ServeHTTP(recDup, reqDup)
	if recDup.Code != http.StatusOK {
		t.Fatalf("expected status 200 on duplicate, got %d", recDup.Code)
	}

	// Balance MUST NOT double credit
	balAfter, _ := server.ledger.GetBalance("LIABILITY:MERCHANT:" + merchantID + ":AVAILABLE")
	if balAfter.AmountCents != 50000 {
		t.Errorf("double-spending detected! Expected 50000 cents, got %d", balAfter.AmountCents)
	}
}

func TestSTPAbonoWebhook_InvalidSignatureFails(t *testing.T) {
	server, _ := setupTestServer(t)

	payload := STPAbonoRequest{
		ID:                 12346,
		ClaveRastreo:       "SPEI_TEST_002",
		Monto:              100.00,
		CuentaBeneficiario: "646180000000000014",
		Firma:              "invalid_forged_signature",
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/stp/abono", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected HTTP 401 Unauthorized for forged signature, got %d", rec.Code)
	}
}

func TestSoftPOSPaymentSettle_SuccessWithRESICO(t *testing.T) {
	server, _ := setupTestServer(t)

	merchantID := "merch_taqueria_01"
	server.RegisterMerchant(merchantID, "GARM850101ABC", "Taqueria Los Primos", true)

	// Settle a $1,160.00 MXN sale (116000 cents)
	// Platform fee: $33.60 (3360 cents)
	// Buyer is Persona Moral -> Art. 113-J applies:
	// Base = 100000, IVA = 16000, PM withholds 1.25% ISR (1250 cents)
	// Total tax escrow = 16000 (IVA)
	// Net available = 116000 - (3360 + 16000) = 96640 cents
	payload := SettlePaymentRequest{
		PaymentID:        "pay_softpos_001",
		MerchantID:       merchantID,
		GrossAmountCents: 116000,
		CustomerRFC:      "B2B850101XYZ",
		IsPersonaMoral:   true,
		PlatformFeeCents: 3360,
		RBFSweepCents:    0,
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/payments/settle", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d. Body: %s", rec.Code, rec.Body.String())
	}

	var resp SettlePaymentResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	if resp.BaseAmountCents != 100000 {
		t.Errorf("expected base 100000, got %d", resp.BaseAmountCents)
	}
	if resp.IVAAmountCents != 16000 {
		t.Errorf("expected IVA 16000, got %d", resp.IVAAmountCents)
	}
	if resp.TaxEscrowCents != 16000 {
		t.Errorf("expected TaxEscrow 16000, got %d", resp.TaxEscrowCents)
	}
	if resp.NetAvailableCents != 96640 {
		t.Errorf("expected net available 96640, got %d", resp.NetAvailableCents)
	}

	// Verify ledger
	availBal, _ := server.ledger.GetBalance("LIABILITY:MERCHANT:" + merchantID + ":AVAILABLE")
	if availBal.AmountCents != 96640 {
		t.Errorf("expected ledger available balance 96640, got %d", availBal.AmountCents)
	}

	escrowBal, _ := server.ledger.GetBalance("LIABILITY:MERCHANT:" + merchantID + ":TAX_ESCROW")
	if escrowBal.AmountCents != 16000 {
		t.Errorf("expected ledger escrow balance 16000, got %d", escrowBal.AmountCents)
	}
}

func TestDisbursement_Success(t *testing.T) {
	server, _ := setupTestServer(t)

	merchantID := "merch_taqueria_02"
	server.RegisterMerchant(merchantID, "GARM850101ABC", "Taqueria 2", true)

	// Seed available funds in merchant account ($2,000.00 MXN)
	entries := []ledger.Entry{
		{AccountID: "ASSET:STP_CONCENTRATOR", Direction: ledger.Debit, Money: ledger.NewMoney(200000, "MXN")},
		{AccountID: "LIABILITY:MERCHANT:" + merchantID + ":AVAILABLE", Direction: ledger.Credit, Money: ledger.NewMoney(200000, "MXN")},
	}
	jnl, _ := ledger.NewJournalEntry("seed_funds", time.Now(), "seed", entries, nil)
	_ = server.ledger.PostTransaction(jnl)

	// Request disbursement of $500.00 (50000 cents) to valid CLABE
	validCLABE := "032180000118359719"
	payload := DisbursementRequest{
		MerchantID:       merchantID,
		AmountCents:      50000,
		DestinationCLABE: validCLABE,
		BeneficiaryName:  "Juan Perez Garcia",
		Concept:          "Retiro Diario SoftPOS",
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/disbursements", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d. Body: %s", rec.Code, rec.Body.String())
	}

	var resp DisbursementResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	if resp.Status != "DISPATCHED" {
		t.Errorf("expected status DISPATCHED, got %s", resp.Status)
	}
	if resp.RemainingBalanceCents != 150000 {
		t.Errorf("expected remaining balance 150000, got %d", resp.RemainingBalanceCents)
	}

	// Verify ledger updated
	availBal, _ := server.ledger.GetBalance("LIABILITY:MERCHANT:" + merchantID + ":AVAILABLE")
	if availBal.AmountCents != 150000 {
		t.Errorf("expected ledger balance 150000, got %d", availBal.AmountCents)
	}
}

func TestDisbursement_SAT69BBlacklistBlocked(t *testing.T) {
	server, _ := setupTestServer(t)

	merchantID := "merch_blacklisted"
	blacklistedRFC := "EFO110101XYZ"
	server.RegisterMerchant(merchantID, blacklistedRFC, "Simulated Company", false)

	// Mark in blacklist as DEFINITIVO
	server.blacklist.AddEntry(blacklistedRFC, identity.BlacklistDefinitivo, "500-05-2023-1234")

	payload := DisbursementRequest{
		MerchantID:       merchantID,
		AmountCents:      10000,
		DestinationCLABE: "032180000118359719",
		BeneficiaryName:  "Fraudster",
		Concept:          "Pagar",
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/disbursements", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected HTTP 403 Forbidden for blacklisted merchant, got %d", rec.Code)
	}

	var errResp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &errResp)
	if errResp["error"] != "SAT_69B_SANCTION_HOLD" {
		t.Errorf("expected SAT_69B_SANCTION_HOLD, got %v", errResp["error"])
	}
}

func TestDisbursement_InsufficientFundsFails(t *testing.T) {
	server, _ := setupTestServer(t)

	merchantID := "merch_broke"
	server.RegisterMerchant(merchantID, "GARM850101ABC", "Broke Merchant", true)

	payload := DisbursementRequest{
		MerchantID:       merchantID,
		AmountCents:      10000,
		DestinationCLABE: "032180000118359719",
		BeneficiaryName:  "Juan",
		Concept:          "Retiro",
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/disbursements", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected HTTP 400 for insufficient funds, got %d", rec.Code)
	}
}

func TestDisbursement_InvalidCLABEFails(t *testing.T) {
	server, _ := setupTestServer(t)

	merchantID := "merch_clabe_bad"
	server.RegisterMerchant(merchantID, "GARM850101ABC", "Bad CLABE Merchant", true)

	payload := DisbursementRequest{
		MerchantID:       merchantID,
		AmountCents:      10000,
		DestinationCLABE: "032180000118359710", // Bad check digit (valid is 9)
		BeneficiaryName:  "Juan",
		Concept:          "Retiro",
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/disbursements", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected HTTP 400 for invalid CLABE, got %d", rec.Code)
	}
}

func TestGetMerchantBalances(t *testing.T) {
	server, _ := setupTestServer(t)

	merchantID := "merch_query_bal"
	server.RegisterMerchant(merchantID, "GARM850101ABC", "Query Merchant", true)

	// Seed 7500 cents available and 2500 cents escrow
	entries := []ledger.Entry{
		{AccountID: "ASSET:STP_CONCENTRATOR", Direction: ledger.Debit, Money: ledger.NewMoney(10000, "MXN")},
		{AccountID: "LIABILITY:MERCHANT:" + merchantID + ":AVAILABLE", Direction: ledger.Credit, Money: ledger.NewMoney(7500, "MXN")},
		{AccountID: "LIABILITY:MERCHANT:" + merchantID + ":TAX_ESCROW", Direction: ledger.Credit, Money: ledger.NewMoney(2500, "MXN")},
	}
	jnl, _ := ledger.NewJournalEntry("seed_bal", time.Now(), "seed", entries, nil)
	_ = server.ledger.PostTransaction(jnl)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/merchants/"+merchantID+"/balances", nil)
	rec := httptest.NewRecorder()

	server.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var resp MerchantBalancesResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	if resp.AvailableCents != 7500 {
		t.Errorf("expected available 7500, got %d", resp.AvailableCents)
	}
	if resp.TaxEscrowCents != 2500 {
		t.Errorf("expected escrow 2500, got %d", resp.TaxEscrowCents)
	}
	if resp.RBFDebtCents != 0 {
		t.Errorf("expected RBF debt 0, got %d", resp.RBFDebtCents)
	}
}
