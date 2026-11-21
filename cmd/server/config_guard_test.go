package main

import (
	"context"
	"errors"
	"testing"
)

func TestValidateProductionConfig_DevelopmentPassesWithDefaults(t *testing.T) {
	devCfg := Config{
		Port:              8080,
		DatabaseURL:       "postgres://postgres:postgres@localhost:5432/softpos?sslmode=disable",
		VaultMasterKeyHex: "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20",
		STPHMACSecret:     "softpos_dev_secret_key_12345678",
		SeedDemoMerchant:  true,
	}

	// Dev environment passes
	if err := ValidateProductionConfig(devCfg, "development"); err != nil {
		t.Errorf("expected development to pass with defaults, got error: %v", err)
	}

	// Test environment passes
	if err := ValidateProductionConfig(devCfg, "test"); err != nil {
		t.Errorf("expected test to pass with defaults, got error: %v", err)
	}
}

func TestValidateProductionConfig_RejectsInsecureSettingsInProduction(t *testing.T) {
	prodCfg := Config{
		Port:               8443,
		DatabaseURL:        "postgres://softpos_user:UltraSecurePass987@db.prod.internal:5432/softpos?sslmode=require",
		VaultMasterKeyHex:  "a1b2c3d4e5f67890123456789abcdef0123456789abcdef0123456789abcdef0",
		STPHMACSecret:      "prod_stp_hmac_secret_very_long_and_random_string_xyz",
		SeedDemoMerchant:   false,
		JWTSecret:          "prod_jwt_secret_with_at_least_32_random_bytes",
		CoDiWebhookSecret:  "prod_codi_webhook_secret_with_32_random_bytes",
		ReceiptTokenSecret: "prod_receipt_token_secret_32_random_bytes!",
	}

	// 1. Valid production config passes
	if err := ValidateProductionConfig(prodCfg, "production"); err != nil {
		t.Errorf("expected valid production config to pass, got: %v", err)
	}

	// 2. Reject default vault master key
	badVaultCfg := prodCfg
	badVaultCfg.VaultMasterKeyHex = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
	if err := ValidateProductionConfig(badVaultCfg, "production"); !errors.Is(err, ErrInsecureDefaultVaultKey) {
		t.Errorf("expected ErrInsecureDefaultVaultKey, got: %v", err)
	}

	// 2b. Reject malformed vault keys (active or previous)
	shortVault := prodCfg
	shortVault.VaultMasterKeyHex = "a1b2c3"
	if err := ValidateProductionConfig(shortVault, "production"); !errors.Is(err, ErrInvalidVaultKey) {
		t.Errorf("expected ErrInvalidVaultKey for a short key, got: %v", err)
	}
	badPrev := prodCfg
	badPrev.VaultPreviousKeys = "v0:nothex"
	if err := ValidateProductionConfig(badPrev, "production"); !errors.Is(err, ErrInvalidVaultKey) {
		t.Errorf("expected ErrInvalidVaultKey for a bad previous key, got: %v", err)
	}

	// 2c. The dev key in another spelling, or a trivial key, is not a real key
	for _, k := range []string{
		"0102030405060708090A0B0C0D0E0F101112131415161718191A1B1C1D1E1F20",
		" 0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20",
		"0000000000000000000000000000000000000000000000000000000000000000",
		"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
	} {
		weak := prodCfg
		weak.VaultMasterKeyHex = k
		if err := ValidateProductionConfig(weak, "production"); !errors.Is(err, ErrInsecureDefaultVaultKey) {
			t.Errorf("vault key %q: expected ErrInsecureDefaultVaultKey, got: %v", k, err)
		}
	}
	devPrev := prodCfg
	devPrev.VaultPreviousKeys = "dev:0102030405060708090A0B0C0D0E0F101112131415161718191A1B1C1D1E1F20"
	if err := ValidateProductionConfig(devPrev, "production"); !errors.Is(err, ErrInvalidVaultKey) {
		t.Errorf("expected ErrInvalidVaultKey for the dev key as a previous key, got: %v", err)
	}

	// 3. Reject default STP HMAC secret
	badSTPCfg := prodCfg
	badSTPCfg.STPHMACSecret = "softpos_dev_secret_key_12345678"
	if err := ValidateProductionConfig(badSTPCfg, "production"); !errors.Is(err, ErrInsecureDefaultSTPSecret) {
		t.Errorf("expected ErrInsecureDefaultSTPSecret, got: %v", err)
	}

	// 4. Reject sslmode=disable in production
	badDBCfg := prodCfg
	badDBCfg.DatabaseURL = "postgres://user:pass@host:5432/db?sslmode=disable"
	if err := ValidateProductionConfig(badDBCfg, "production"); !errors.Is(err, ErrInsecureDatabaseURL) {
		t.Errorf("expected ErrInsecureDatabaseURL, got: %v", err)
	}

	// 5. Reject demo merchant seeding in production
	badSeedCfg := prodCfg
	badSeedCfg.SeedDemoMerchant = true
	if err := ValidateProductionConfig(badSeedCfg, "production"); !errors.Is(err, ErrDemoMerchantInProduction) {
		t.Errorf("expected ErrDemoMerchantInProduction, got: %v", err)
	}

	// 6. Reject missing / short JWT secret and DEV_MODE in production
	for _, weak := range []string{"", "short_secret"} {
		badJWT := prodCfg
		badJWT.JWTSecret = weak
		if err := ValidateProductionConfig(badJWT, "production"); !errors.Is(err, ErrWeakJWTSecret) {
			t.Errorf("JWT_SECRET %q: expected ErrWeakJWTSecret, got: %v", weak, err)
		}
	}
	// 7. Reject missing / short CoDi webhook secret and missing STP secret
	for _, weak := range []string{"", "short"} {
		badCoDi := prodCfg
		badCoDi.CoDiWebhookSecret = weak
		if err := ValidateProductionConfig(badCoDi, "production"); !errors.Is(err, ErrInsecureCoDiWebhookSecret) {
			t.Errorf("CODI_WEBHOOK_SECRET %q: expected ErrInsecureCoDiWebhookSecret, got: %v", weak, err)
		}
	}
	noSTP := prodCfg
	noSTP.STPHMACSecret = ""
	if err := ValidateProductionConfig(noSTP, "production"); !errors.Is(err, ErrInsecureDefaultSTPSecret) {
		t.Errorf("missing STP secret: expected ErrInsecureDefaultSTPSecret, got: %v", err)
	}

	devCfg := prodCfg
	devCfg.DevMode = true
	if err := ValidateProductionConfig(devCfg, "production"); !errors.Is(err, ErrDevModeInProduction) {
		t.Errorf("expected ErrDevModeInProduction, got: %v", err)
	}
}

