package identity

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
)

var ErrCSDVault = errors.New("CSD cryptographic operation failed")

type EncryptedCSD struct {
	MerchantID        string
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
	masterKey []byte
}

func NewCSDVaultService(masterKeyHex string) (*CSDVaultService, error) {
	masterKey, err := hex.DecodeString(masterKeyHex)
	if err != nil {
		return nil, ErrCSDVault
	}
	if len(masterKey) != 32 {
		return nil, ErrCSDVault
	}
	return &CSDVaultService{masterKey: masterKey}, nil
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

	// Dedicated 12-byte IV used exclusively for wrapping the DEK with the master key
	dekIV := make([]byte, 12)
	if _, err := rand.Read(dekIV); err != nil {
		return EncryptedCSD{}, ErrCSDVault
	}

	blockMaster, err := aes.NewCipher(v.masterKey)
	if err != nil {
		return EncryptedCSD{}, ErrCSDVault
	}
	gcmMaster, err := cipher.NewGCM(blockMaster)
	if err != nil {
		return EncryptedCSD{}, ErrCSDVault
	}

	encryptedDEK := gcmMaster.Seal(nil, dekIV, dek, nil)

	return EncryptedCSD{
		MerchantID:        merchantID,
		EncryptedCER:      encryptedCER,
		EncryptedKey:      encryptedKey,
		EncryptedPassword: encryptedPassword,
		EncryptedDEK:      encryptedDEK,
		IV:                dekIV,
	}, nil
}

func (v *CSDVaultService) DecryptCSDForSigning(enc EncryptedCSD) (DecryptedCSD, error) {
	blockMaster, err := aes.NewCipher(v.masterKey)
	if err != nil {
		return DecryptedCSD{}, ErrCSDVault
	}
	gcmMaster, err := cipher.NewGCM(blockMaster)
	if err != nil {
		return DecryptedCSD{}, ErrCSDVault
	}
	if len(enc.IV) != gcmMaster.NonceSize() {
		return DecryptedCSD{}, ErrCSDVault
	}

	dek, err := gcmMaster.Open(nil, enc.IV, enc.EncryptedDEK, nil)
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
