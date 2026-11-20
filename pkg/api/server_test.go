package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"softpos/pkg/identity"
	"softpos/pkg/ledger"
	"softpos/pkg/pac"
	"softpos/pkg/payments"
	"softpos/pkg/rbf"
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
		ID:                      12345,
		FechaOperacion:          20260917,
		InstitucionOrdenante:    "40012",
		InstitucionBeneficiaria: "646",
		ClaveRastreo:            "SPEI_TEST_001",
		Monto:                   "500.00", // $500.00 MXN -> 50000 cents
		NombreOrdenante:         "Cliente Comprador",
		CuentaOrdenante:         "012180001234567890",
		RfcCurpOrdenante:        "XAXX010101000",
		NombreBeneficiario:      "Tienda Oxxo",
		CuentaBeneficiario:      virtualCLABE,
		ConceptoPago:            "Pago de compra",
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
		Monto:              "100.00",
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

	// Settle a $1,160.00 MXN sale (116000 cents) under a 2.90% merchant tariff:
	// platform fee = 116000 * 290 / 10000 = 3364 cents (server-derived).
	// Buyer is Persona Moral -> Art. 113-J applies:
	// Base = 100000, IVA = 16000, PM withholds 1.25% ISR (1250 cents)
	// Total tax escrow = 16000 (IVA)
	// Net available = 116000 - (3364 + 16000) = 96636 cents
	authorizeTestPayment(t, server, merchantID, "pay_softpos_001", 116000)
	if err := server.merchantRepo.SetMerchantPlatformFeeBps(context.Background(), merchantID, 290); err != nil {
		t.Fatal(err)
	}
	// Client-sent fee / RBF amounts are ignored (no longer part of the contract).
	payload := map[string]any{
		"payment_id":         "pay_softpos_001",
		"merchant_id":        merchantID,
		"customer_rfc":       "B2B850101XYZ",
		"is_persona_moral":   true,
		"platform_fee_cents": 0,
		"rbf_sweep_cents":    50000,
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/payments/settle", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.Router().ServeHTTP(rec, asTenant(t, server, merchantID, req))
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
	if resp.PlatformFeeCents != 3364 || resp.PlatformFeeBps != 290 || resp.RBFSweepCents != 0 {
		t.Errorf("fee/rbf must be server-derived, got fee=%d bps=%d rbf=%d", resp.PlatformFeeCents, resp.PlatformFeeBps, resp.RBFSweepCents)
	}
	if resp.NetAvailableCents != 96636 {
		t.Errorf("expected net available 96636, got %d", resp.NetAvailableCents)
	}

	// Verify ledger
	availBal, _ := server.ledger.GetBalance("LIABILITY:MERCHANT:" + merchantID + ":AVAILABLE")
	if availBal.AmountCents != 96636 {
		t.Errorf("expected ledger available balance 96636, got %d", availBal.AmountCents)
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
	if err := server.SeedVerifiedSettlementAccount(context.Background(), merchantID, "032180000118359719"); err != nil {
		t.Fatal(err)
	}

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

	req := withPayoutKey(httptest.NewRequest(http.MethodPost, "/api/v1/disbursements", bytes.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.Router().ServeHTTP(rec, asTenant(t, server, merchantID, req))
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
	if err := server.SeedVerifiedSettlementAccount(context.Background(), merchantID, "032180000118359719"); err != nil {
		t.Fatal(err)
	}

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

	req := withPayoutKey(httptest.NewRequest(http.MethodPost, "/api/v1/disbursements", bytes.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.Router().ServeHTTP(rec, asTenant(t, server, merchantID, req))
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
	if err := server.SeedVerifiedSettlementAccount(context.Background(), merchantID, "032180000118359719"); err != nil {
		t.Fatal(err)
	}

	payload := DisbursementRequest{
		MerchantID:       merchantID,
		AmountCents:      10000,
		DestinationCLABE: "032180000118359719",
		BeneficiaryName:  "Juan",
		Concept:          "Retiro",
	}
	body, _ := json.Marshal(payload)

	req := withPayoutKey(httptest.NewRequest(http.MethodPost, "/api/v1/disbursements", bytes.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.Router().ServeHTTP(rec, asTenant(t, server, merchantID, req))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected HTTP 400 for insufficient funds, got %d", rec.Code)
	}
}

func TestDisbursement_InvalidCLABEFails(t *testing.T) {
	server, _ := setupTestServer(t)

	merchantID := "merch_clabe_bad"
	server.RegisterMerchant(merchantID, "GARM850101ABC", "Bad CLABE Merchant", true)
	if err := server.SeedVerifiedSettlementAccount(context.Background(), merchantID, "032180000118359719"); err != nil {
		t.Fatal(err)
	}

	payload := DisbursementRequest{
		MerchantID:       merchantID,
		AmountCents:      10000,
		DestinationCLABE: "032180000118359710", // not the registered settlement CLABE
		BeneficiaryName:  "Juan",
		Concept:          "Retiro",
	}
	body, _ := json.Marshal(payload)

	req := withPayoutKey(httptest.NewRequest(http.MethodPost, "/api/v1/disbursements", bytes.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.Router().ServeHTTP(rec, asTenant(t, server, merchantID, req))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "DESTINATION_CLABE_NOT_ALLOWED") {
		t.Errorf("expected 403 DESTINATION_CLABE_NOT_ALLOWED for a CLABE other than the registered one, got %d %s", rec.Code, rec.Body.String())
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

	server.Router().ServeHTTP(rec, asTenant(t, server, merchantID, req))
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

func TestSoftPOSPaymentSettle_MonthlyAccumulatedSalesTierEscalation(t *testing.T) {
	server, _ := setupTestServer(t)

	merchantID := "merch_resico_tiers"
	_ = server.RegisterMerchant(merchantID, "GARM850101ABC", "Taqueria Escalonada", true)
	authorizeTestPayment(t, server, merchantID, "pay_tier_01", 2900000)
	authorizeTestPayment(t, server, merchantID, "pay_tier_02", 1160000)

	// Sale 1: $29,000.00 MXN gross (2,900,000 cents) -> Base = 2,500,000 cents ($25,000.00 MXN) -> 1.0% ISR tier (0.010)
	sale1 := SettlePaymentRequest{
		PaymentID:        "pay_tier_01",
		MerchantID:       merchantID,
		GrossAmountCents: 2900000,
		IsPersonaMoral:   false,
	}
	body1, _ := json.Marshal(sale1)
	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/payments/settle", bytes.NewReader(body1))
	server.Router().ServeHTTP(rec1, asTenant(t, server, merchantID, req1))

	if rec1.Code != http.StatusOK {
		t.Fatalf("expected 200 on sale 1, got %d: %s", rec1.Code, rec1.Body.String())
	}
	var resp1 SettlePaymentResponse
	_ = json.Unmarshal(rec1.Body.Bytes(), &resp1)
	if resp1.ISRRateBps != 100 {
		t.Errorf("expected ISR rate 100 bps on first sale, got %d", resp1.ISRRateBps)
	}

	// Sale 2 in the same month: $11,600.00 MXN gross (1,160,000 cents) -> Base = 1,000,000 cents
	// Accumulated monthly base becomes 2,500,000 + 1,000,000 = 3,500,000 cents ($35,000.00 MXN) -> escalates to 1.1% ISR tier (0.011)!
	sale2 := SettlePaymentRequest{
		PaymentID:        "pay_tier_02",
		MerchantID:       merchantID,
		GrossAmountCents: 1160000,
		IsPersonaMoral:   false,
	}
	body2, _ := json.Marshal(sale2)
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/payments/settle", bytes.NewReader(body2))
	server.Router().ServeHTTP(rec2, asTenant(t, server, merchantID, req2))

	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 on sale 2, got %d: %s", rec2.Code, rec2.Body.String())
	}
	var resp2 SettlePaymentResponse
	_ = json.Unmarshal(rec2.Body.Bytes(), &resp2)
	if resp2.ISRRateBps != 110 {
		t.Errorf("expected escalated ISR rate 110 bps on second sale, got %d", resp2.ISRRateBps)
	}
	if resp2.ISREscrowCents != 11000 {
		t.Errorf("expected ISREscrowCents 11000 (1.1%% of 1,000,000), got %d", resp2.ISREscrowCents)
	}
}

func TestMerchantIsolatedRBFBalance(t *testing.T) {
	server, _ := setupTestServer(t)

	m1 := "merch_rbf_1"
	m2 := "merch_rbf_2"
	_ = server.RegisterMerchant(m1, "GARM850101ABC", "Merchant 1", true)
	_ = server.RegisterMerchant(m2, "XAXX010101000", "Merchant 2", true)

	// Originate a $500.00 MXN (50,000 cents) RBF loan ONLY for Merchant 1
	loanEntries := []ledger.Entry{
		{AccountID: "ASSET:RBF_LOANS_RECEIVABLE:" + m1, Direction: ledger.Debit, Money: ledger.NewMoney(50000, "MXN")},
		{AccountID: "LIABILITY:MERCHANT:" + m1 + ":AVAILABLE", Direction: ledger.Credit, Money: ledger.NewMoney(50000, "MXN")},
	}
	jnl, _ := ledger.NewJournalEntry("loan_orig_m1", time.Now(), "Originate RBF Loan M1", loanEntries, nil)
	_ = server.ledger.PostTransaction(jnl)

	// Settle a payment for Merchant 1 claiming a 10,000-cent RBF sweep: the
	// client value is ignored (no active loan in the RBF engine -> sweep 0).
	authorizeTestPayment(t, server, m1, "pay_rbf_m1", 116000)
	b1, _ := json.Marshal(map[string]any{"payment_id": "pay_rbf_m1", "rbf_sweep_cents": 10000})
	recSettle := httptest.NewRecorder()
	server.Router().ServeHTTP(recSettle, asTenant(t, server, m1, httptest.NewRequest(http.MethodPost, "/api/v1/payments/settle", bytes.NewReader(b1))))
	if recSettle.Code != http.StatusOK {
		t.Fatalf("settle M1 failed: %d", recSettle.Code)
	}

	// Query Merchant 1 balance -> RBF debt unchanged at 50,000 cents
	recBal1 := httptest.NewRecorder()
	server.Router().ServeHTTP(recBal1, asTenant(t, server, m1, httptest.NewRequest(http.MethodGet, "/api/v1/merchants/"+m1+"/balances", nil)))
	var bal1 MerchantBalancesResponse
	_ = json.Unmarshal(recBal1.Body.Bytes(), &bal1)
	if bal1.RBFDebtCents != 50000 {
		t.Errorf("expected Merchant 1 RBFDebtCents 50000 (client sweep ignored), got %d", bal1.RBFDebtCents)
	}

	// Query Merchant 2 balance -> RBF debt MUST be 0 (strictly isolated from Merchant 1)
	recBal2 := httptest.NewRecorder()
	server.Router().ServeHTTP(recBal2, asTenant(t, server, m2, httptest.NewRequest(http.MethodGet, "/api/v1/merchants/"+m2+"/balances", nil)))
	var bal2 MerchantBalancesResponse
	_ = json.Unmarshal(recBal2.Body.Bytes(), &bal2)
	if bal2.RBFDebtCents != 0 {
		t.Errorf("expected Merchant 2 RBFDebtCents 0 (isolated), got %d", bal2.RBFDebtCents)
	}
}

func generateTestMerchantCSD(t *testing.T) ([]byte, []byte) {
	t.Helper()
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(privKey)
	if err != nil {
		t.Fatalf("failed to marshal PKCS8 key: %v", err)
	}
	serial, _ := new(big.Int).SetString("30001000000400002434", 10)
	tpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "COMERCIO SOFTPOS TEST"},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &tpl, &tpl, &privKey.PublicKey, privKey)
	if err != nil {
		t.Fatalf("failed to create cert: %v", err)
	}
	return privDER, certDER
}

func TestEMVAuthorizeSettleAndReverse(t *testing.T) {
	server, _ := setupTestServer(t)
	merchantID := "merch_emv_01"
	_ = server.RegisterMerchant(merchantID, "GARM850101ABC", "Cafeteria NFC", true)

	// Valid EMV TLV for $250.00 MXN (25,000 cents), Currency 0484, CID 80 (ARQC), PAN 4152313333339012, Exp 2812
	validTLVHex := "9F02060000000250005F2A0204849F2701809F2608AABBCCDDEEFF00119F360200145A0841523133333390125F24022812"

	// 1. Authorize payment 1 -> then Void it
	auth1Body, _ := json.Marshal(EMVAuthorizeRequest{
		PaymentID:  "emv_pay_void_01",
		MerchantID: merchantID,
		TLVHex:     validTLVHex,
	})
	recAuth1 := httptest.NewRecorder()
	server.Router().ServeHTTP(recAuth1, asTenant(t, server, merchantID, httptest.NewRequest(http.MethodPost, "/api/v1/payments/emv/authorize", bytes.NewReader(auth1Body))))
	if recAuth1.Code != http.StatusOK {
		t.Fatalf("expected 200 on EMV authorize, got %d: %s", recAuth1.Code, recAuth1.Body.String())
	}
	var auth1Resp EMVAuthorizeResponse
	_ = json.Unmarshal(recAuth1.Body.Bytes(), &auth1Resp)
	if auth1Resp.MaskedPAN != "415231******9012" || auth1Resp.AmountCents != 25000 {
		t.Errorf("unexpected auth response: %+v", auth1Resp)
	}

	rev1Body, _ := json.Marshal(EMVReverseRequest{PaymentID: "emv_pay_void_01"})
	recRev1 := httptest.NewRecorder()
	server.Router().ServeHTTP(recRev1, asTenant(t, server, merchantID, httptest.NewRequest(http.MethodPost, "/api/v1/payments/emv/reverse", bytes.NewReader(rev1Body))))
	if recRev1.Code != http.StatusOK {
		t.Fatalf("expected 200 on EMV void, got %d: %s", recRev1.Code, recRev1.Body.String())
	}
	var rev1Resp EMVReverseResponse
	_ = json.Unmarshal(recRev1.Body.Bytes(), &rev1Resp)
	if rev1Resp.Status != "VOIDED" {
		t.Errorf("expected VOIDED, got %s", rev1Resp.Status)
	}

	// 2. Authorize payment 2 -> Settle it -> Reverse (Refund) it
	auth2Body, _ := json.Marshal(EMVAuthorizeRequest{
		PaymentID:  "emv_pay_settle_02",
		MerchantID: merchantID,
		TLVHex:     validTLVHex,
	})
	recAuth2 := httptest.NewRecorder()
	server.Router().ServeHTTP(recAuth2, asTenant(t, server, merchantID, httptest.NewRequest(http.MethodPost, "/api/v1/payments/emv/authorize", bytes.NewReader(auth2Body))))
	if recAuth2.Code != http.StatusOK {
		t.Fatalf("expected 200 on EMV authorize 2, got %d", recAuth2.Code)
	}

	settleBody, _ := json.Marshal(SettlePaymentRequest{
		PaymentID:  "emv_pay_settle_02",
		MerchantID: merchantID,
	})
	recSettle := httptest.NewRecorder()
	server.Router().ServeHTTP(recSettle, asTenant(t, server, merchantID, httptest.NewRequest(http.MethodPost, "/api/v1/payments/settle", bytes.NewReader(settleBody))))
	if recSettle.Code != http.StatusOK {
		t.Fatalf("expected 200 on settle, got %d: %s", recSettle.Code, recSettle.Body.String())
	}

	rev2Body, _ := json.Marshal(EMVReverseRequest{PaymentID: "emv_pay_settle_02", RefundCents: 25000})
	recRev2 := httptest.NewRecorder()
	server.Router().ServeHTTP(recRev2, asTenant(t, server, merchantID, httptest.NewRequest(http.MethodPost, "/api/v1/payments/emv/reverse", bytes.NewReader(rev2Body))))
	if recRev2.Code != http.StatusOK {
		t.Fatalf("expected 200 on EMV refund, got %d: %s", recRev2.Code, recRev2.Body.String())
	}
	var rev2Resp EMVReverseResponse
	_ = json.Unmarshal(recRev2.Body.Bytes(), &rev2Resp)
	if rev2Resp.Status != "REFUNDED" {
		t.Errorf("expected REFUNDED, got %s", rev2Resp.Status)
	}
}

func TestCoDiQRAndWebhookConfirm(t *testing.T) {
	server, _ := setupTestServer(t)
	merchantID := "merch_codi_01"
	clabe := "646180123400000007"
	_ = server.RegisterMerchantCLABE(clabe, merchantID, "GARM850101ABC")

	// 1. Generate CoDi QR
	qrReq, _ := json.Marshal(CoDiCreateQRRequest{
		MerchantID:  merchantID,
		AmountCents: 11600, // $116.00 MXN
		Concept:     "Orden de tacos",
		Reference:   "CODIREF01",
	})
	recQR := httptest.NewRecorder()
	server.Router().ServeHTTP(recQR, asTenant(t, server, merchantID, httptest.NewRequest(http.MethodPost, "/api/v1/payments/codi/qr", bytes.NewReader(qrReq))))
	if recQR.Code != http.StatusOK {
		t.Fatalf("expected 200 on CoDi QR, got %d: %s", recQR.Code, recQR.Body.String())
	}
	var qrResp CoDiCreateQRResponse
	_ = json.Unmarshal(recQR.Body.Bytes(), &qrResp)
	if !strings.Contains(qrResp.QRPayload, "116.00") {
		t.Errorf("expected CoDi QR payload to contain 116.00, got %s", qrResp.QRPayload)
	}

	// 2. Receive Banxico CoDi confirmation webhook
	webhookPayload := `{
		"tracking_key": "BNXCODI2026102001",
		"seller_clabe": "646180123400000007",
		"buyer_clabe": "012180000118359719",
		"amount_cents": 11600,
		"timestamp": "2026-10-20T12:00:00Z",
		"status": "APROBADO",
		"banxico_approval_code": "AUT001"
	}`
	server.SetCoDiWebhookSecret(testCoDiWebhookSecret)
	recWH := httptest.NewRecorder()
	server.Router().ServeHTTP(recWH, signedCoDiRequest(testCoDiWebhookSecret, time.Now(), webhookPayload))
	if recWH.Code != http.StatusOK {
		t.Fatalf("expected 200 on CoDi webhook, got %d: %s", recWH.Code, recWH.Body.String())
	}
	var whResp CoDiWebhookResponse
	_ = json.Unmarshal(recWH.Body.Bytes(), &whResp)
	// $116.00 gross -> $100.00 base, $16.00 IVA + $1.00 ISR (1%) = $17.00 tax escrow (1700 cents), net available = 9900 cents
	if whResp.TaxEscrowCents != 1700 || whResp.NetAvailableCents != 9900 {
		t.Errorf("unexpected CoDi tax split: %+v", whResp)
	}
}

func TestCFDIIndividualAndFacturaGlobalStamping(t *testing.T) {
	server, _ := setupTestServer(t)
	merchantID := "merch_cfdi_01"
	_ = server.RegisterMerchant(merchantID, "GARM850101ABC", "RESTAURANTE EL SOL", true)

	privDER, certDER := generateTestMerchantCSD(t)
	if err := server.RegisterMerchantCSD(merchantID, privDER, certDER); err != nil {
		t.Fatalf("RegisterMerchantCSD failed: %v", err)
	}

	// Configure Multi-PAC where Primary fails with network timeout and Fallback succeeds
	primary := pac.NewMockPACClient(true, "NETWORK_TIMEOUT")
	fallback := pac.NewMockPACClient(false, "")
	server.SetPACClient(pac.NewMultiPACOrchestrator(primary, fallback, 200*time.Millisecond))

	// 1. Stamp Individual CFDI 4.0
	indivReq, _ := json.Marshal(StampIndividualCFDIRequest{
		MerchantID:       merchantID,
		ReceptorRFC:      "B2B850101XYZ",
		ReceptorNombre:   "EMPRESA CLIENTE SA DE CV",
		ReceptorCP:       "06600",
		ReceptorRegimen:  "601",
		UsoCFDI:          "G03",
		GrossAmountCents: 116000,
	})
	recIndiv := httptest.NewRecorder()
	server.Router().ServeHTTP(recIndiv, asTenant(t, server, merchantID, httptest.NewRequest(http.MethodPost, "/api/v1/invoices/cfdi", bytes.NewReader(indivReq))))
	if recIndiv.Code != http.StatusOK {
		t.Fatalf("expected 200 on individual CFDI stamp, got %d: %s", recIndiv.Code, recIndiv.Body.String())
	}
	var indivResp StampCFDIResponse
	_ = json.Unmarshal(recIndiv.Body.Bytes(), &indivResp)
	if len(indivResp.UUID) != 36 || !strings.Contains(indivResp.StampedXML, "tfd:TimbreFiscalDigital") {
		t.Errorf("expected valid UUID and TFD in stamped XML, got UUID=%s", indivResp.UUID)
	}

	// 2. Stamp Daily Factura Global
	globalReq, _ := json.Marshal(StampFacturaGlobalRequest{
		MerchantID:      merchantID,
		TotalGrossCents: 232000,
	})
	recGlobal := httptest.NewRecorder()
	server.Router().ServeHTTP(recGlobal, asTenant(t, server, merchantID, httptest.NewRequest(http.MethodPost, "/api/v1/invoices/global", bytes.NewReader(globalReq))))
	if recGlobal.Code != http.StatusOK {
		t.Fatalf("expected 200 on Factura Global stamp, got %d: %s", recGlobal.Code, recGlobal.Body.String())
	}
	var globalResp StampCFDIResponse
	_ = json.Unmarshal(recGlobal.Body.Bytes(), &globalResp)
	if !strings.Contains(globalResp.StampedXML, `Rfc="XAXX010101000"`) || !strings.Contains(globalResp.StampedXML, `Periodicidad="01"`) {
		t.Errorf("expected Factura Global attributes in XML")
	}
}

func TestRBFLoanLifecycleAndAutomaticSweepCap(t *testing.T) {
	server, _ := setupTestServer(t)
	merchantID := "merch_rbf_auto"
	_ = server.RegisterMerchant(merchantID, "GARM850101ABC", "Panaderia RBF", true)
	authorizeTestPayment(t, server, merchantID, "pay_auto_rbf_1", 348_000)
	authorizeTestPayment(t, server, merchantID, "pay_auto_rbf_2", 348_000)

	// 1. Originate $1,000.00 MXN (100,000 cents) loan with 10% fixed fee (1000 bps -> 10,000 cents fee, Cap = 110,000 cents)
	// and 20% holdback rate (2000 bps)
	origBody, _ := json.Marshal(OriginateRBFLoanRequest{
		MerchantID:     merchantID,
		PrincipalCents: 100_000,
		FixedFeeBps:    1000,
		HoldbackBps:    2000,
	})
	recOrig := httptest.NewRecorder()
	server.Router().ServeHTTP(recOrig, asTenant(t, server, merchantID, httptest.NewRequest(http.MethodPost, "/api/v1/rbf/loans", bytes.NewReader(origBody))))
	if recOrig.Code != http.StatusCreated {
		t.Fatalf("expected 201 on RBF origination, got %d: %s", recOrig.Code, recOrig.Body.String())
	}
	var loanResp RBFLoanResponse
	_ = json.Unmarshal(recOrig.Body.Bytes(), &loanResp)
	if loanResp.RepaymentCapCents != 110_000 || loanResp.RemainingCents != 110_000 {
		t.Fatalf("unexpected loan cap: %+v", loanResp)
	}

	// 2. Settle Sale 1 of $3,480.00 MXN (348,000 cents) with RBFSweepCents = 0 (auto-calculated at 20% = 69,600 cents)
	s1Body, _ := json.Marshal(SettlePaymentRequest{
		PaymentID:        "pay_auto_rbf_1",
		MerchantID:       merchantID,
		GrossAmountCents: 348_000,
	})
	recS1 := httptest.NewRecorder()
	server.Router().ServeHTTP(recS1, asTenant(t, server, merchantID, httptest.NewRequest(http.MethodPost, "/api/v1/payments/settle", bytes.NewReader(s1Body))))
	if recS1.Code != http.StatusOK {
		t.Fatalf("sale 1 failed: %d", recS1.Code)
	}
	var s1Resp SettlePaymentResponse
	_ = json.Unmarshal(recS1.Body.Bytes(), &s1Resp)
	if s1Resp.RBFSweepCents != 69_600 {
		t.Errorf("expected automatic 20%% sweep of 69600 cents, got %d", s1Resp.RBFSweepCents)
	}

	// 3. Settle Sale 2 of $3,480.00 MXN (348,000 cents) -> 20% would be 69,600, but remaining cap is 110,000 - 69,600 = 40,400 cents!
	s2Body, _ := json.Marshal(SettlePaymentRequest{
		PaymentID:        "pay_auto_rbf_2",
		MerchantID:       merchantID,
		GrossAmountCents: 348_000,
	})
	recS2 := httptest.NewRecorder()
	server.Router().ServeHTTP(recS2, asTenant(t, server, merchantID, httptest.NewRequest(http.MethodPost, "/api/v1/payments/settle", bytes.NewReader(s2Body))))
	if recS2.Code != http.StatusOK {
		t.Fatalf("sale 2 failed: %d", recS2.Code)
	}
	var s2Resp SettlePaymentResponse
	_ = json.Unmarshal(recS2.Body.Bytes(), &s2Resp)
	if s2Resp.RBFSweepCents != 40_400 {
		t.Errorf("expected sweep capped at remaining 40400 cents, got %d", s2Resp.RBFSweepCents)
	}

	// 4. Query RBF status & merchant balances -> loan must be PAID_OFF and RBFDebtCents in ledger must be 0!
	recLoan := httptest.NewRecorder()
	server.Router().ServeHTTP(recLoan, asTenant(t, server, merchantID, httptest.NewRequest(http.MethodGet, "/api/v1/merchants/"+merchantID+"/rbf", nil)))
	var finalLoan RBFLoanResponse
	_ = json.Unmarshal(recLoan.Body.Bytes(), &finalLoan)
	if finalLoan.Status != rbf.LoanPaidOff || finalLoan.RemainingCents != 0 {
		t.Errorf("expected loan PAID_OFF with 0 remaining, got %+v", finalLoan)
	}

	recBal := httptest.NewRecorder()
	server.Router().ServeHTTP(recBal, asTenant(t, server, merchantID, httptest.NewRequest(http.MethodGet, "/api/v1/merchants/"+merchantID+"/balances", nil)))
	var balResp MerchantBalancesResponse
	_ = json.Unmarshal(recBal.Body.Bytes(), &balResp)
	if balResp.RBFDebtCents != 0 {
		t.Errorf("expected ledger RBFDebtCents == 0 after full repayment cap reached, got %d", balResp.RBFDebtCents)
	}
}

func TestDisbursementWithHTTPSTPClientAndCEPReconciliation(t *testing.T) {
	server, signer := setupTestServer(t)
	merchantID := "merch_stp_cep"
	_ = server.RegisterMerchant(merchantID, "GARM850101ABC", "Comercio SPEI", true)
	if err := server.SeedVerifiedSettlementAccount(context.Background(), merchantID, "032180000118359719"); err != nil {
		t.Fatal(err)
	}

	// Seed $1,000.00 MXN (100,000 cents)
	entries := []ledger.Entry{
		{AccountID: "ASSET:STP_CONCENTRATOR", Direction: ledger.Debit, Money: ledger.NewMoney(100000, "MXN")},
		{AccountID: "LIABILITY:MERCHANT:" + merchantID + ":AVAILABLE", Direction: ledger.Credit, Money: ledger.NewMoney(100000, "MXN")},
	}
	jnl, _ := ledger.NewJournalEntry("seed_stp_cep", time.Now(), "seed", entries, nil)
	_ = server.ledger.PostTransaction(jnl)

	// Stand up mock STP registraOrden HTTP server
	stpMux := http.NewServeMux()
	stpMux.HandleFunc("PUT /speiws/rest/ordenPago/registra", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"resultado":{"id":888111,"descripcionError":""}}`))
	})
	stpTestSrv := httptest.NewServer(stpMux)
	defer stpTestSrv.Close()

	server.SetSTPClient(stp.NewHTTPSTPClient(stpTestSrv.URL, signer, 2*time.Second))

	// 1. Execute Disbursement
	disbBody, _ := json.Marshal(DisbursementRequest{
		MerchantID:       merchantID,
		AmountCents:      45000,
		DestinationCLABE: "032180000118359719",
		BeneficiaryName:  "Comercio SPEI",
		Concept:          "Liquidacion T+0",
	})
	recDisb := httptest.NewRecorder()
	server.Router().ServeHTTP(recDisb, asTenant(t, server, merchantID, withPayoutKey(httptest.NewRequest(http.MethodPost, "/api/v1/disbursements", bytes.NewReader(disbBody)))))
	if recDisb.Code != http.StatusOK {
		t.Fatalf("expected 200 on disbursement, got %d: %s", recDisb.Code, recDisb.Body.String())
	}
	var disbResp DisbursementResponse
	_ = json.Unmarshal(recDisb.Body.Bytes(), &disbResp)
	if disbResp.STPOrderID != 888111 || disbResp.CEPStatus != "PENDING_CEP" {
		t.Errorf("unexpected disbursement response: %+v", disbResp)
	}

	// 2. Reconcile CEP with Banxico
	recCEP := httptest.NewRecorder()
	server.Router().ServeHTTP(recCEP, asTenant(t, server, merchantID, httptest.NewRequest(http.MethodPost, "/api/v1/disbursements/"+disbResp.ClaveRastreo+"/reconcile-cep", nil)))
	if recCEP.Code != http.StatusOK {
		t.Fatalf("expected 200 on CEP reconciliation, got %d: %s", recCEP.Code, recCEP.Body.String())
	}
	var reconciledRecord stp.DisbursementRecord
	_ = json.Unmarshal(recCEP.Body.Bytes(), &reconciledRecord)
	if reconciledRecord.Status != stp.StatusReconciledCEP || reconciledRecord.CEP == nil || reconciledRecord.CEP.MontoCents != 45000 {
		t.Errorf("expected RECONCILED_CEP with 45000 cents, got %+v", reconciledRecord)
	}
}


func TestRegisterMerchantWithCLABE_KeepsProfileAndRejectsDuplicateCLABE(t *testing.T) {
	server, _ := setupTestServer(t)
	sender := withTestAuth(t, server)
	user := httpLogin(t, server, sender, "+525510000001", "")
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		server.Router().ServeHTTP(rec, withBearer(httptest.NewRequest(http.MethodPost, "/api/v1/merchants", strings.NewReader(body)), user.AccessToken))
		return rec
	}
	rec := post(`{"id":"m_clabe_1","rfc":"GARM850101ABC","name":"Fonda Real","is_resico":false,"virtual_clabe":"646180123400000007"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		MerchantID   string `json:"merchant_id"`
		VirtualCLABE string `json:"virtual_clabe"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.MerchantID == "" || created.MerchantID == "m_clabe_1" {
		t.Fatalf("server must generate the merchant id (client id ignored), got %q", created.MerchantID)
	}
	m, ok := server.GetMerchant(created.MerchantID)
	if !ok || m.Name != "Fonda Real" || m.IsRESICO {
		t.Fatalf("CLABE assignment must not overwrite the merchant profile: %+v", m)
	}
	// The collection CLABE is server-assigned (client value ignored: no squatting).
	if m.CLABE == "646180123400000007" || m.CLABE != created.VirtualCLABE || !stp.ValidateCLABE(m.CLABE) || !strings.HasPrefix(m.CLABE, "646180") {
		t.Fatalf("expected a server-assigned STP CLABE, got %q (response %q)", m.CLABE, created.VirtualCLABE)
	}
	rec2 := post(`{"rfc":"XAXX010101000","name":"Otro","is_resico":true,"virtual_clabe":"` + m.CLABE + `"}`)
	var created2 struct {
		VirtualCLABE string `json:"virtual_clabe"`
	}
	_ = json.Unmarshal(rec2.Body.Bytes(), &created2)
	if rec2.Code != http.StatusCreated || created2.VirtualCLABE == m.CLABE || !stp.ValidateCLABE(created2.VirtualCLABE) {
		t.Fatalf("second merchant must get its own CLABE even when asking for another's: %d %s", rec2.Code, rec2.Body.String())
	}
	if err := server.RegisterMerchantCSD("ghost_merchant", []byte("k"), []byte("c")); err == nil {
		t.Fatalf("expected error registering a CSD for an unknown merchant")
	}
}

type countingProcessor struct {
	payments.PaymentProcessor
	authorizes int
}

func (c *countingProcessor) Authorize(id, merchantID string, emv payments.EMVTags) (*payments.Payment, error) {
	c.authorizes++
	return c.PaymentProcessor.Authorize(id, merchantID, emv)
}

func TestSetPaymentProcessor_IsUsedByHandlers(t *testing.T) {
	server, _ := setupTestServer(t)
	proc := &countingProcessor{PaymentProcessor: payments.NewPaymentEngine()}
	server.SetPaymentProcessor(proc)
	body := `{"payment_id":"pp_1","merchant_id":"m1","tlv_hex":"9F02060000000250005F2A0204849F2701809F2608AABBCCDDEEFF00119F360200145A0841523133333390125F24022812"}`
	rec := httptest.NewRecorder()
	server.Router().ServeHTTP(rec, asTenant(t, server, "m1", httptest.NewRequest(http.MethodPost, "/api/v1/payments/emv/authorize", strings.NewReader(body))))
	if rec.Code != http.StatusOK || proc.authorizes != 1 {
		t.Fatalf("expected injected processor to authorize once, got code=%d count=%d", rec.Code, proc.authorizes)
	}
}
