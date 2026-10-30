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
