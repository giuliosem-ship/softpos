package pac_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"softpos/pkg/pac"
)

func TestMockPACClient_StampSuccess(t *testing.T) {
	client := pac.NewMockPACClient(false, "")

	signedXML := []byte(`<cfdi:Comprobante xmlns:cfdi="http://www.sat.gob.mx/cfd/4" Version="4.0"></cfdi:Comprobante>`)

	resp, err := client.Stamp(context.Background(), signedXML)
	if err != nil {
		t.Fatalf("Stamp failed: %v", err)
	}
	if !resp.Success {
		t.Errorf("expected success, got error: %s", resp.ErrorMessage)
	}
	if resp.Timbre == nil {
		t.Fatal("expected TimbreFiscalDigital, got nil")
	}
	if len(resp.Timbre.UUID) != 36 {
		t.Errorf("invalid UUID length: %s", resp.Timbre.UUID)
	}
	if resp.Timbre.Version != "1.1" {
		t.Errorf("expected TFD version 1.1, got %s", resp.Timbre.Version)
	}
}

func TestMultiPACOrchestrator_FailoverOnNetworkError(t *testing.T) {
	// Primary PAC fails with network outage
	primary := pac.NewMockPACClient(true, "NETWORK_TIMEOUT")
	// Secondary PAC is operational
	secondary := pac.NewMockPACClient(false, "")

	orchestrator := pac.NewMultiPACOrchestrator(primary, secondary, 100*time.Millisecond)

	signedXML := []byte(`<cfdi:Comprobante Version="4.0"></cfdi:Comprobante>`)
	resp, err := orchestrator.Stamp(context.Background(), signedXML)
	if err != nil {
		t.Fatalf("orchestrator Stamp failed: %v", err)
	}

	if !resp.Success {
		t.Errorf("failover should have succeeded, got failure")
	}
	if resp.Timbre == nil {
		t.Errorf("expected valid timbre from secondary PAC")
	}
}

func TestMultiPACOrchestrator_NoFailoverOnSATRejection(t *testing.T) {
	// Primary PAC rejects document due to SAT compliance (e.g. invalid RFC 301)
	primary := pac.NewMockPACClient(false, "SAT_ERROR_301")
	secondary := pac.NewMockPACClient(false, "")

	orchestrator := pac.NewMultiPACOrchestrator(primary, secondary, 100*time.Millisecond)

	signedXML := []byte(`<cfdi:Comprobante Version="4.0"></cfdi:Comprobante>`)
	_, err := orchestrator.Stamp(context.Background(), signedXML)

	// Business rule: SAT business rejection must NOT failover to secondary PAC
	if !errors.Is(err, pac.ErrSATValidationError) {
		t.Errorf("expected ErrSATValidationError without failover, got %v", err)
	}
}

func TestInjectTimbreFiscal(t *testing.T) {
	signedXML := []byte(`<cfdi:Comprobante xmlns:cfdi="http://www.sat.gob.mx/cfd/4" Version="4.0"><cfdi:Emisor Rfc="TEST"/></cfdi:Comprobante>`)

	timbre := &pac.TimbreFiscalDigital{
		Version:          "1.1",
		UUID:             "4a24f0c7-0f81-4277-bc63-123456789abc",
		FechaTimbrado:    time.Now(),
		RfcProvCertif:    "SW080808000",
		SelloCFD:         "sello_cfd_base64",
		NoCertificadoSAT: "30001000000400002434",
		SelloSAT:         "sello_sat_base64",
	}

	stampedXML, err := pac.InjectTimbreFiscal(signedXML, timbre)
	if err != nil {
		t.Fatalf("InjectTimbreFiscal failed: %v", err)
	}

	outStr := string(stampedXML)
	if !strings.Contains(outStr, "cfdi:Complemento") {
		t.Errorf("missing cfdi:Complemento tag")
	}
	if !strings.Contains(outStr, "UUID=\"4a24f0c7-0f81-4277-bc63-123456789abc\"") {
		t.Errorf("missing or incorrect UUID attribute in stamped XML")
	}
	if !strings.Contains(outStr, "http://www.sat.gob.mx/TimbreFiscalDigital") {
		t.Errorf("missing TimbreFiscalDigital namespace")
	}
}

