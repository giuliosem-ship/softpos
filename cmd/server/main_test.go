package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"softpos/pkg/api"
	"softpos/pkg/auth"
	"softpos/pkg/identity"
	_ "softpos/pkg/store" // register mockpostgres driver
	"softpos/pkg/stp"
)

func TestLoadConfigFromEnv(t *testing.T) {
	t.Setenv("PORT", "9090")
	t.Setenv("DATABASE_URL", "postgres://test:test@localhost:5432/testdb")
	t.Setenv("REDIS_ADDR", "localhost:6379")
	t.Setenv("STP_HMAC_SECRET", "custom_secret_key")

	cfg := LoadConfigFromEnv()
	if cfg.Port != 9090 {
		t.Errorf("expected port 9090, got %d", cfg.Port)
	}
	if cfg.DatabaseURL != "postgres://test:test@localhost:5432/testdb" {
		t.Errorf("unexpected database URL: %s", cfg.DatabaseURL)
	}
	if cfg.RedisAddr != "localhost:6379" {
		t.Errorf("unexpected redis addr: %s", cfg.RedisAddr)
	}
	if cfg.STPHMACSecret != "custom_secret_key" {
		t.Errorf("unexpected hmac secret: %s", cfg.STPHMACSecret)
	}
}

func TestBuildServer_InMemoryAndMockDB(t *testing.T) {
	ctx := context.Background()

	// 1. In-memory build
	cfgMem := Config{
		Port:             8080,
		SeedDemoMerchant: true,
		STPHMACSecret:    "test_secret",
		JWTSecret:        testJWTSecret, VaultMasterKeyHex: testVaultKeyHex,
	}
	srvMem, dbMem, rdMem, err := BuildServer(ctx, cfgMem)
	if err != nil {
		t.Fatalf("BuildServer in-memory failed: %v", err)
	}
	if dbMem != nil || rdMem != nil {
		t.Errorf("expected nil DB and Redis for in-memory config")
	}

	// Verify /healthz
	recHealth := httptest.NewRecorder()
	srvMem.Router().ServeHTTP(recHealth, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recHealth.Code != http.StatusOK {
		t.Errorf("expected 200 on /healthz, got %d", recHealth.Code)
	}

	// Verify demo merchant seeded
	m, ok := srvMem.GetMerchant("merch_demo_01")
	if !ok || m.RFC != "TAQ850101XYZ" {
		t.Errorf("expected demo merchant seeded, got %+v", m)
	}

	// 2. Build with mock SQL database driver
	tmpDir := t.TempDir()
	cfgSQL := Config{
		Port:             8081,
		SQLDriver:        "mockpostgres",
		DatabaseURL:      "main_test_db",
		MigrationsDir:    tmpDir,
		SeedDemoMerchant: true,
		STPHMACSecret:    "test_secret",
		JWTSecret:        testJWTSecret, VaultMasterKeyHex: testVaultKeyHex,
	}
	srvSQL, dbSQL, _, err := BuildServer(ctx, cfgSQL)
	if err != nil {
		t.Fatalf("BuildServer with mock DB failed: %v", err)
	}
	defer func() {
		if dbSQL != nil {
			_ = dbSQL.Close()
		}
	}()

	recGet := httptest.NewRecorder()
	srvSQL.Router().ServeHTTP(recGet, asMerchant(t, srvSQL, "merch_demo_01", httptest.NewRequest(http.MethodGet, "/api/v1/merchants/merch_demo_01", nil)))
	if recGet.Code != http.StatusOK {
		t.Fatalf("expected 200 on get demo merchant, got %d", recGet.Code)
	}

	var merchResp map[string]any
	_ = json.Unmarshal(recGet.Body.Bytes(), &merchResp)
	if merchResp["id"] != "merch_demo_01" {
		t.Errorf("expected merch_demo_01, got %v", merchResp["id"])
	}
}

func TestServerLifecycle_GracefulShutdown(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		Port:             0, // ephemeral
		SeedDemoMerchant: true,
		STPHMACSecret:    "test_secret",
		JWTSecret:        testJWTSecret, VaultMasterKeyHex: testVaultKeyHex,
	}

	srv, _, _, err := BuildServer(ctx, cfg)
	if err != nil {
		t.Fatalf("BuildServer failed: %v", err)
	}

	testServer := httptest.NewServer(srv.Router())
	defer testServer.Close()

	res, err := http.Get(testServer.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz failed: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", res.StatusCode)
	}
}

