// Package fiscaltest builds SAT-like CSD fixtures (self-signed, never real
// SAT certificates) for tests and the DEV_MODE demo.
package fiscaltest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"math/big"
	"time"

	"softpos/pkg/fiscal"
)

// CSDOptions shapes a fixture certificate.
type CSDOptions struct {
	RFC       string // subject x500UniqueIdentifier
	Name      string
	FIEL      bool // e.firma key usage instead of a CSD
	NotBefore time.Time
	NotAfter  time.Time
	Serial    string // 20 ASCII digits (SAT style); default 30001000000500003416
	AES       bool   // AES-256-CBC instead of DES-EDE3-CBC key encryption
}

// CSD is a generated fixture.
type CSD struct {
	CertDER      []byte
	EncryptedKey []byte // SAT .key format
	PlainKey     []byte // PKCS#8
	Password     []byte
	Key          *rsa.PrivateKey
}

// NewCSD generates a 2048-bit RSA key and a SAT-like certificate.
func NewCSD(o CSDOptions) (CSD, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return CSD{}, err
	}
	if o.Serial == "" {
		o.Serial = "30001000000500003416"
	}
	if o.Name == "" {
		o.Name = "CONTRIBUYENTE DE PRUEBA"
	}
	if o.NotBefore.IsZero() {
		o.NotBefore = time.Now().Add(-24 * time.Hour)
	}
	if o.NotAfter.IsZero() {
		o.NotAfter = time.Now().Add(4 * 365 * 24 * time.Hour)
	}
	subject := pkix.Name{
		CommonName:   o.Name,
		Organization: []string{o.Name},
		ExtraNames: []pkix.AttributeTypeAndValue{
			{Type: asn1.ObjectIdentifier{2, 5, 4, 45}, Value: o.RFC + " / "},
			{Type: asn1.ObjectIdentifier{2, 5, 4, 5}, Value: " / "},
		},
	}
	tpl := x509.Certificate{
		SerialNumber: new(big.Int).SetBytes([]byte(o.Serial)),
		Subject:      subject,
		Issuer:       pkix.Name{CommonName: "AC DE PRUEBAS (NO SAT)"},
		NotBefore:    o.NotBefore,
		NotAfter:     o.NotAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageContentCommitment,
	}
	if o.FIEL {
		tpl.KeyUsage |= x509.KeyUsageDataEncipherment | x509.KeyUsageKeyAgreement
		tpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageEmailProtection, x509.ExtKeyUsageClientAuth}
	} else {
		tpl.Subject.OrganizationalUnit = []string{"Sucursal 1"}
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &tpl, &tpl, &key.PublicKey, key)
	if err != nil {
		return CSD{}, err
	}
	plain, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return CSD{}, err
	}
	pwd := []byte("12345678a")
	enc, err := fiscal.EncryptSATPrivateKey(plain, pwd, o.AES)
	if err != nil {
		return CSD{}, err
	}
	return CSD{CertDER: certDER, EncryptedKey: enc, PlainKey: plain, Password: pwd, Key: key}, nil
}