func stampMock(t *testing.T, c pac.PACClient, total, receptor string) string {
	t.Helper()
	x := `<cfdi:Comprobante xmlns:cfdi="http://www.sat.gob.mx/cfd/4" Version="4.0" Sello="SELLO" Total="` + total + `"><cfdi:Receptor Rfc="` + receptor + `"/></cfdi:Comprobante>`
	resp, err := c.Stamp(context.Background(), []byte(x))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Timbre.SelloCFD != "SELLO" {
		t.Fatalf("SelloCFD must be the comprobante sello, got %q", resp.Timbre.SelloCFD)
	}
	return resp.Timbre.UUID
}

func TestMockPAC_CancelAndStatusFollowSATRules(t *testing.T) {
	ctx := context.Background()
	sat := pac.NewMockSAT()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	sat.SetClock(func() time.Time { return now })
	primary := pac.NewMockPACClientWithSAT(false, "", sat)
	orch := pac.NewMultiPACOrchestrator(pac.NewMockPACClientWithSAT(true, "", sat), primary, 100*time.Millisecond)
	csd := pac.CancelRequest{CertDER: []byte{1}, KeyPKCS8: []byte{1}, Motivo: "02"}

	small := stampMock(t, primary, "999.99", "EKU9003173C9")
	req := csd
	req.UUID = small
	if resp, err := orch.Cancel(ctx, req); err != nil || resp.State != pac.CancelCanceled {
		t.Fatalf("<= $1,000 cancels without acceptance: %+v %v", resp, err)
	}
	if st, _ := orch.Status(ctx, pac.StatusRequest{UUID: small}); st.Estado != pac.EstadoCancelado {
		t.Fatalf("status: %+v", st)
	}

	big := stampMock(t, primary, "5000.00", "EKU9003173C9")
	req.UUID = big
	if resp, err := orch.Cancel(ctx, req); err != nil || resp.State != pac.CancelInProcess {
		t.Fatalf("> $1,000 needs acceptance: %+v %v", resp, err)
	}
	if !sat.ResolveCancellation(big, false) {
		t.Fatal("resolve")
	}
	if st, _ := orch.Status(ctx, pac.StatusRequest{UUID: big}); st.Estado != pac.EstadoVigente || st.EstatusCancelacion != pac.CancelacionRechazada {
		t.Fatalf("rejected: %+v", st)
	}

	expiring := stampMock(t, primary, "5000.00", "EKU9003173C9")
	req.UUID = expiring
	_, _ = orch.Cancel(ctx, req)
	now = now.Add(73 * time.Hour)
	if st, _ := orch.Status(ctx, pac.StatusRequest{UUID: expiring}); st.Estado != pac.EstadoCancelado || st.EstatusCancelacion != pac.CancelacionPlazoVencido {
		t.Fatalf("plazo vencido: %+v", st)
	}
	global := stampMock(t, primary, "9000.00", "XAXX010101000")
	req.UUID = global
	if resp, _ := orch.Cancel(ctx, req); resp.State != pac.CancelCanceled {
		t.Fatalf("general public cancels without acceptance: %+v", resp)
	}
	req.UUID = "00000000-0000-4000-8000-000000000000"
	if _, err := orch.Cancel(ctx, req); !errors.Is(err, pac.ErrSATValidationError) {
		t.Fatalf("unknown uuid: %v", err)
	}
	if got := pac.TFDCadenaOriginal(&pac.TimbreFiscalDigital{UUID: "U", FechaTimbrado: now, RfcProvCertif: "SPR190613I52", SelloCFD: "S", NoCertificadoSAT: "N"}); !strings.HasPrefix(got, "||1.1|U|") || !strings.HasSuffix(got, "|S|N||") {
		t.Fatalf("tfd cadena: %s", got)
	}
}