const testJWTSecret = "cmd_server_test_jwt_secret_32_bytes_min"

// testVaultKeyHex is a throwaway 32-byte CSD vault master key for tests.
const testVaultKeyHex = "7e1f3c5a9b2d4e6f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f7"

func TestBuildServer_RequiresJWTSecretUnlessDevMode(t *testing.T) {
	if _, _, _, err := BuildServer(context.Background(), Config{STPHMACSecret: "test_secret"}); !errors.Is(err, ErrMissingJWTSecret) {
		t.Fatalf("expected ErrMissingJWTSecret, got %v", err)
	}
	if _, _, _, err := BuildServer(context.Background(), Config{STPHMACSecret: "test_secret", DevMode: true}); err != nil {
		t.Fatalf("DEV_MODE without JWT_SECRET must boot with an ephemeral secret, got %v", err)
	}
	if _, _, _, err := BuildServer(context.Background(), Config{STPHMACSecret: "test_secret", JWTSecret: "too_short"}); err == nil {
		t.Fatal("short JWT secret must be rejected")
	}
}

func TestLoadConfigFromEnv_AuthSettings(t *testing.T) {
	t.Setenv("DEV_MODE", "true")
	t.Setenv("JWT_SECRET", testJWTSecret)
	t.Setenv("AUTH_ACCESS_TTL", "5m")
	t.Setenv("AUTH_REFRESH_TTL", "bogus")
	cfg := LoadConfigFromEnv()
	if !cfg.DevMode || cfg.JWTSecret != testJWTSecret || cfg.AuthAccessTTL != 5*time.Minute || cfg.AuthRefreshTTL != 0 {
		t.Fatalf("unexpected auth config: %+v", cfg)
	}
}

