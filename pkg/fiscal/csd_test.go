package fiscal_test

import (
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	"softpos/pkg/fiscal"
	"softpos/pkg/fiscal/fiscaltest"
)

func mustCSD(t *testing.T, o fiscaltest.CSDOptions) fiscaltest.CSD {
	t.Helper()
	c, err := fiscaltest.NewCSD(o)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDecryptSATPrivateKey_DES3AndAES(t *testing.T) {
	for _, aes := range []bool{false, true} {
		c := mustCSD(t, fiscaltest.CSDOptions{RFC: "EKU9003173C9", AES: aes})
		plain, err := fiscal.DecryptSATPrivateKey(c.EncryptedKey, c.Password)
		if err != nil {
			t.Fatalf("aes=%v: %v", aes, err)
		}
		if string(plain) != string(c.PlainKey) {
			t.Fatalf("aes=%v: decrypted key differs", aes)
		}
		if _, err := fiscal.DecryptSATPrivateKey(c.EncryptedKey, []byte("wrong-password")); fiscal.CSDReason(err) != fiscal.CSDReasonWrongPassword {
			t.Fatalf("aes=%v wrong password: %v", aes, err)
		}
		pemKey := pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: c.EncryptedKey})
		if _, err := fiscal.DecryptSATPrivateKey(pemKey, c.Password); err != nil {
			t.Fatalf("PEM key: %v", err)
		}
	}
	// A plaintext PKCS#8 key is not the SAT format.
	c := mustCSD(t, fiscaltest.CSDOptions{RFC: "EKU9003173C9"})
	if _, err := fiscal.DecryptSATPrivateKey(c.PlainKey, c.Password); fiscal.CSDReason(err) == "" {
		t.Fatalf("plaintext key must be rejected: %v", err)
	}
	if _, err := fiscal.DecryptSATPrivateKey([]byte("garbage"), c.Password); fiscal.CSDReason(err) != fiscal.CSDReasonKeyUnreadable {
		t.Fatalf("garbage: %v", err)
	}
}

func TestValidateCSD_Reasons(t *testing.T) {
	now := time.Now()
	good := mustCSD(t, fiscaltest.CSDOptions{RFC: "EKU9003173C9", Serial: "30001000000500003416"})
	info, certDER, err := fiscal.ValidateCSD(good.CertDER, good.EncryptedKey, good.Password, "eku9003173c9", now)
	if err != nil {
		t.Fatalf("valid CSD rejected: %v", err)
	}
	if info.Serial != "30001000000500003416" || info.RFC != "EKU9003173C9" || len(certDER) == 0 || !info.ValidTo.After(now) {
		t.Fatalf("info: %+v", info)
	}
	cert, _ := x509.ParseCertificate(certDER)
	if fiscal.CertificateNumber(cert) != "30001000000500003416" {
		t.Fatal("SAT NoCertificado must be the ASCII serial")
	}
	other := mustCSD(t, fiscaltest.CSDOptions{RFC: "EKU9003173C9"})
	pemCert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: good.CertDER})
	if _, _, err := fiscal.ValidateCSD(pemCert, good.EncryptedKey, good.Password, "EKU9003173C9", now); err != nil {
		t.Fatalf("PEM certificate: %v", err)
	}

	cases := []struct {
		name          string
		cer, key, pwd []byte
		rfc           string
		at            time.Time
		want          string
	}{
		{"wrong password", good.CertDER, good.EncryptedKey, []byte("nope"), "EKU9003173C9", now, fiscal.CSDReasonWrongPassword},
		{"rfc mismatch", good.CertDER, good.EncryptedKey, good.Password, "GODE561231GR8", now, fiscal.CSDReasonRFCMismatch},
		{"expired", good.CertDER, good.EncryptedKey, good.Password, "EKU9003173C9", now.Add(5 * 365 * 24 * time.Hour), fiscal.CSDReasonExpired},
		{"not yet valid", good.CertDER, good.EncryptedKey, good.Password, "EKU9003173C9", now.Add(-48 * time.Hour), fiscal.CSDReasonNotYetValid},
		{"key/cer mismatch", good.CertDER, other.EncryptedKey, other.Password, "EKU9003173C9", now, fiscal.CSDReasonKeyMismatch},
		{"cer garbage", []byte("not a cert"), good.EncryptedKey, good.Password, "EKU9003173C9", now, fiscal.CSDReasonCertUnreadable},
	}
	fiel := mustCSD(t, fiscaltest.CSDOptions{RFC: "EKU9003173C9", FIEL: true})
	cases = append(cases, struct {
		name          string
		cer, key, pwd []byte
		rfc           string
		at            time.Time
		want          string
	}{"FIEL instead of CSD", fiel.CertDER, fiel.EncryptedKey, fiel.Password, "EKU9003173C9", now, fiscal.CSDReasonFIEL})
	for _, c := range cases {
		if _, _, err := fiscal.ValidateCSD(c.cer, c.key, c.pwd, c.rfc, c.at); fiscal.CSDReason(err) != c.want {
			t.Errorf("%s: got %v, want %s", c.name, err, c.want)
		}
	}
}

func TestSealAndVerify_SignsTheCadenaOriginal(t *testing.T) {
	c := mustCSD(t, fiscaltest.CSDOptions{RFC: "EKU9003173C9"})
	signer, err := fiscal.NewCSDSignerFromSAT(c.CertDER, c.EncryptedKey, c.Password)
	if err != nil {
		t.Fatal(err)
	}
	comp := goldenVariants()[0].comp
	cadena, err := signer.Seal(comp)
	if err != nil {
		t.Fatal(err)
	}
	if cadena != fiscal.BuildCadenaOriginal(comp) || comp.NoCertificado != "30001000000500003416" {
		t.Fatalf("seal: %s / %s", cadena, comp.NoCertificado)
	}
	if err := fiscal.VerifySello(comp); err != nil {
		t.Fatalf("verify: %v", err)
	}
	comp.Total = "999.99"
	if err := fiscal.VerifySello(comp); err == nil {
		t.Fatal("a tampered comprobante must not verify")
	}
	if _, err := fiscal.NewCSDSigner(c.CertDER, mustCSD(t, fiscaltest.CSDOptions{}).PlainKey); err == nil {
		t.Fatal("signer must refuse a key of another certificate")
	}
}
