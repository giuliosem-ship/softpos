package identity

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"testing"
)

func hexKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// legacyEnvelope reproduces the pre key-id format: DEK sealed with the master
// key under a separate IV, no AAD, no KeyID.
func legacyEnvelope(t *testing.T, masterHex string) EncryptedCSD {
	t.Helper()
	p, _ := NewStaticKeyProvider("tmp", masterHex, nil)
	v, _ := NewCSDVaultServiceWithProvider(p)
	enc, err := v.StoreCSD("m_legacy", []byte("cer"), []byte("key"), "pwd")
	if err != nil {
		t.Fatal(err)
	}
	dek, err := p.UnwrapDEK(context.Background(), enc.KeyID, enc.EncryptedDEK)
	if err != nil {
		t.Fatal(err)
	}
	master, _ := hex.DecodeString(masterHex)
	block, _ := aes.NewCipher(master)
	g, _ := cipher.NewGCM(block)
	iv := make([]byte, 12)
	_, _ = rand.Read(iv)
	enc.KeyID, enc.IV, enc.EncryptedDEK = "", iv, g.Seal(nil, iv, dek, nil)
	return enc
}

func TestStaticKeyProvider_RejectsInvalidKeys(t *testing.T) {
	for _, k := range []string{"", "zz", hex.EncodeToString(make([]byte, 16)), hex.EncodeToString(make([]byte, 33))} {
		if _, err := NewStaticKeyProvider("v1", k, nil); !errors.Is(err, ErrInvalidMasterKey) {
			t.Errorf("key %q: want ErrInvalidMasterKey, got %v", k, err)
		}
	}
	if _, err := NewStaticKeyProvider("v2", hexKey(t), map[string]string{"v1": "short"}); err == nil {
		t.Error("invalid previous key must be rejected")
	}
	ring, err := ParseKeyring(" v1:aa , v0:bb ,")
	if err != nil || ring["v1"] != "aa" || ring["v0"] != "bb" || len(ring) != 2 {
		t.Fatalf("ParseKeyring: %v %v", ring, err)
	}
	if _, err := ParseKeyring("nocolon"); err == nil {
		t.Error("malformed keyring must fail")
	}
}

func TestVault_EnvelopeCarriesKeyIDAndRotates(t *testing.T) {
	k1, k2 := hexKey(t), hexKey(t)
	p1, _ := NewStaticKeyProvider("v1", k1, nil)
	v1, _ := NewCSDVaultServiceWithProvider(p1)
	enc, err := v1.StoreCSD("m_rot", []byte("cer"), []byte("key"), "pwd")
	if err != nil || enc.KeyID != "v1" || len(enc.IV) != 0 {
		t.Fatalf("new envelopes must carry the key id: %+v %v", enc.KeyID, err)
	}

	// Rotation: v2 active, v1 retained -> old envelope still decrypts.
	p2, _ := NewStaticKeyProvider("v2", k2, map[string]string{"v1": k1})
	v2, _ := NewCSDVaultServiceWithProvider(p2)
	if dec, err := v2.DecryptCSDForSigning(enc); err != nil || string(dec.KeyBytes) != "key" {
		t.Fatalf("decrypt with previous key: %v", err)
	}
	rewrapped, err := v2.RewrapCSD(enc)
	if err != nil || rewrapped.KeyID != "v2" {
		t.Fatalf("rewrap: %+v %v", rewrapped.KeyID, err)
	}
	// After re-wrapping, v1 can be dropped.
	only2, _ := NewStaticKeyProvider("v2", k2, nil)
	v2only, _ := NewCSDVaultServiceWithProvider(only2)
	if dec, err := v2only.DecryptCSDForSigning(rewrapped); err != nil || dec.Password != "pwd" {
		t.Fatalf("decrypt rewrapped: %v", err)
	}
	if _, err := v2only.DecryptCSDForSigning(enc); !errors.Is(err, ErrCSDVault) {
		t.Fatalf("envelope under a dropped key must fail, got %v", err)
	}
	// The key id is authenticated (AAD): relabeling an envelope fails.
	relabeled := enc
	relabeled.KeyID = "v2"
	if _, err := v2.DecryptCSDForSigning(relabeled); !errors.Is(err, ErrCSDVault) {
		t.Fatalf("relabeled envelope must fail, got %v", err)
	}
}

func TestVault_LegacyEnvelopesKeepDecrypting(t *testing.T) {
	k := hexKey(t)
	legacy := legacyEnvelope(t, k)
	v, _ := NewCSDVaultService(k)
	dec, err := v.DecryptCSDForSigning(legacy)
	if err != nil || string(dec.CERBytes) != "cer" {
		t.Fatalf("legacy decrypt: %v", err)
	}
	migrated, err := v.RewrapCSD(legacy)
	if err != nil || migrated.KeyID != "v1" || migrated.IV != nil {
		t.Fatalf("legacy rewrap: %+v %v", migrated, err)
	}
	if _, err := v.DecryptCSDForSigning(migrated); err != nil {
		t.Fatalf("decrypt migrated: %v", err)
	}
	other, _ := NewCSDVaultService(hexKey(t))
	if _, err := other.DecryptCSDForSigning(legacy); !errors.Is(err, ErrCSDVault) {
		t.Fatal("legacy envelope must not open with another key")
	}
}

type fakeKMS struct{ key []byte }

func (f fakeKMS) Encrypt(_ context.Context, keyID string, pt []byte) ([]byte, error) {
	p := &StaticKeyProvider{activeID: keyID, keys: map[string][]byte{keyID: f.key}}
	_, ct, err := p.WrapDEK(context.Background(), pt)
	return ct, err
}

func (f fakeKMS) Decrypt(_ context.Context, keyID string, ct []byte) ([]byte, error) {
	p := &StaticKeyProvider{activeID: keyID, keys: map[string][]byte{keyID: f.key}}
	return p.UnwrapDEK(context.Background(), keyID, ct)
}

func TestKMSKeyProvider_WrapsThroughClient(t *testing.T) {
	key, _ := hex.DecodeString(hexKey(t))
	v, _ := NewCSDVaultServiceWithProvider(&KMSKeyProvider{Client: fakeKMS{key: key}, KeyID: "arn:aws:kms:us-east-2:000:key/test"})
	enc, err := v.StoreCSD("m_kms", []byte("cer"), []byte("key"), "pwd")
	if err != nil || enc.KeyID != "arn:aws:kms:us-east-2:000:key/test" {
		t.Fatalf("kms store: %+v %v", enc.KeyID, err)
	}
	if dec, err := v.DecryptCSDForSigning(enc); err != nil || dec.Password != "pwd" {
		t.Fatalf("kms decrypt: %v", err)
	}
	if _, _, err := (&KMSKeyProvider{}).WrapDEK(context.Background(), key); err == nil {
		t.Fatal("unconfigured KMS provider must fail")
	}
}