// asMerchant signs req with an owner access token for merchantID.
func asMerchant(t *testing.T, srv *api.Server, merchantID string, req *http.Request) *http.Request {
	t.Helper()
	tok, err := srv.AuthService().MintAccessToken("usr_cmd_test", merchantID, auth.RoleOwner)
	if err != nil {
		t.Fatalf("MintAccessToken: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return req
}

func TestBuildServer_MountsAuthAndDevSession(t *testing.T) {
	srv, _, _, err := BuildServer(context.Background(), Config{STPHMACSecret: "test_secret", JWTSecret: testJWTSecret, VaultMasterKeyHex: testVaultKeyHex, SeedDemoMerchant: true})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/merchants/merch_demo_01/balances", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("balances without token: want 401, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/dev-session", strings.NewReader(`{"merchant_id":"merch_demo_01"}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("dev-session without DEV_MODE: want 404, got %d", rec.Code)
	}

	dev, _, _, err := BuildServer(context.Background(), Config{STPHMACSecret: "test_secret", DevMode: true, SeedDemoMerchant: true})
	if err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	dev.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/dev-session", strings.NewReader(`{"merchant_id":"merch_demo_01"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("dev-session in DEV_MODE: want 200, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestLoadConfigFromEnv_AttestationAndRateLimit(t *testing.T) {
	t.Setenv("AUTH_RATE_LIMIT_PER_MINUTE", "7")
	t.Setenv("PLAY_INTEGRITY_SECRET", "pi_secret")
	cfg := LoadConfigFromEnv()
	if cfg.AuthRateLimitPerMinute != 7 || cfg.PlayIntegritySecret != "pi_secret" || cfg.PlayIntegrityPackage != "mx.softpos.app" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestBuildServer_STPDevSecretOnlyInDevMode(t *testing.T) {
	// The public dev secret is refused without DEV_MODE...
	if _, _, _, err := BuildServer(context.Background(), Config{STPHMACSecret: defaultSTPHMACSecret, JWTSecret: testJWTSecret, VaultMasterKeyHex: testVaultKeyHex}); !errors.Is(err, ErrDevSTPSecretOutsideDevMode) {
		t.Fatalf("expected ErrDevSTPSecretOutsideDevMode, got %v", err)
	}
	abono := func(srv *api.Server, secret string) int {
		signer := stp.NewSTPSigner(nil, nil, secret)
		canonical := signer.BuildAbonoCanonicalString("646180123400000007", 10000, "STPDEV0001", "40012", "012180000118359719")
		body := `{"cuentaBeneficiario":"646180123400000007","monto":100.00,"claveRastreo":"STPDEV0001","institucionOrdenante":"40012","cuentaOrdenante":"012180000118359719","firma":"` + signer.SignHMAC(canonical) + `"}`
		rec := httptest.NewRecorder()
		srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/stp/abono", strings.NewReader(body)))
		return rec.Code
	}
	// ...without any STP secret (and no DEV_MODE) HMAC abonos signed with the dev secret are rejected...
	noSecret, _, _, err := BuildServer(context.Background(), Config{JWTSecret: testJWTSecret, VaultMasterKeyHex: testVaultKeyHex, SeedDemoMerchant: true})
	if err != nil {
		t.Fatal(err)
	}
	if code := abono(noSecret, defaultSTPHMACSecret); code != http.StatusUnauthorized {
		t.Fatalf("dev-signed abono without DEV_MODE: want 401, got %d", code)
	}
	// ...and only DEV_MODE falls back to it.
	dev, _, _, err := BuildServer(context.Background(), Config{DevMode: true, SeedDemoMerchant: true})
	if err != nil {
		t.Fatal(err)
	}
	if code := abono(dev, defaultSTPHMACSecret); code == http.StatusUnauthorized {
		t.Fatalf("dev-signed abono in DEV_MODE must pass signature check, got %d", code)
	}
}

func TestBuildServer_CoDiWebhookSecret(t *testing.T) {
	body := `{"tracking_key":"BNXCMD0001","seller_clabe":"646180123400000007","buyer_clabe":"012180000118359719","amount_cents":11600,"timestamp":"2026-11-20T12:00:00Z","status":"APROBADO","banxico_approval_code":"A1"}`
	post := func(srv *api.Server, secret string) int {
		ts := time.Now().Unix()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/codi/confirm", strings.NewReader(body))
		req.Header.Set(api.WebhookTimestampHeader, strconv.FormatInt(ts, 10))
		req.Header.Set(api.WebhookSignatureHeader, api.SignWebhook([]byte(secret), ts, []byte(body)))
		rec := httptest.NewRecorder()
		srv.Router().ServeHTTP(rec, req)
		return rec.Code
	}
	off, _, _, err := BuildServer(context.Background(), Config{STPHMACSecret: "test_secret", JWTSecret: testJWTSecret, VaultMasterKeyHex: testVaultKeyHex, SeedDemoMerchant: true})
	if err != nil {
		t.Fatal(err)
	}
	if code := post(off, "anything"); code != http.StatusServiceUnavailable {
		t.Fatalf("no CODI_WEBHOOK_SECRET: want 503, got %d", code)
	}
	const secret = "cmd_codi_webhook_secret_32_bytes_long!"
	on, _, _, err := BuildServer(context.Background(), Config{STPHMACSecret: "test_secret", JWTSecret: testJWTSecret, VaultMasterKeyHex: testVaultKeyHex, SeedDemoMerchant: true, CoDiWebhookSecret: secret})
	if err != nil {
		t.Fatal(err)
	}
	if code := post(on, "wrong_secret"); code != http.StatusUnauthorized {
		t.Fatalf("wrong secret: want 401, got %d", code)
	}
	if code := post(on, secret); code != http.StatusOK {
		t.Fatalf("valid signature: want 200, got %d", code)
	}
}

func TestBuildServer_VaultKeyFromEnvOnly(t *testing.T) {
	base := Config{STPHMACSecret: "test_secret", JWTSecret: testJWTSecret}
	if _, _, _, err := BuildServer(context.Background(), base); !errors.Is(err, ErrMissingVaultKey) {
		t.Fatalf("no VAULT_MASTER_KEY_HEX outside DEV_MODE: want ErrMissingVaultKey, got %v", err)
	}
	devKey := base
	devKey.VaultMasterKeyHex = defaultVaultMasterKeyHex
	if _, _, _, err := BuildServer(context.Background(), devKey); !errors.Is(err, ErrDevVaultKeyOutsideDevMode) {
		t.Fatalf("public dev key outside DEV_MODE: want ErrDevVaultKeyOutsideDevMode, got %v", err)
	}
	short := base
	short.VaultMasterKeyHex = "abcd"
	if _, _, _, err := BuildServer(context.Background(), short); err == nil {
		t.Fatal("short vault key must be rejected")
	}

	// The configured key is the one used: a CSD stored by one server opens on
	// another with the same key (and the same envelope fails with another key).
	cfg := base
	cfg.VaultMasterKeyHex = testVaultKeyHex
	cfg.SeedDemoMerchant = true
	srv1, _, _, err := BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	priv, cert, _ := generateSelfSignedCSD()
	if err := srv1.RegisterMerchantCSD("merch_demo_01", priv, cert); err != nil {
		t.Fatal(err)
	}
	p, err := buildVaultKeyProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	vault, _ := identity.NewCSDVaultServiceWithProvider(p)
	enc, err := vault.StoreCSD("m", cert, priv, "pwd")
	if err != nil || enc.KeyID != "v1" {
		t.Fatalf("envelope key id: %q %v", enc.KeyID, err)
	}
	rotated := cfg
	rotated.VaultMasterKeyHex = strings.Repeat("ab", 32)
	rotated.VaultKeyID = "v2"
	rotated.VaultPreviousKeys = "v1:" + testVaultKeyHex
	p2, err := buildVaultKeyProvider(rotated)
	if err != nil {
		t.Fatal(err)
	}
	v2, _ := identity.NewCSDVaultServiceWithProvider(p2)
	if _, err := v2.DecryptCSDForSigning(enc); err != nil {
		t.Fatalf("rotated keyring must decrypt v1 envelopes: %v", err)
	}

	// DEV_MODE without a key uses the dev key (legacy dev envelopes keep working).
	dev := Config{STPHMACSecret: "test_secret", DevMode: true}
	pd, err := buildVaultKeyProvider(dev)
	if err != nil {
		t.Fatal(err)
	}
	// Legacy envelope (pre key-id format) sealed with the old hardcoded key.
	tmpP, _ := identity.NewStaticKeyProvider("tmp", defaultVaultMasterKeyHex, nil)
	tmpVault, _ := identity.NewCSDVaultServiceWithProvider(tmpP)
	legacyEnc, _ := tmpVault.StoreCSD("m", cert, priv, "pwd")
	dek, _ := tmpP.UnwrapDEK(context.Background(), legacyEnc.KeyID, legacyEnc.EncryptedDEK)
	master, _ := hex.DecodeString(defaultVaultMasterKeyHex)
	block, _ := aes.NewCipher(master)
	gcm, _ := cipher.NewGCM(block)
	legacyEnc.KeyID, legacyEnc.IV = "", make([]byte, 12)
	legacyEnc.EncryptedDEK = gcm.Seal(nil, legacyEnc.IV, dek, nil)
	devVault, _ := identity.NewCSDVaultServiceWithProvider(pd)
	if _, err := devVault.DecryptCSDForSigning(legacyEnc); err != nil {
		t.Fatalf("dev key must open envelopes made with the old hardcoded key: %v", err)
	}
}
