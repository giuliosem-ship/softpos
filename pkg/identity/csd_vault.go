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
	MerchantID  string
	CERBytes    []byte
	KeyBytes    []byte
	Password    string
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

func (v *CSDVaultService) StoreCSD(merchantID string, cerBytes []byte, keyBytes []byte, password string) (EncryptedCSD, error) {
	if merchantID == "" || len(cerBytes) == 0 || len(keyBytes) == 0 || password == "" {
		return EncryptedCSD{}, ErrCSDVault
	}

	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return EncryptedCSD{}, ErrCSDVault
	}

	iv := make([]byte, 12)
	if _, err := rand.Read(iv); err != nil {
		return EncryptedCSD{}, ErrCSDVault
	}

	block, err := aes.NewCipher(dek)
	if err != nil {
		return EncryptedCSD{}, ErrCSDVault
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return EncryptedCSD{}, ErrCSDVault
	}

	encryptedCER := gcm.Seal(nil, iv, cerBytes, nil)
	encryptedKey := gcm.Seal(nil, iv, keyBytes, nil)
	encryptedPassword := gcm.Seal(nil, iv, []byte(password), nil)

	blockMaster, err := aes.NewCipher(v.masterKey)
	if err != nil {
		return EncryptedCSD{}, ErrCSDVault
	}
	gcmMaster, err := cipher.NewGCM(blockMaster)
	if err != nil {
		return EncryptedCSD{}, ErrCSDVault
	}

	encryptedDEK := gcmMaster.Seal(nil, iv, dek, nil)

	return EncryptedCSD{
		MerchantID:        merchantID,
		EncryptedCER:      encryptedCER,
		EncryptedKey:      encryptedKey,
		EncryptedPassword: encryptedPassword,
		EncryptedDEK:      encryptedDEK,
		IV:                iv,
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

	dek, err := gcmMaster.Open(nil, enc.IV, enc.EncryptedDEK, nil)
	if err != nil {
		return DecryptedCSD{}, ErrCSDVault
	}

	block, err := aes.NewCipher(dek)
	if err != nil {
		return DecryptedCSD{}, ErrCSDVault
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return DecryptedCSD{}, ErrCSDVault
	}

	cerBytes, err := gcm.Open(nil, enc.IV, enc.EncryptedCER, nil)
	if err != nil {
		return DecryptedCSD{}, ErrCSDVault
	}

	keyBytes, err := gcm.Open(nil, enc.IV, enc.EncryptedKey, nil)
	if err != nil {
		return DecryptedCSD{}, ErrCSDVault
	}

	passwordBytes, err := gcm.Open(nil, enc.IV, enc.EncryptedPassword, nil)
	if err != nil {
		return DecryptedCSD{}, ErrCSDVault
	}

	return DecryptedCSD{
		MerchantID:  enc.MerchantID,
		CERBytes:    cerBytes,
		KeyBytes:    keyBytes,
		Password:    string(passwordBytes),
	}, nil
}
