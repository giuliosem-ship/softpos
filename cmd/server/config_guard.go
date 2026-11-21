package main

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"

	"softpos/pkg/identity"
)

const (
	defaultVaultMasterKeyHex = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
	defaultSTPHMACSecret     = "softpos_dev_secret_key_12345678"
)

var (
	ErrInsecureDefaultVaultKey   = errors.New("insecure configuration: default vault master key cannot be used in production")
	ErrInvalidVaultKey           = errors.New("insecure configuration: VAULT_MASTER_KEY_HEX must be a valid 32-byte hex key (VAULT_PREVIOUS_KEYS too)")
	ErrInsecureDefaultSTPSecret  = errors.New("insecure configuration: default STP HMAC secret cannot be used in production")
	ErrInsecureDatabaseURL       = errors.New("insecure configuration: database SSL mode cannot be disabled in production")
	ErrDemoMerchantInProduction  = errors.New("insecure configuration: demo merchant seeding must be disabled in production")
	ErrWeakJWTSecret             = errors.New("insecure configuration: JWT_SECRET must be set to at least 32 bytes in production")
	ErrDevModeInProduction       = errors.New("insecure configuration: DEV_MODE cannot be enabled in production")
	ErrInsecureCoDiWebhookSecret = errors.New("insecure configuration: CODI_WEBHOOK_SECRET must be set to at least 32 bytes in production")
	ErrMissingDatabaseURL        = errors.New("insecure configuration: DATABASE_URL is required in production (without it all money state is in memory)")
	ErrInsecureRedisPassword     = errors.New("insecure configuration: REDIS_PASSWORD must be set to at least 16 bytes when REDIS_ADDR is used in production")
	ErrWeakPlayIntegritySecret   = errors.New("insecure configuration: PLAY_INTEGRITY_SECRET, when set, must be at least 32 bytes in production")
	ErrInsecureAcquirerSecret    = errors.New("insecure configuration: ACQUIRER_WEBHOOK_SECRET must be set to at least 32 bytes in production when ACQUIRER_PROVIDER is set")
	ErrMockAcquirerInProduction  = errors.New("insecure configuration: ACQUIRER_PROVIDER=mock cannot be used in production (it approves without a processor)")
	ErrMockKYCInProduction       = errors.New("insecure configuration: KYC_PROVIDER=mock cannot be used in production (it passes every identity check)")
	ErrPlaceholderSecret         = errors.New("insecure configuration: a secret still holds the .env.example placeholder")
	ErrWeakReceiptTokenSecret    = errors.New("insecure configuration: RECEIPT_TOKEN_SECRET must be set to at least 32 bytes in production (it signs public receipt links)")
)

// minSecretLen is the minimum length of shared secrets in production.
const minSecretLen = 32

// EnvVar documents one environment variable read by the server.
type EnvVar struct {
	Name   string
	Secret bool // secrets must never be committed; the production guard checks each one
}

// ServerEnvVars is the complete list of environment variables the server
// reads (TestServerEnvVars_MatchCode keeps it in sync with the code and with
// .env.example). Every Secret entry is enforced by ValidateProductionConfig
// (TestValidateProductionConfig_CoversEverySecret).
var ServerEnvVars = []EnvVar{
	{Name: "APP_ENV"},
	{Name: "ENV"},
	{Name: "PORT"},
	{Name: "STATIC_DIR"},
	{Name: "MIGRATIONS_DIR"},
	{Name: "SQL_DRIVER"},
	{Name: "DATABASE_URL", Secret: true},
	{Name: "REDIS_ADDR"},
	{Name: "REDIS_PASSWORD", Secret: true},
	{Name: "DEV_MODE"},
	{Name: "SEED_DEMO_MERCHANT"},
	{Name: "JWT_SECRET", Secret: true},
	{Name: "AUTH_ACCESS_TTL"},
	{Name: "AUTH_REFRESH_TTL"},
	{Name: "AUTH_RATE_LIMIT_PER_MINUTE"},
	{Name: "PLAY_INTEGRITY_SECRET", Secret: true},
	{Name: "PLAY_INTEGRITY_PACKAGE"},
	{Name: "CODI_WEBHOOK_SECRET", Secret: true},
	{Name: "STP_HMAC_SECRET", Secret: true},
	{Name: "STP_BASE_URL"},
	{Name: "VAULT_MASTER_KEY_HEX", Secret: true},
	{Name: "VAULT_MASTER_KEY_ID"},
	{Name: "VAULT_PREVIOUS_KEYS", Secret: true},
	{Name: "DEFAULT_PLATFORM_FEE_BPS"},
	{Name: "ALLOW_CASHIER_REFUNDS"},
	{Name: "ACQUIRER_PROVIDER"},
	{Name: "ACQUIRER_WEBHOOK_SECRET", Secret: true},
	{Name: "ACQUIRER_RECONCILE_INTERVAL"},
	{Name: "KYC_PROVIDER"},
	{Name: "BACKOFFICE_BOOTSTRAP_OPERATOR_PHONE"},
	{Name: "OUTBOX_POLL_INTERVAL"},
	{Name: "OUTBOX_BATCH_SIZE"},
	{Name: "OUTBOX_PUBLISHER"},
	{Name: "OUTBOX_PUBLISH_TIMEOUT"},
	{Name: "PAYOUT_RECONCILE_INTERVAL"},
	{Name: "PAYOUT_RECONCILE_AFTER"},
	{Name: "SAT_69B_URL"},
	{Name: "SAT_69B_REFRESH_INTERVAL"},
	{Name: "SAT_69B_MAX_AGE"},
	{Name: "SAT_69B_MAX_SHRINK_PCT"},
	{Name: "CFDI_GLOBAL_INVOICE_INTERVAL"},
	{Name: "CFDI_CANCEL_POLL_INTERVAL"},
	{Name: "CFDI_CREDIT_NOTE_INTERVAL"},
	{Name: "CFDI_GLOBAL_MAX_AGE"},
	{Name: "RECEIPT_TOKEN_SECRET", Secret: true},
	{Name: "RECEIPT_BASE_URL"},
	{Name: "RECEIPT_TTL"},
}

