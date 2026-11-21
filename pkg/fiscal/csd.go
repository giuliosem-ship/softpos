package fiscal

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/des"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"hash"
	"strings"
	"time"
)

// CSD (Certificado de Sello Digital) loading: the SAT delivers a DER X.509
// certificate (.cer) and a PKCS#8 EncryptedPrivateKeyInfo (.key, PBES2 /
// PBKDF2 with DES-EDE3-CBC or AES-CBC) protected by the merchant's
// password.

// CSD rejection reasons (HTTP 422 CSD_INVALID "reason").
const (
	CSDReasonCertUnreadable = "CER_UNREADABLE"
	CSDReasonKeyUnreadable  = "KEY_UNREADABLE"
	CSDReasonUnsupported    = "KEY_ENCRYPTION_UNSUPPORTED"
	CSDReasonWrongPassword  = "WRONG_PASSWORD"
	CSDReasonKeyMismatch    = "KEY_CERT_MISMATCH"
	CSDReasonRFCMismatch    = "RFC_MISMATCH"
	CSDReasonExpired        = "EXPIRED"
	CSDReasonNotYetValid    = "NOT_YET_VALID"
	CSDReasonFIEL           = "FIEL_NOT_CSD"
	CSDReasonNotRSA         = "NOT_RSA"
)

// CSDError is a validation failure with its reason code.
type CSDError struct{ Reason string }

func (e *CSDError) Error() string { return "invalid CSD: " + e.Reason }

func csdErr(reason string) error { return &CSDError{Reason: reason} }

// CSDReason returns the reason code of a CSD validation error ("" otherwise).
func CSDReason(err error) string {
	var ce *CSDError
	if errors.As(err, &ce) {
		return ce.Reason
	}
	return ""
}

var (
	oidPBES2         = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 13}
	oidPBKDF2        = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 12}
	oidHMACWithSHA1  = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 7}
	oidHMACWithSHA2  = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 9}
	oidDESEDE3CBC    = asn1.ObjectIdentifier{1, 2, 840, 113549, 3, 7}
	oidAES128CBC     = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 2}
	oidAES192CBC     = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 22}
	oidAES256CBC     = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 42}
	oidX500UniqueID  = asn1.ObjectIdentifier{2, 5, 4, 45}
	oidSubjectSerial = asn1.ObjectIdentifier{2, 5, 4, 5}
)

type encryptedPrivateKeyInfo struct {
	Algo          algorithmIdentifier
	EncryptedData []byte
}

type algorithmIdentifier struct {
	Algorithm  asn1.ObjectIdentifier
	Parameters asn1.RawValue `asn1:"optional"`
}

type pbes2Params struct {
	KeyDerivationFunc algorithmIdentifier
	EncryptionScheme  algorithmIdentifier
}

type pbkdf2Params struct {
	Salt           []byte
	IterationCount int
	KeyLength      int                 `asn1:"optional"`
	PRF            algorithmIdentifier `asn1:"optional"`
}

func pemOrDER(b []byte, label string) []byte {
	if blk, _ := pem.Decode(b); blk != nil && strings.Contains(blk.Type, label) {
		return blk.Bytes
	}
	return b
}

