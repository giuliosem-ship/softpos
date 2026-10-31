package identity

import (
	"errors"
	"testing"
	"time"
)

func TestPlayIntegrityVerifier_ValidTokenPasses(t *testing.T) {
	secret := "play_integrity_hmac_secret_32_bytes_123"
	verifier := NewPlayIntegrityVerifier("mx.softpos.app", secret, 5*time.Minute)

	verdict := PlayIntegrityVerdict{
		PackageName:              "mx.softpos.app",
		AppLicensingVerdict:      "LICENSED",
		DeviceRecognitionVerdict: []string{"MEETS_DEVICE_INTEGRITY", "MEETS_BASIC_INTEGRITY"},
		Nonce:                    "secure_nonce_12345",
		TimestampMillis:          time.Now().UnixMilli(),
	}

	token, err := CreatePlayIntegrityTestToken(verdict, secret)
	if err != nil {
		t.Fatalf("CreatePlayIntegrityTestToken failed: %v", err)
	}

	parsed, err := verifier.VerifyToken(token, "secure_nonce_12345")
	if err != nil {
		t.Fatalf("VerifyToken failed on valid token: %v", err)
	}
	if parsed.PackageName != "mx.softpos.app" {
		t.Errorf("expected package name mx.softpos.app, got: %s", parsed.PackageName)
	}
}

func TestPlayIntegrityVerifier_TamperedSignatureFails(t *testing.T) {
	secret := "play_integrity_hmac_secret_32_bytes_123"
	verifier := NewPlayIntegrityVerifier("mx.softpos.app", secret, 5*time.Minute)

	verdict := PlayIntegrityVerdict{
		PackageName:              "mx.softpos.app",
		AppLicensingVerdict:      "LICENSED",
		DeviceRecognitionVerdict: []string{"MEETS_DEVICE_INTEGRITY"},
		Nonce:                    "nonce_abc",
		TimestampMillis:          time.Now().UnixMilli(),
	}

	// Signed with wrong secret
	token, _ := CreatePlayIntegrityTestToken(verdict, "wrong_attacker_secret_999999999")
	_, err := verifier.VerifyToken(token, "nonce_abc")
	if !errors.Is(err, ErrInvalidIntegrityToken) {
		t.Errorf("expected ErrInvalidIntegrityToken for bad signature, got: %v", err)
	}
}

func TestPlayIntegrityVerifier_RootedOrHookedDeviceRejected(t *testing.T) {
	secret := "play_integrity_hmac_secret_32_bytes_123"
	verifier := NewPlayIntegrityVerifier("mx.softpos.app", secret, 5*time.Minute)

	// Device only meets NO integrity verdict (e.g. rooted, bootloader unlocked)
	verdict := PlayIntegrityVerdict{
		PackageName:              "mx.softpos.app",
		AppLicensingVerdict:      "LICENSED",
		DeviceRecognitionVerdict: []string{}, // No MEETS_DEVICE_INTEGRITY
		Nonce:                    "nonce_rooted",
		TimestampMillis:          time.Now().UnixMilli(),
	}

	token, _ := CreatePlayIntegrityTestToken(verdict, secret)
	_, err := verifier.VerifyToken(token, "nonce_rooted")
	if !errors.Is(err, ErrDeviceNotIntegrity) {
		t.Errorf("expected ErrDeviceNotIntegrity for untrusted device, got: %v", err)
	}
}

func TestPlayIntegrityVerifier_PackageMismatchOrNonceMismatch(t *testing.T) {
	secret := "play_integrity_hmac_secret_32_bytes_123"
	verifier := NewPlayIntegrityVerifier("mx.softpos.app", secret, 5*time.Minute)

	// 1. Package mismatch (cloned app)
	verdictCloned := PlayIntegrityVerdict{
		PackageName:              "com.malicious.repackaged.app",
		AppLicensingVerdict:      "LICENSED",
		DeviceRecognitionVerdict: []string{"MEETS_DEVICE_INTEGRITY"},
		Nonce:                    "valid_nonce",
		TimestampMillis:          time.Now().UnixMilli(),
	}
	tokenCloned, _ := CreatePlayIntegrityTestToken(verdictCloned, secret)
	_, errPkg := verifier.VerifyToken(tokenCloned, "valid_nonce")
	if !errors.Is(errPkg, ErrPackageMismatch) {
		t.Errorf("expected ErrPackageMismatch, got: %v", errPkg)
	}

	// 2. Nonce mismatch (replay attack)
	verdictValid := PlayIntegrityVerdict{
		PackageName:              "mx.softpos.app",
		AppLicensingVerdict:      "LICENSED",
		DeviceRecognitionVerdict: []string{"MEETS_DEVICE_INTEGRITY"},
		Nonce:                    "original_nonce_1",
		TimestampMillis:          time.Now().UnixMilli(),
	}
	tokenValid, _ := CreatePlayIntegrityTestToken(verdictValid, secret)
	_, errNonce := verifier.VerifyToken(tokenValid, "expected_different_nonce_2")
	if !errors.Is(errNonce, ErrNonceMismatch) {
		t.Errorf("expected ErrNonceMismatch, got: %v", errNonce)
	}
}
