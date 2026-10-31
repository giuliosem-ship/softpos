package identity

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var (
	ErrInvalidIntegrityToken = errors.New("invalid or malformed play integrity token")
	ErrDeviceNotIntegrity    = errors.New("device does not meet hardware-backed integrity requirements")
	ErrPackageMismatch       = errors.New("package name mismatch")
	ErrNonceMismatch         = errors.New("nonce mismatch or replay attack")
	ErrAttestationExpired    = errors.New("play integrity attestation expired")
)

// PlayIntegrityVerdict represents the decoded attestation payload from Google Play Integrity.
type PlayIntegrityVerdict struct {
	PackageName              string   `json:"packageName"`
	AppLicensingVerdict      string   `json:"appLicensingVerdict"`
	DeviceRecognitionVerdict []string `json:"deviceRecognitionVerdict"`
	Nonce                    string   `json:"nonce"`
	TimestampMillis          int64    `json:"timestampMillis"`
}

// PlayIntegrityVerifier verifies Google Play Integrity tokens for SoftPOS Android endpoints.
type PlayIntegrityVerifier struct {
	expectedPackageName string
	secret              string
	maxAge              time.Duration
}

// NewPlayIntegrityVerifier constructs a verifier for Android attestation tokens.
func NewPlayIntegrityVerifier(expectedPackageName string, secret string, maxAge time.Duration) *PlayIntegrityVerifier {
	return &PlayIntegrityVerifier{
		expectedPackageName: expectedPackageName,
		secret:              secret,
		maxAge:              maxAge,
	}
}

// CreatePlayIntegrityTestToken generates a signed JWT-like token for testing or local simulation.
func CreatePlayIntegrityTestToken(verdict PlayIntegrityVerdict, secret string) (string, error) {
	headerJSON := []byte(`{"alg":"HS256","typ":"JWT"}`)
	headerB64 := base64.RawURLEncoding.EncodeToString(headerJSON)

	payloadJSON, err := json.Marshal(verdict)
	if err != nil {
		return "", err
	}
	payloadB64 := base64.RawURLEncoding.EncodeToString(payloadJSON)

	signingInput := headerB64 + "." + payloadB64
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	signature := mac.Sum(nil)
	sigB64 := base64.RawURLEncoding.EncodeToString(signature)

	return signingInput + "." + sigB64, nil
}

// VerifyToken validates the signature, freshness, nonce, package name, and device integrity.
func (v *PlayIntegrityVerifier) VerifyToken(tokenStr string, expectedNonce string) (*PlayIntegrityVerdict, error) {
	parts := strings.Split(tokenStr, ".")
	if len(parts) != 3 {
		return nil, ErrInvalidIntegrityToken
	}

	signingInput := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		sig, err = base64.URLEncoding.DecodeString(parts[2])
		if err != nil {
			return nil, ErrInvalidIntegrityToken
		}
	}

	mac := hmac.New(sha256.New, []byte(v.secret))
	mac.Write([]byte(signingInput))
	expectedSig := mac.Sum(nil)

	if !hmac.Equal(sig, expectedSig) {
		return nil, ErrInvalidIntegrityToken
	}

	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payloadJSON, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return nil, ErrInvalidIntegrityToken
		}
	}

	var verdict PlayIntegrityVerdict
	if err := json.Unmarshal(payloadJSON, &verdict); err != nil {
		return nil, ErrInvalidIntegrityToken
	}

	// 1. Check Package Name
	if v.expectedPackageName != "" && verdict.PackageName != v.expectedPackageName {
		return nil, ErrPackageMismatch
	}

	// 2. Check Nonce
	if expectedNonce != "" && verdict.Nonce != expectedNonce {
		return nil, ErrNonceMismatch
	}

	// 3. Check Freshness / Expiration
	if v.maxAge > 0 && verdict.TimestampMillis > 0 {
		age := time.Duration(time.Now().UnixMilli()-verdict.TimestampMillis) * time.Millisecond
		if age > v.maxAge || age < -30*time.Second {
			return nil, ErrAttestationExpired
		}
	}

	// 4. Check Device Recognition Verdict (must meet at least MEETS_DEVICE_INTEGRITY)
	meetsDeviceIntegrity := false
	for _, rec := range verdict.DeviceRecognitionVerdict {
		if rec == "MEETS_DEVICE_INTEGRITY" || rec == "MEETS_STRONG_INTEGRITY" {
			meetsDeviceIntegrity = true
			break
		}
	}

	if !meetsDeviceIntegrity {
		return nil, ErrDeviceNotIntegrity
	}

	return &verdict, nil
}