// DecryptSATPrivateKey decrypts a SAT .key (PKCS#8 EncryptedPrivateKeyInfo,
// PBES2) with password and returns the plaintext PKCS#8 DER. The caller
// must clear the result after use.
func DecryptSATPrivateKey(keyDER, password []byte) ([]byte, error) {
	keyDER = pemOrDER(keyDER, "PRIVATE KEY")
	var info encryptedPrivateKeyInfo
	if rest, err := asn1.Unmarshal(keyDER, &info); err != nil || len(rest) != 0 {
		return nil, csdErr(CSDReasonKeyUnreadable)
	}
	if !info.Algo.Algorithm.Equal(oidPBES2) {
		return nil, csdErr(CSDReasonUnsupported)
	}
	var params pbes2Params
	if _, err := asn1.Unmarshal(info.Algo.Parameters.FullBytes, &params); err != nil {
		return nil, csdErr(CSDReasonKeyUnreadable)
	}
	if !params.KeyDerivationFunc.Algorithm.Equal(oidPBKDF2) {
		return nil, csdErr(CSDReasonUnsupported)
	}
	var kdf pbkdf2Params
	if _, err := asn1.Unmarshal(params.KeyDerivationFunc.Parameters.FullBytes, &kdf); err != nil || kdf.IterationCount <= 0 || kdf.IterationCount > 10_000_000 {
		return nil, csdErr(CSDReasonKeyUnreadable)
	}
	var prf func() hash.Hash = sha1.New
	switch {
	case kdf.PRF.Algorithm == nil, kdf.PRF.Algorithm.Equal(oidHMACWithSHA1):
	case kdf.PRF.Algorithm.Equal(oidHMACWithSHA2):
		prf = sha256.New
	default:
		return nil, csdErr(CSDReasonUnsupported)
	}
	var iv []byte
	if _, err := asn1.Unmarshal(params.EncryptionScheme.Parameters.FullBytes, &iv); err != nil {
		return nil, csdErr(CSDReasonKeyUnreadable)
	}
	var keyLen int
	var newCipher func([]byte) (cipher.Block, error)
	switch alg := params.EncryptionScheme.Algorithm; {
	case alg.Equal(oidDESEDE3CBC):
		keyLen, newCipher = 24, des.NewTripleDESCipher
	case alg.Equal(oidAES128CBC):
		keyLen, newCipher = 16, aes.NewCipher
	case alg.Equal(oidAES192CBC):
		keyLen, newCipher = 24, aes.NewCipher
	case alg.Equal(oidAES256CBC):
		keyLen, newCipher = 32, aes.NewCipher
	default:
		return nil, csdErr(CSDReasonUnsupported)
	}
	if kdf.KeyLength != 0 && kdf.KeyLength != keyLen {
		return nil, csdErr(CSDReasonKeyUnreadable)
	}
	key, err := pbkdf2.Key(prf, string(password), kdf.Salt, kdf.IterationCount, keyLen)
	if err != nil {
		return nil, csdErr(CSDReasonKeyUnreadable)
	}
	defer clear(key)
	block, err := newCipher(key)
	if err != nil {
		return nil, csdErr(CSDReasonKeyUnreadable)
	}
	bs := block.BlockSize()
	if len(iv) != bs || len(info.EncryptedData) == 0 || len(info.EncryptedData)%bs != 0 {
		return nil, csdErr(CSDReasonKeyUnreadable)
	}
	plain := make([]byte, len(info.EncryptedData))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, info.EncryptedData)
	// PKCS#7 padding; a wrong password almost always breaks it (and, when
	// it does not, the PKCS#8 parse below fails).
	pad := int(plain[len(plain)-1])
	if pad == 0 || pad > bs || !bytes.Equal(plain[len(plain)-pad:], bytes.Repeat([]byte{byte(pad)}, pad)) {
		clear(plain)
		return nil, csdErr(CSDReasonWrongPassword)
	}
	plain = plain[:len(plain)-pad]
	if _, err := x509.ParsePKCS8PrivateKey(plain); err != nil {
		clear(plain)
		return nil, csdErr(CSDReasonWrongPassword)
	}
	return plain, nil
}

