package main

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"softpos/pkg/auth"
	"softpos/pkg/fiscal/fiscaltest"
	"softpos/pkg/store/pgtest"
)

func csdMultipart(t *testing.T, c fiscaltest.CSD) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("cer", "csd.cer")
	_, _ = fw.Write(c.CertDER)
	fw, _ = mw.CreateFormFile("key", "csd.key")
	_, _ = fw.Write(c.EncryptedKey)
	_ = mw.WriteField("password", string(c.Password))
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

// TestPGIntegration_CSDUploadAndFiscalSettingsSurviveRestart: a CSD uploaded
// over HTTP and the fiscal settings persist in PostgreSQL across a reboot.
func TestPGIntegration_CSDUploadAndFiscalSettingsSurviveRestart(t *testing.T) {
	cfg := Config{SQLDriver: "pgx", DatabaseURL: pgtest.URL(t), STPHMACSecret: "test_secret", JWTSecret: testJWTSecret, VaultMasterKeyHex: testVaultKeyHex}
	srv, db, _, err := BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	const m, rfc = "merch_pg_cfdi", "CACX7605101P8"
	if err := srv.RegisterMerchant(m, rfc, "Xochilt Casas Chavez", true); err != nil {
		t.Fatal(err)
	}
	owner, _ := srv.AuthService().MintAccessToken("usr_"+m, m, auth.RoleOwner)
	c, err := fiscaltest.NewCSD(fiscaltest.CSDOptions{RFC: rfc})
	if err != nil {
		t.Fatal(err)
	}
	body, ct := csdMultipart(t, c)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/merchants/"+m+"/csd", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer "+owner)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	b, _ := json.Marshal(map[string]string{"clave_prod_serv": "90101501", "lugar_expedicion": "44100"})
	req = httptest.NewRequest(http.MethodPut, "/api/v1/merchants/"+m+"/fiscal-settings", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+owner)
	rec = httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("settings: %d %s", rec.Code, rec.Body.String())
	}
	_ = db.Close()

	srv2, db2, _, err := BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	owner2, _ := srv2.AuthService().MintAccessToken("usr_"+m, m, auth.RoleOwner)
	for path, want := range map[string]string{"/csd": `"serial":"30001000000500003416"`, "/fiscal-settings": `"lugar_expedicion":"44100"`} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/merchants/"+m+path, nil)
		req.Header.Set("Authorization", "Bearer "+owner2)
		rec := httptest.NewRecorder()
		srv2.Router().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(want)) {
			t.Fatalf("GET %s after restart: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	var n int
	if err := db2.QueryRow(`SELECT COUNT(*) FROM merchants WHERE id = $1 AND encrypted_csd_json::text NOT LIKE '%12345678a%'`, m).Scan(&n); err != nil || n != 1 {
		t.Fatalf("envelope must not hold the clear password: n=%d %v", n, err)
	}
}