// isPlaceholder reports whether v still holds a .env.example placeholder.
func isPlaceholder(v string) bool {
	return strings.Contains(strings.ToLower(v), "change_me")
}

// insecureVaultKey reports whether keyHex decodes to the public dev key (in
// any spelling: case, surrounding spaces) or to a trivial key (all bytes equal).
func insecureVaultKey(keyHex string) bool {
	k, err := identity.DecodeMasterKeyHex(keyHex)
	if err != nil {
		return false // malformed keys are rejected separately
	}
	dev, _ := hex.DecodeString(defaultVaultMasterKeyHex)
	return bytes.Equal(k, dev) || identity.IsTrivialMasterKey(k)
}

// ValidateProductionConfig performs fail-fast security auditing on the runtime configuration.
// If env is "production" (or "prod"), it rejects default dev secrets, plain-text DB connections,
// and synthetic merchant seeding.
func ValidateProductionConfig(cfg Config, env string) error {
	normEnv := strings.ToLower(strings.TrimSpace(env))
	if normEnv != "production" && normEnv != "prod" {
		// Non-production environments allow dev defaults
		return nil
	}

	// 0. No secret may still hold the .env.example placeholder.
	for _, v := range []string{cfg.DatabaseURL, cfg.RedisPassword, cfg.JWTSecret, cfg.PlayIntegritySecret,
		cfg.CoDiWebhookSecret, cfg.STPHMACSecret, cfg.VaultMasterKeyHex, cfg.VaultPreviousKeys, cfg.AcquirerWebhookSecret, cfg.ReceiptTokenSecret} {
		if isPlaceholder(v) {
			return ErrPlaceholderSecret
		}
	}

	// 1. Vault Master Key validation
	if cfg.VaultMasterKeyHex == "" || insecureVaultKey(cfg.VaultMasterKeyHex) {
		return ErrInsecureDefaultVaultKey
	}
	if _, err := identity.DecodeMasterKeyHex(cfg.VaultMasterKeyHex); err != nil {
		return ErrInvalidVaultKey
	}
	if ring, err := identity.ParseKeyring(cfg.VaultPreviousKeys); err != nil {
		return ErrInvalidVaultKey
	} else {
		for _, h := range ring {
			if _, err := identity.DecodeMasterKeyHex(h); err != nil || insecureVaultKey(h) {
				return ErrInvalidVaultKey
			}
		}
	}

	// 2. STP HMAC Secret validation
	if len(cfg.STPHMACSecret) < minSecretLen || cfg.STPHMACSecret == defaultSTPHMACSecret {
		return ErrInsecureDefaultSTPSecret
	}

	// 3. Database required, with SSL
	if strings.TrimSpace(cfg.DatabaseURL) == "" {
		return ErrMissingDatabaseURL
	}
	if strings.Contains(cfg.DatabaseURL, "sslmode=disable") {
		return ErrInsecureDatabaseURL
	}
	if cfg.RedisAddr != "" && len(cfg.RedisPassword) < 16 {
		return ErrInsecureRedisPassword
	}

	// 4. No demo merchant seeding in production
	if cfg.SeedDemoMerchant {
		return ErrDemoMerchantInProduction
	}

	// 5. Auth: explicit dev mode is forbidden and a strong JWT secret is required
	if cfg.DevMode {
		return ErrDevModeInProduction
	}
	if len(cfg.JWTSecret) < minSecretLen {
		return ErrWeakJWTSecret
	}

	// 6. Webhook and attestation signing secrets
	if len(cfg.CoDiWebhookSecret) < minSecretLen {
		return ErrInsecureCoDiWebhookSecret
	}
	if cfg.PlayIntegritySecret != "" && len(cfg.PlayIntegritySecret) < minSecretLen {
		return ErrWeakPlayIntegritySecret
	}

	// 7. Card processor: signed webhooks are the source of truth for card
	// payments; the mock approves without a processor.
	if cfg.AcquirerProvider != "" && len(cfg.AcquirerWebhookSecret) < minSecretLen {
		return ErrInsecureAcquirerSecret
	}
	if cfg.AcquirerProvider == "mock" {
		return ErrMockAcquirerInProduction
	}

	// 8. KYB identity verification: the mock passes every check.
	if cfg.KYCProvider == "mock" {
		return ErrMockKYCInProduction
	}

	// 9. Public sale receipts: the token secret is what makes links unguessable.
	if len(cfg.ReceiptTokenSecret) < minSecretLen {
		return ErrWeakReceiptTokenSecret
	}

	return nil
}