// EncryptSATPrivateKey encrypts a plaintext PKCS#8 key in the SAT .key
// format (PBES2, PBKDF2-HMAC-SHA1, DES-EDE3-CBC or AES-256-CBC). Used to
// build test fixtures and dev tooling; merchants upload SAT-issued keys.
func EncryptSATPrivateKey(pkcs8, password []byte, aes256 bool) ([]byte, error) {
	salt := make([]byte, 8)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	const iter = 2048
	keyLen, alg, newCipher := 24, oidDESEDE3CBC, des.NewTripleDESCipher
	if aes256 {
		keyLen, alg, newCipher = 32, oidAES256CBC, aes.NewCipher
	}
	key, err := pbkdf2.Key(sha1.New, string(password), salt, iter, keyLen)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	block, err := newCipher(key)
	if err != nil {
		return nil, err
	}
	iv := make([]byte, block.BlockSize())
	if _, err := rand.Read(iv); err != nil {
		return nil, err
	}
	pad := block.BlockSize() - len(pkcs8)%block.BlockSize()
	buf := append(append([]byte(nil), pkcs8...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	defer clear(buf)
	enc := make([]byte, len(buf))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(enc, buf)

	kdfParams, err := asn1.Marshal(struct {
		Salt           []byte
		IterationCount int
	}{salt, iter})
	if err != nil {
		return nil, err
	}
	ivParam, err := asn1.Marshal(iv)
	if err != nil {
		return nil, err
	}
	pbes2, err := asn1.Marshal(pbes2Params{
		KeyDerivationFunc: algorithmIdentifier{Algorithm: oidPBKDF2, Parameters: asn1.RawValue{FullBytes: kdfParams}},
		EncryptionScheme:  algorithmIdentifier{Algorithm: alg, Parameters: asn1.RawValue{FullBytes: ivParam}},
	})
	if err != nil {
		return nil, err
	}
	return asn1.Marshal(encryptedPrivateKeyInfo{
		Algo:          algorithmIdentifier{Algorithm: oidPBES2, Parameters: asn1.RawValue{FullBytes: pbes2}},
		EncryptedData: enc,
	})
}

// CSDInfo is the validated metadata of an uploaded CSD.
type CSDInfo struct {
	Serial    string // SAT NoCertificado (20 digits)
	RFC       string // from the subject x500UniqueIdentifier
	Subject   string // subject common name
	ValidFrom time.Time
	ValidTo   time.Time
}

// CertificateRFC extracts the RFC of a SAT certificate: the first token of
// the subject x500UniqueIdentifier ("EKU9003173C9 / XIQB891116QE4" for a
// persona moral: company RFC / legal representative RFC).
func CertificateRFC(cert *x509.Certificate) string {
	for _, n := range cert.Subject.Names {
		if n.Type.Equal(oidX500UniqueID) {
			if v, ok := n.Value.(string); ok {
				first, _, _ := strings.Cut(v, "/")
				return strings.ToUpper(strings.TrimSpace(first))
			}
		}
	}
	return ""
}

// IsFIEL reports whether cert is an e.firma (FIEL) rather than a CSD: FIEL
// certificates are issued for authentication / encryption too (key usage
// keyAgreement / dataEncipherment, extended key usage emailProtection /
// clientAuth); a CSD only signs (digitalSignature + nonRepudiation) and
// carries the branch (sucursal) in OU.
func IsFIEL(cert *x509.Certificate) bool {
	if cert.KeyUsage&(x509.KeyUsageKeyAgreement|x509.KeyUsageDataEncipherment|x509.KeyUsageKeyEncipherment) != 0 {
		return true
	}
	for _, u := range cert.ExtKeyUsage {
		if u == x509.ExtKeyUsageEmailProtection || u == x509.ExtKeyUsageClientAuth {
			return true
		}
	}
	return false
}

// ValidateCSD checks an uploaded CSD: readable DER/PEM certificate, SAT .key
// decrypting with password, key matching the certificate, RSA, RFC equal to
// merchantRFC, valid at now and a CSD (not a FIEL). It returns the metadata
// and the certificate DER; the decrypted key is wiped before returning.
func ValidateCSD(cerBytes, keyBytes, password []byte, merchantRFC string, now time.Time) (CSDInfo, []byte, error) {
	certDER := pemOrDER(cerBytes, "CERTIFICATE")
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return CSDInfo{}, nil, csdErr(CSDReasonCertUnreadable)
	}
	pub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return CSDInfo{}, nil, csdErr(CSDReasonNotRSA)
	}
	plain, err := DecryptSATPrivateKey(keyBytes, password)
	if err != nil {
		return CSDInfo{}, nil, err
	}
	defer clear(plain)
	keyIfc, err := x509.ParsePKCS8PrivateKey(plain)
	if err != nil {
		return CSDInfo{}, nil, csdErr(CSDReasonWrongPassword)
	}
	priv, ok := keyIfc.(*rsa.PrivateKey)
	if !ok {
		return CSDInfo{}, nil, csdErr(CSDReasonNotRSA)
	}
	if !pub.Equal(&priv.PublicKey) {
		return CSDInfo{}, nil, csdErr(CSDReasonKeyMismatch)
	}
	if IsFIEL(cert) {
		return CSDInfo{}, nil, csdErr(CSDReasonFIEL)
	}
	rfc := CertificateRFC(cert)
	if rfc == "" || !strings.EqualFold(rfc, strings.TrimSpace(merchantRFC)) {
		return CSDInfo{}, nil, csdErr(CSDReasonRFCMismatch)
	}
	switch {
	case now.Before(cert.NotBefore):
		return CSDInfo{}, nil, csdErr(CSDReasonNotYetValid)
	case now.After(cert.NotAfter):
		return CSDInfo{}, nil, csdErr(CSDReasonExpired)
	}
	return CSDInfo{Serial: CertificateNumber(cert), RFC: rfc, Subject: cert.Subject.CommonName,
		ValidFrom: cert.NotBefore.UTC(), ValidTo: cert.NotAfter.UTC()}, certDER, nil
}

// NewCSDSignerFromSAT builds a signer from a SAT .cer and encrypted .key.
func NewCSDSignerFromSAT(certDER, keyDER, password []byte) (*CSDSigner, error) {
	plain, err := DecryptSATPrivateKey(keyDER, password)
	if err != nil {
		return nil, err
	}
	defer clear(plain)
	s, err := NewCSDSigner(pemOrDER(certDER, "CERTIFICATE"), plain)
	if err != nil {
		return nil, fmt.Errorf("csd signer: %w", err)
	}
	return s, nil
}
