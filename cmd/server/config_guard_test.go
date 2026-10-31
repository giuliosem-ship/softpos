package main

import (
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
		Port:              8443,
		DatabaseURL:       "postgres://softpos_user:UltraSecurePass987@db.prod.internal:5432/softpos?sslmode=require",
		VaultMasterKeyHex: "a1b2c3d4e5f67890123456789abcdef0123456789abcdef0123456789abcdef0",
		STPHMACSecret:     "prod_stp_hmac_secret_very_long_and_random_string_xyz",
		SeedDemoMerchant:  false,
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
}
