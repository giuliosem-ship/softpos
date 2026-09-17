package stp

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"
)

func generateTestKeyPair(t *testing.T) (privPEM string, pubPEM string) {
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}

	privDER := x509.MarshalPKCS1PrivateKey(privKey)
	privBlock := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: privDER,
	}
	privPEM = string(pem.EncodeToMemory(privBlock))

	pubDER, err := x509.MarshalPKIXPublicKey(&privKey.PublicKey)
	if err != nil {
		t.Fatalf("failed to marshal public key: %v", err)
	}
	pubBlock := &pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: pubDER,
	}
	pubPEM = string(pem.EncodeToMemory(pubBlock))
	return privPEM, pubPEM
}

func TestBuildAbonoCanonicalString(t *testing.T) {
	signer := NewSTPSigner(nil, nil, "secret")
	canonical := signer.BuildAbonoCanonicalString(
		"646180123456789014",
		150050, // $1,500.50 MXN
		"SPEI_TRACK_001",
		"40012",
		"012180001234567890",
	)

	expected := "||646180123456789014|1500.50|SPEI_TRACK_001|40012|012180001234567890||"
	if canonical != expected {
		t.Errorf("canonical string mismatch: expected %s, got %s", expected, canonical)
	}
}

func TestRSASignAndVerify(t *testing.T) {
	privPEM, pubPEM := generateTestKeyPair(t)
	signer, err := NewSTPSignerFromPEM(pubPEM, privPEM, "")
	if err != nil {
		t.Fatalf("unexpected error creating signer: %v", err)
	}

	data := "||646180123456789014|100.00|TRACK123|40012|012180001234567890||"
	sig, err := signer.SignRSA(data)
	if err != nil {
		t.Fatalf("unexpected error signing: %v", err)
	}

	// 1. Valid verification
	if !signer.VerifyRSASignature(data, sig) {
		t.Errorf("expected valid RSA signature to verify successfully")
	}

	// 2. Tampered data fails
	tamperedData := "||646180123456789014|1000.00|TRACK123|40012|012180001234567890||"
	if signer.VerifyRSASignature(tamperedData, sig) {
		t.Errorf("expected verification to fail for tampered data")
	}

	// 3. Corrupted signature fails
	if signer.VerifyRSASignature(data, "invalid_base64_signature") {
		t.Errorf("expected verification to fail for corrupt signature")
	}
}

func TestHMACSignAndVerify(t *testing.T) {
	secret := "stp_webhook_hmac_secret_key_123"
	signer := NewSTPSigner(nil, nil, secret)

	data := "||TEST_DATA_PAYLOAD||"
	sig := signer.SignHMAC(data)

	if !signer.VerifyHMAC(data, sig) {
		t.Errorf("expected valid HMAC signature to verify")
	}

	if signer.VerifyHMAC("||ALTERED_DATA||", sig) {
		t.Errorf("expected altered data to fail HMAC verification")
	}
}
