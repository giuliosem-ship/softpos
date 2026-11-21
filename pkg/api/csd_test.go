package api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"softpos/pkg/auth"
	"softpos/pkg/fiscal"
	"softpos/pkg/fiscal/fiscaltest"
)

const csdTestRFC = "CACX7605101P8"

func csdUploadReq(t *testing.T, merchantID string, parts map[string][]byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, name := range []string{"cer", "key", "password"} {
		v, ok := parts[name]
		if !ok {
			continue
		}
		var w interface{ Write([]byte) (int, error) }
		var err error
		if name == "password" {
			w, err = mw.CreateFormField(name)
		} else {
			w, err = mw.CreateFormFile(name, "file."+name)
		}
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(v)
	}
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/merchants/"+merchantID+"/csd", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func newFixtureCSD(t *testing.T, o fiscaltest.CSDOptions) fiscaltest.CSD {
	t.Helper()
	c, err := fiscaltest.NewCSD(o)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestCSDUpload_ValidatesStoresAndSigns: the owner uploads .cer/.key +
// password; every SAT rule maps to 422 CSD_INVALID with a reason; the stored
// CSD (encrypted key + password in the vault) signs CFDIs.
func TestCSDUpload_ValidatesStoresAndSigns(t *testing.T) {
	s, _ := setupTestServer(t)
	if err := s.RegisterMerchant("m_csd", csdTestRFC, "Xochilt Casas Chavez", true); err != nil {
		t.Fatal(err)
	}
	good := newFixtureCSD(t, fiscaltest.CSDOptions{RFC: csdTestRFC})
	parts := func(c fiscaltest.CSD, pwd string) map[string][]byte {
		return map[string][]byte{"cer": c.CertDER, "key": c.EncryptedKey, "password": []byte(pwd)}
	}

	if rec := serve(s, asTenant(t, s, "m_csd", httptest.NewRequest(http.MethodGet, "/api/v1/merchants/m_csd/csd", nil))); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "CSD_NOT_FOUND") {
		t.Fatalf("no CSD yet: %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(s, asRole(t, s, "m_csd", auth.RoleCashier, csdUploadReq(t, "m_csd", parts(good, string(good.Password))))); rec.Code != http.StatusForbidden {
		t.Fatalf("cashier upload: %d", rec.Code)
	}
	if rec := serve(s, asTenant(t, s, "m_other", csdUploadReq(t, "m_csd", parts(good, string(good.Password))))); rec.Code != http.StatusForbidden {
		t.Fatalf("other tenant: %d", rec.Code)
	}

	other := newFixtureCSD(t, fiscaltest.CSDOptions{RFC: csdTestRFC})
	cases := []struct {
		name   string
		parts  map[string][]byte
		reason string
	}{
		{"wrong password", parts(good, "bad-password"), fiscal.CSDReasonWrongPassword},
		{"rfc mismatch", parts(newFixtureCSD(t, fiscaltest.CSDOptions{RFC: "EKU9003173C9"}), "12345678a"), fiscal.CSDReasonRFCMismatch},
		{"expired", parts(newFixtureCSD(t, fiscaltest.CSDOptions{RFC: csdTestRFC, NotBefore: time.Now().Add(-5 * 365 * 24 * time.Hour), NotAfter: time.Now().Add(-time.Hour)}), "12345678a"), fiscal.CSDReasonExpired},
		{"FIEL", parts(newFixtureCSD(t, fiscaltest.CSDOptions{RFC: csdTestRFC, FIEL: true}), "12345678a"), fiscal.CSDReasonFIEL},
		{"key/cer mismatch", map[string][]byte{"cer": good.CertDER, "key": other.EncryptedKey, "password": other.Password}, fiscal.CSDReasonKeyMismatch},
		{"missing key", map[string][]byte{"cer": good.CertDER, "password": good.Password}, "MISSING_KEY"},
	}
	for _, c := range cases {
		rec := serve(s, asTenant(t, s, "m_csd", csdUploadReq(t, "m_csd", c.parts)))
		var body map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusUnprocessableEntity || body["error"] != "CSD_INVALID" || body["reason"] != c.reason {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body.String())
		}
	}

	rec := serve(s, asTenant(t, s, "m_csd", csdUploadReq(t, "m_csd", parts(good, string(good.Password)))))
	var out CSDResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK || out.Serial != "30001000000500003416" || out.RFC != csdTestRFC || out.Status != "ACTIVE" || out.ValidTo == "" {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "12345678a") {
		t.Fatal("the password must never be echoed")
	}
	rec = serve(s, asRole(t, s, "m_csd", auth.RoleCashier, httptest.NewRequest(http.MethodGet, "/api/v1/merchants/m_csd/csd", nil)))
	var got CSDResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || got != out {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}
	// The vault holds the SAT key encrypted with its password; signing works.
	signer, err := s.merchantSigner("m_csd")
	if err != nil {
		t.Fatal(err)
	}
	comp := &fiscal.CFDIComprobante{Fecha: "2026-11-20T10:00:00", SubTotal: "1.00", Moneda: "MXN", Total: "1.00", TipoDeComprobante: "I",
		Exportacion: "01", LugarExpedicion: "06000", Emisor: fiscal.CFDIEmisor{Rfc: csdTestRFC, Nombre: "X", RegimenFiscal: "626"},
		Receptor: fiscal.PublicoEnGeneral("06000")}
	if _, err := signer.Seal(comp); err != nil || fiscal.VerifySello(comp) != nil {
		t.Fatalf("stored CSD does not sign: %v", err)
	}
}

