package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"softpos/pkg/api"
	"softpos/pkg/auth"
	"softpos/pkg/store/pgtest"
	"softpos/pkg/stp"
)

// TestPGIntegration_SalesEndpointPaginates: card intents and a signed SPEI
// abono listed over HTTP from PostgreSQL, newest first, across cursor pages.
func TestPGIntegration_SalesEndpointPaginates(t *testing.T) {
	cfg := Config{SQLDriver: "pgx", DatabaseURL: pgtest.URL(t), STPHMACSecret: "test_secret", JWTSecret: testJWTSecret,
		VaultMasterKeyHex: testVaultKeyHex, AcquirerProvider: "mock", AcquirerWebhookSecret: "sales_pg_acquirer_webhook_secret_32!"}
	srv, db, _, err := BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	const m, clabe = "merch_pg_sales", "646180123400000007"
	if err := srv.RegisterMerchantCLABE(clabe, m, "GARM850101ABC"); err != nil {
		t.Fatal(err)
	}
	owner, _ := srv.AuthService().MintAccessToken("usr_"+m, m, auth.RoleOwner)
	for _, key := range []string{"s1", "s2", "s3"} {
		b, _ := json.Marshal(map[string]any{"amount_cents": 1500})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/card-payments/intents", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+owner)
		req.Header.Set("Idempotency-Key", key)
		rec := httptest.NewRecorder()
		srv.Router().ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("intent: %d %s", rec.Code, rec.Body.String())
		}
	}
	signer := stp.NewSTPSigner(nil, nil, cfg.STPHMACSecret)
	canonical := signer.BuildAbonoCanonicalString(clabe, 25000, "SPEIPGSALES1", "40012", "012180001234567890")
	if rec := doJSON(t, srv, "", http.MethodPost, "/api/v1/webhooks/stp/abono", map[string]any{
		"id": 9, "claveRastreo": "SPEIPGSALES1", "monto": json.Number("250.00"), "institucionOrdenante": "40012",
		"cuentaOrdenante": "012180001234567890", "cuentaBeneficiario": clabe, "firma": signer.SignHMAC(canonical),
	}); rec.Code != http.StatusOK {
		t.Fatalf("abono: %d %s", rec.Code, rec.Body.String())
	}
	var ids []string
	kinds := map[string]int{}
	cursor := ""
	for i := 0; i < 5; i++ {
		path := "/api/v1/merchants/" + m + "/sales?limit=3"
		if cursor != "" {
			path += "&before=" + url.QueryEscape(cursor)
		}
		rec := doJSON(t, srv, owner, http.MethodGet, path, nil)
		var page api.SalesPage
		_ = json.Unmarshal(rec.Body.Bytes(), &page)
		if rec.Code != http.StatusOK {
			t.Fatalf("sales: %d %s", rec.Code, rec.Body.String())
		}
		for _, it := range page.Items {
			ids = append(ids, it.ID)
			kinds[it.Kind]++
		}
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}
	if len(ids) != 4 || kinds["card"] != 3 || kinds["spei"] != 1 || ids[0] != "spei_40012:SPEIPGSALES1" {
		t.Fatalf("sales over HTTP: %v %v", ids, kinds)
	}
}
