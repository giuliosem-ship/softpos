package stp

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
)

// STPSigner holds the necessary keys and secret for signing and verifying.
type STPSigner struct {
	stpPublicKey  *rsa.PublicKey
	myPrivateKey  *rsa.PrivateKey
	hmacSecret    []byte
}

// NewSTPSigner creates a new STPSigner instance.
func NewSTPSigner(pubKey *rsa.PublicKey, privKey *rsa.PrivateKey, hmacSecret string) *STPSigner {
	return &STPSigner{
		stpPublicKey: pubKey,
		myPrivateKey: privKey,
		hmacSecret:   []byte(hmacSecret),
	}
}

// NewSTPSignerFromPEM creates a new STPSigner instance from PEM encoded keys.
func NewSTPSignerFromPEM(pubPEM string, privPEM string, hmacSecret string) (*STPSigner, error) {
	block, _ := pem.Decode([]byte(pubPEM))
	if block == nil || (block.Type != "PUBLIC KEY" && block.Type != "RSA PUBLIC KEY") {
		return nil, errors.New("failed to decode public key PEM")
	}

	var pubKey *rsa.PublicKey
	var err error
	if block.Type == "RSA PUBLIC KEY" {
		pubKey, err = x509.ParsePKCS1PublicKey(block.Bytes)
	} else {
		pubKeyInterface, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		var ok bool
		pubKey, ok = pubKeyInterface.(*rsa.PublicKey)
		if !ok {
			return nil, errors.New("public key is not RSA")
		}
	}

	if err != nil {
		return nil, err
	}

	block, _ = pem.Decode([]byte(privPEM))
	if block == nil || (block.Type != "RSA PRIVATE KEY" && block.Type != "PRIVATE KEY") {
		return nil, errors.New("failed to decode private key PEM")
	}

	var privKey *rsa.PrivateKey
	if block.Type == "RSA PRIVATE KEY" {
		privKey, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	} else {
		privKeyInterface, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		var ok bool
		privKey, ok = privKeyInterface.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("private key is not RSA")
		}
	}

	if err != nil {
		return nil, err
	}

	return &STPSigner{
		stpPublicKey: pubKey,
		myPrivateKey: privKey,
		hmacSecret:   []byte(hmacSecret),
	}, nil
}

// BuildAbonoCanonicalString constructs the canonical string for an abono without floating point math.
func (s *STPSigner) BuildAbonoCanonicalString(cuentaBeneficiario string, montoCents int64, claveRastreo string, institucionOrdenante string, cuentaOrdenante string) string {
	montoStr := fmt.Sprintf("%d.%02d", montoCents/100, montoCents%100)
	return fmt.Sprintf("||%s|%s|%s|%s|%s||", cuentaBeneficiario, montoStr, claveRastreo, institucionOrdenante, cuentaOrdenante)
}

// VerifyRSASignature verifies the RSA signature of the canonical data.
func (s *STPSigner) VerifyRSASignature(canonicalData string, signatureBase64 string) bool {
	hash := sha256.Sum256([]byte(canonicalData))
	signature, err := base64.StdEncoding.DecodeString(signatureBase64)
	if err != nil {
		return false
	}

	err = rsa.VerifyPKCS1v15(s.stpPublicKey, crypto.SHA256, hash[:], signature)
	return err == nil
}

// SignRSA signs the canonical data using RSA.
func (s *STPSigner) SignRSA(canonicalData string) (string, error) {
	hash := sha256.Sum256([]byte(canonicalData))
	signature, err := rsa.SignPKCS1v15(rand.Reader, s.myPrivateKey, crypto.SHA256, hash[:])
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(signature), nil
}

// SignHMAC signs the canonical data using HMAC.
func (s *STPSigner) SignHMAC(canonicalData string) string {
	h := hmac.New(sha256.New, s.hmacSecret)
	h.Write([]byte(canonicalData))
	return fmt.Sprintf("%x", h.Sum(nil))
}

// VerifyHMAC verifies the HMAC signature of the canonical data.
func (s *STPSigner) VerifyHMAC(canonicalData string, signatureHex string) bool {
	expectedSig := s.SignHMAC(canonicalData)
	return hmac.Equal([]byte(expectedSig), []byte(signatureHex))
}