func TestValidateProductionConfig_CORSOrigins(t *testing.T) {
	cfg := Config{
		DatabaseURL:        "postgres://softpos:Str0ngRandomPassw0rd@db.prod.internal:5432/softpos?sslmode=require",
		VaultMasterKeyHex:  "a1b2c3d4e5f67890123456789abcdef0123456789abcdef0123456789abcdef0",
		STPHMACSecret:      "prod_stp_hmac_secret_very_long_and_random_string_xyz",
		JWTSecret:          "prod_jwt_secret_with_at_least_32_random_bytes",
		CoDiWebhookSecret:  "prod_codi_webhook_secret_with_32_random_bytes",
		ReceiptTokenSecret: "prod_receipt_token_secret_32_random_bytes!",
	}
	for origins, want := range map[string]error{
		"":                                    nil,
		"https://pos.kerno.mx":                nil,
		"https://pos.kerno.mx,http://x.local": ErrInsecureCORSOrigins,
		"*":                                   ErrInsecureCORSOrigins,
	} {
		cfg.CORSAllowedOrigins = origins
		if err := ValidateProductionConfig(cfg, "production"); !errors.Is(err, want) {
			t.Errorf("%q: want %v, got %v", origins, want, err)
		}
	}
	if _, _, _, err := BuildServer(context.Background(), Config{DevMode: true, CORSAllowedOrigins: "*"}); err == nil {
		t.Fatal("an invalid CORS_ALLOWED_ORIGINS must fail the boot")
	}
}
