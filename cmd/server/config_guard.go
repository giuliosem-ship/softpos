package main

import (
	"errors"
	"strings"
)

const (
	defaultVaultMasterKeyHex = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
	defaultSTPHMACSecret     = "softpos_dev_secret_key_12345678"
)

var (
	ErrInsecureDefaultVaultKey  = errors.New("insecure configuration: default vault master key cannot be used in production")
	ErrInsecureDefaultSTPSecret = errors.New("insecure configuration: default STP HMAC secret cannot be used in production")
	ErrInsecureDatabaseURL      = errors.New("insecure configuration: database SSL mode cannot be disabled in production")
	ErrDemoMerchantInProduction = errors.New("insecure configuration: demo merchant seeding must be disabled in production")
)

// ValidateProductionConfig performs fail-fast security auditing on the runtime configuration.
// If env is "production" (or "prod"), it rejects default dev secrets, plain-text DB connections,
// and synthetic merchant seeding.
func ValidateProductionConfig(cfg Config, env string) error {
	normEnv := strings.ToLower(strings.TrimSpace(env))
	if normEnv != "production" && normEnv != "prod" {
		// Non-production environments allow dev defaults
		return nil
	}

	// 1. Vault Master Key validation
	if cfg.VaultMasterKeyHex == "" || cfg.VaultMasterKeyHex == defaultVaultMasterKeyHex {
		return ErrInsecureDefaultVaultKey
	}

	// 2. STP HMAC Secret validation
	if cfg.STPHMACSecret == "" || cfg.STPHMACSecret == defaultSTPHMACSecret {
		return ErrInsecureDefaultSTPSecret
	}

	// 3. Database SSL enforcement
	if strings.Contains(cfg.DatabaseURL, "sslmode=disable") {
		return ErrInsecureDatabaseURL
	}

	// 4. No demo merchant seeding in production
	if cfg.SeedDemoMerchant {
		return ErrDemoMerchantInProduction
	}

	return nil
}
