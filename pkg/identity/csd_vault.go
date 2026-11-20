package identity

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
)

var ErrCSDVault = errors.New("CSD cryptographic operation failed")

// EncryptedCSD is an envelope: fields sealed with a per-CSD DEK, and the DEK
// wrapped by a KeyProvider under KeyID. Envelopes without KeyID are the legacy
// format (DEK sealed with the master key using IV).
type EncryptedCSD struct {
	MerchantID        string
	KeyID             string `json:",omitempty"`
	EncryptedCER      []byte
	EncryptedKey      []byte
	EncryptedPassword []byte
	EncryptedDEK      []byte
	IV                []byte
}

type DecryptedCSD struct {
	MerchantID string
	CERBytes   []byte
	KeyBytes   []byte
	Password   string
}

type CSDVaultService struct {
	keys KeyProvider
}

// NewCSDVaultService builds a vault over a single static master key (key id "v1").
func NewCSDVaultService(masterKeyHex string) (*CSDVaultService, error) {
	p, err := NewStaticKeyProvider("v1", masterKeyHex, nil)
	if err != nil {
		return nil, ErrCSDVault
	}
	return &CSDVaultService{keys: p}, nil
}

// NewCSDVaultServiceWithProvider builds a vault whose DEKs are wrapped by p
// (env keyring or KMS).
func NewCSDVaultServiceWithProvider(p KeyProvider) (*CSDVaultService, error) {
	if p == nil {
		return nil, ErrCSDVault
	}
	return &CSDVaultService{keys: p}, nil
}

// sealWithUniqueNonce generates a fresh 12-byte random nonce per field and prepends it to the AES-GCM ciphertext.
func sealWithUniqueNonce(gcm cipher.AEAD, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, ErrCSDVault
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// openWithPrependedNonce extracts the prepended 12-byte nonce and authenticates/decrypts the AES-GCM payload.
func openWithPrependedNonce(gcm cipher.AEAD, sealed []byte) ([]byte, error) {
	nonceSize := gcm.NonceSize()
	if len(sealed) <= nonceSize {
		return nil, ErrCSDVault
	}
	nonce := sealed[:nonceSize]
	ciphertext := sealed[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, ErrCSDVault
	}
	return plaintext, nil
}

func (v *CSDVaultService) StoreCSD(merchantID string, cerBytes []byte, keyBytes []byte, password string) (EncryptedCSD, error) {
	if merchantID == "" || len(cerBytes) == 0 || len(keyBytes) == 0 || password == "" {
		return EncryptedCSD{}, ErrCSDVault
	}

	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return EncryptedCSD{}, ErrCSDVault
	}
	defer clear(dek)

	block, err := aes.NewCipher(dek)
	if err != nil {
		return EncryptedCSD{}, ErrCSDVault
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return EncryptedCSD{}, ErrCSDVault
	}

	// Each field gets its own cryptographically random 12-byte nonce (zero nonce reuse under DEK)
	encryptedCER, err := sealWithUniqueNonce(gcm, cerBytes)
	if err != nil {
		return EncryptedCSD{}, ErrCSDVault
	}
	encryptedKey, err := sealWithUniqueNonce(gcm, keyBytes)
	if err != nil {
		return EncryptedCSD{}, ErrCSDVault
	}
	encryptedPassword, err := sealWithUniqueNonce(gcm, []byte(password))
	if err != nil {
		return EncryptedCSD{}, ErrCSDVault
	}

	keyID, wrappedDEK, err := v.keys.WrapDEK(context.Background(), dek)
	if err != nil {
		return EncryptedCSD{}, ErrCSDVault
	}

	return EncryptedCSD{
		MerchantID:        merchantID,
		KeyID:             keyID,
		EncryptedCER:      encryptedCER,
		EncryptedKey:      encryptedKey,
		EncryptedPassword: encryptedPassword,
		EncryptedDEK:      wrappedDEK,
	}, nil
}

// unwrapDEK returns the plaintext DEK of an envelope (current or legacy format).
func (v *CSDVaultService) unwrapDEK(enc EncryptedCSD) ([]byte, error) {
	wrapped := enc.EncryptedDEK
	if enc.KeyID == "" { // legacy: IV stored apart from the sealed DEK
		if len(enc.IV) != 12 {
			return nil, ErrCSDVault
		}
		wrapped = append(append([]byte(nil), enc.IV...), enc.EncryptedDEK...)
	}
	dek, err := v.keys.UnwrapDEK(context.Background(), enc.KeyID, wrapped)
	if err != nil || len(dek) != 32 {
		return nil, ErrCSDVault
	}
	return dek, nil
}

// RewrapCSD re-wraps the DEK of enc with the provider's active key (key
// rotation / legacy migration). Field ciphertexts are untouched.
func (v *CSDVaultService) RewrapCSD(enc EncryptedCSD) (EncryptedCSD, error) {
	dek, err := v.unwrapDEK(enc)
	if err != nil {
		return EncryptedCSD{}, err
	}
	defer clear(dek)
	keyID, wrapped, err := v.keys.WrapDEK(context.Background(), dek)
	if err != nil {
		return EncryptedCSD{}, ErrCSDVault
	}
	enc.KeyID, enc.EncryptedDEK, enc.IV = keyID, wrapped, nil
	return enc, nil
}

func (v *CSDVaultService) DecryptCSDForSigning(enc EncryptedCSD) (DecryptedCSD, error) {
	dek, err := v.unwrapDEK(enc)
	if err != nil {
		return DecryptedCSD{}, ErrCSDVault
	}
	defer clear(dek)

	block, err := aes.NewCipher(dek)
	if err != nil {
		return DecryptedCSD{}, ErrCSDVault
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return DecryptedCSD{}, ErrCSDVault
	}

	cerBytes, err := openWithPrependedNonce(gcm, enc.EncryptedCER)
	if err != nil {
		return DecryptedCSD{}, ErrCSDVault
	}

	keyBytes, err := openWithPrependedNonce(gcm, enc.EncryptedKey)
	if err != nil {
		return DecryptedCSD{}, ErrCSDVault
	}

	passwordBytes, err := openWithPrependedNonce(gcm, enc.EncryptedPassword)
	if err != nil {
		return DecryptedCSD{}, ErrCSDVault
	}

	return DecryptedCSD{
		MerchantID: enc.MerchantID,
		CERBytes:   cerBytes,
		KeyBytes:   keyBytes,
		Password:   string(passwordBytes),
	}, nil
}
