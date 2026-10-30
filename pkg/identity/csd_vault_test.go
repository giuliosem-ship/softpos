package identity

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"testing"
)

func randomHex(t *testing.T, n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("failed to read random: %v", err)
	}
	return hex.EncodeToString(b)
}

func TestStoreAndDecryptCSD(t *testing.T) {
	masterKey := randomHex(t, 32)
	vault, err := NewCSDVaultService(masterKey)
	if err != nil {
		t.Fatalf("failed to init vault: %v", err)
	}

	merchantID := "merch_oxxo_01"
	dummyCER := []byte("-----BEGIN CERTIFICATE-----\nMIIC...DUMMY_CSD_CER\n-----END CERTIFICATE-----")
	dummyKey := []byte("-----BEGIN ENCRYPTED PRIVATE KEY-----\nMIIF...DUMMY_CSD_KEY\n-----END ENCRYPTED PRIVATE KEY-----")
	password := "SuperSecretCSDPassword123!"

	// 1. Store and encrypt
	encrypted, err := vault.StoreCSD(merchantID, dummyCER, dummyKey, password)
	if err != nil {
		t.Fatalf("unexpected error storing CSD: %v", err)
	}

	if encrypted.MerchantID != merchantID {
		t.Errorf("expected merchant ID %s, got %s", merchantID, encrypted.MerchantID)
	}
	if bytes.Contains(encrypted.EncryptedCER, dummyCER) {
		t.Errorf("plaintext CER found in ciphertext")
	}
	if bytes.Contains(encrypted.EncryptedKey, dummyKey) {
		t.Errorf("plaintext Key found in ciphertext")
	}
	if bytes.Contains(encrypted.EncryptedPassword, []byte(password)) {
		t.Errorf("plaintext password found in ciphertext")
	}

	// 2. Decrypt in memory for signing
	decrypted, err := vault.DecryptCSDForSigning(encrypted)
	if err != nil {
		t.Fatalf("unexpected error decrypting CSD: %v", err)
	}

	if decrypted.MerchantID != merchantID {
		t.Errorf("expected merchant ID %s, got %s", merchantID, decrypted.MerchantID)
	}
	if !bytes.Equal(decrypted.CERBytes, dummyCER) {
		t.Errorf("CER bytes mismatch after decryption")
	}
	if !bytes.Equal(decrypted.KeyBytes, dummyKey) {
		t.Errorf("Key bytes mismatch after decryption")
	}
	if decrypted.Password != password {
		t.Errorf("password mismatch: expected %s, got %s", password, decrypted.Password)
	}
}

func TestDecryptWithWrongMasterKeyFails(t *testing.T) {
	masterKey1 := randomHex(t, 32)
	vault1, _ := NewCSDVaultService(masterKey1)

	encrypted, err := vault1.StoreCSD("merch_02", []byte("cer"), []byte("key"), "pwd")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Attacker tries to decrypt with a different master key
	masterKey2 := randomHex(t, 32)
	vault2, _ := NewCSDVaultService(masterKey2)

	_, err = vault2.DecryptCSDForSigning(encrypted)
	if err != ErrCSDVault {
		t.Errorf("expected ErrCSDVault when decrypting with wrong key, got %v", err)
	}
}

func TestTamperedPayloadFails(t *testing.T) {
	masterKey := randomHex(t, 32)
	vault, _ := NewCSDVaultService(masterKey)

	encrypted, _ := vault.StoreCSD("merch_03", []byte("cer"), []byte("key"), "pwd")

	// Tamper with encrypted key bytes
	tamperedKey := make([]byte, len(encrypted.EncryptedKey))
	copy(tamperedKey, encrypted.EncryptedKey)
	tamperedKey[0] ^= 0xFF

	tampered := EncryptedCSD{
		MerchantID:        encrypted.MerchantID,
		EncryptedCER:      encrypted.EncryptedCER,
		EncryptedKey:      tamperedKey,
		EncryptedPassword: encrypted.EncryptedPassword,
		EncryptedDEK:      encrypted.EncryptedDEK,
		IV:                encrypted.IV,
	}

	_, err := vault.DecryptCSDForSigning(tampered)
	if err != ErrCSDVault {
		t.Errorf("expected ErrCSDVault when decrypting tampered payload, got %v", err)
	}
}

func TestEmptyCSDInputsRaise(t *testing.T) {
	masterKey := randomHex(t, 32)
	vault, _ := NewCSDVaultService(masterKey)

	_, err := vault.StoreCSD("", []byte("cer"), []byte("key"), "pwd")
	if err == nil {
		t.Errorf("expected error for empty merchantID")
	}

	_, err = vault.StoreCSD("m1", []byte(""), []byte("key"), "pwd")
	if err == nil {
		t.Errorf("expected error for empty CER")
	}

	_, err = vault.StoreCSD("m1", []byte("cer"), []byte(""), "pwd")
	if err == nil {
		t.Errorf("expected error for empty Key")
	}

	_, err = vault.StoreCSD("m1", []byte("cer"), []byte("key"), "")
	if err == nil {
		t.Errorf("expected error for empty password")
	}
}

func TestUniqueNoncesPerField_NoIVReuse(t *testing.T) {
	masterKey := randomHex(t, 32)
	vault, _ := NewCSDVaultService(masterKey)

	samePayload := "IDENTICAL_SECRET_PAYLOAD_BLOCK_12345"
	encrypted, err := vault.StoreCSD("merch_nonce_check", []byte(samePayload), []byte(samePayload), samePayload)
	if err != nil {
		t.Fatalf("StoreCSD failed: %v", err)
	}

	cerNonce := encrypted.EncryptedCER[:12]
	keyNonce := encrypted.EncryptedKey[:12]
	pwdNonce := encrypted.EncryptedPassword[:12]

	if bytes.Equal(cerNonce, keyNonce) || bytes.Equal(cerNonce, pwdNonce) || bytes.Equal(keyNonce, pwdNonce) {
		t.Fatalf("CRITICAL: AES-GCM nonce reuse detected across fields! cer=%x key=%x pwd=%x", cerNonce, keyNonce, pwdNonce)
	}
	if bytes.Equal(cerNonce, encrypted.IV) || bytes.Equal(keyNonce, encrypted.IV) || bytes.Equal(pwdNonce, encrypted.IV) {
		t.Fatalf("CRITICAL: DEK IV reused for field encryption!")
	}
	if bytes.Equal(encrypted.EncryptedCER, encrypted.EncryptedKey) {
		t.Fatalf("identical plaintexts produced identical ciphertexts")
	}
}