func TestSATCatalogsEndpoint(t *testing.T) {
	s, _ := setupTestServer(t)
	if rec := serve(s, httptest.NewRequest(http.MethodGet, "/api/v1/catalogs/sat", nil)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("catalogs need authentication: %d", rec.Code)
	}
	rec := serve(s, asTenant(t, s, "m_cat", httptest.NewRequest(http.MethodGet, "/api/v1/catalogs/sat?names=regimen_fiscal,uso_cfdi", nil)))
	var out struct {
		Regimen []fiscal.RegimenFiscalEntry `json:"regimen_fiscal"`
		Uso     []fiscal.UsoCFDIEntry       `json:"uso_cfdi"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != http.StatusOK || len(out.Regimen) < 19 || len(out.Uso) < 24 {
		t.Fatalf("catalogs: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-SAT-Catalog-Version") == "" || strings.Contains(rec.Body.String(), "clave_unidad") {
		t.Fatal("version header / only the requested names")
	}
	if rec := serve(s, asTenant(t, s, "m_cat", httptest.NewRequest(http.MethodGet, "/api/v1/catalogs/sat?names=nope", nil))); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown catalog: %d", rec.Code)
	}
}

func TestFiscalSettings_DefaultsAndOwnerUpdate(t *testing.T) {
	s, _ := setupTestServer(t)
	if err := s.RegisterMerchant("m_fs", csdTestRFC, "Xochilt", true); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/merchants/m_fs/fiscal-settings"
	rec := serve(s, asTenant(t, s, "m_fs", httptest.NewRequest(http.MethodGet, path, nil)))
	var fs FiscalSettingsBody
	_ = json.Unmarshal(rec.Body.Bytes(), &fs)
	if rec.Code != http.StatusOK || fs.ClaveProdServ != "01010101" || fs.ClaveUnidad != "H87" || fs.GlobalPeriodicity != "MONTHLY" {
		t.Fatalf("defaults: %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(s, asRole(t, s, "m_fs", auth.RoleCashier, jsonReq(http.MethodPut, path, map[string]string{"clave_unidad": "E48"}))); rec.Code != http.StatusForbidden {
		t.Fatalf("cashier update: %d", rec.Code)
	}
	rec = serve(s, asTenant(t, s, "m_fs", jsonReq(http.MethodPut, path, map[string]string{"clave_prod_serv": "99999999", "clave_unidad": "ZZZ", "lugar_expedicion": "12"})))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "clave_prod_serv") || !strings.Contains(rec.Body.String(), "lugar_expedicion") {
		t.Fatalf("invalid: %d %s", rec.Code, rec.Body.String())
	}
	rec = serve(s, asTenant(t, s, "m_fs", jsonReq(http.MethodPut, path, map[string]string{"clave_prod_serv": "90101501", "clave_unidad": "e48",
		"descripcion": "Consumo de alimentos", "lugar_expedicion": "44100", "global_periodicity": "daily"})))
	_ = json.Unmarshal(rec.Body.Bytes(), &fs)
	if rec.Code != http.StatusOK || fs.ClaveProdServ != "90101501" || fs.ClaveUnidad != "E48" || fs.LugarExpedicion != "44100" || fs.GlobalPeriodicity != "DAILY" {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
}
