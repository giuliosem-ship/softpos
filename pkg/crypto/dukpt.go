package crypto

import (
	"crypto/des"
	"errors"
)

var (
	ErrInvalidBDKLength   = errors.New("invalid BDK length, must be 16 bytes")
	ErrInvalidKSNLength   = errors.New("invalid KSN length, must be 10 bytes")
	ErrInvalidBlockLength = errors.New("invalid block length, must be 8 bytes")
)

var (
	bdkMask = []byte{0xC0, 0xC0, 0xC0, 0xC0, 0x00, 0x00, 0x00, 0x00, 0xC0, 0xC0, 0xC0, 0xC0, 0x00, 0x00, 0x00, 0x00}
	pekMask = []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xFF, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xFF}
)

// ZeroBytes cleans sensitive cryptographic key and PIN buffers from memory.
func ZeroBytes(buf []byte) {
	for i := range buf {
		buf[i] = 0
	}
}

func des3EncryptECB(key, plaintext []byte) ([]byte, error) {
	tripleKey := append(key, key[:8]...)
	defer ZeroBytes(tripleKey)

	block, err := des.NewTripleDESCipher(tripleKey)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 8)
	block.Encrypt(out, plaintext)
	return out, nil
}

func des3DecryptECB(key, ciphertext []byte) ([]byte, error) {
	tripleKey := append(key, key[:8]...)
	defer ZeroBytes(tripleKey)

	block, err := des.NewTripleDESCipher(tripleKey)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 8)
	block.Decrypt(out, ciphertext)
	return out, nil
}

func desEncrypt(key, plaintext []byte) ([]byte, error) {
	block, err := des.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 8)
	block.Encrypt(out, plaintext)
	return out, nil
}

// DeriveIPEK calculates the Initial PIN Encryption Key (IPEK) per ANSI X9.24.
func DeriveIPEK(bdk []byte, ksn []byte) ([]byte, error) {
	if len(bdk) != 16 {
		return nil, ErrInvalidBDKLength
	}
	if len(ksn) != 10 {
		return nil, ErrInvalidKSNLength
	}

	ksn8 := make([]byte, 8)
	copy(ksn8, ksn[:8])
	ksn8[7] &= 0xE0

	ipekL, err := des3EncryptECB(bdk, ksn8)
	if err != nil {
		return nil, err
	}

	bdkMod := make([]byte, 16)
	defer ZeroBytes(bdkMod)
	for i := 0; i < 16; i++ {
		bdkMod[i] = bdk[i] ^ bdkMask[i]
	}

	ipekR, err := des3EncryptECB(bdkMod, ksn8)
	if err != nil {
		return nil, err
	}

	ipek := append(ipekL, ipekR...)
	return ipek, nil
}

// ExtractTransactionCounter extracts the 21-bit transaction counter from a 10-byte KSN.
func ExtractTransactionCounter(ksn []byte) (uint32, error) {
	if len(ksn) != 10 {
		return 0, ErrInvalidKSNLength
	}
	counter := (uint32(ksn[7]&0x1F) << 16) | (uint32(ksn[8]) << 8) | uint32(ksn[9])
	return counter, nil
}

func encryptRegister(curKey, reg []byte) ([]byte, error) {
	curKeyL := curKey[:8]
	curKeyR := curKey[8:]

	d := make([]byte, 8)
	for i := 0; i < 8; i++ {
		d[i] = curKeyR[i] ^ reg[i]
	}

	enc, err := desEncrypt(curKeyL, d)
	if err != nil {
		return nil, err
	}

	res := make([]byte, 8)
	for i := 0; i < 8; i++ {
		res[i] = curKeyR[i] ^ enc[i]
	}
	return res, nil
}

func generateKey(curKey, reg []byte) ([]byte, error) {
	maskedKey := make([]byte, 16)
	defer ZeroBytes(maskedKey)
	for i := 0; i < 16; i++ {
		maskedKey[i] = curKey[i] ^ bdkMask[i]
	}

	left, err := encryptRegister(maskedKey, reg)
	if err != nil {
		return nil, err
	}
	right, err := encryptRegister(curKey, reg)
	if err != nil {
		return nil, err
	}

	res := make([]byte, 16)
	copy(res[:8], left)
	copy(res[8:], right)
	return res, nil
}

// DeriveTransactionKey calculates the current transaction key from IPEK and KSN.
func DeriveTransactionKey(ipek []byte, ksn []byte) ([]byte, error) {
	if len(ipek) != 16 {
		return nil, ErrInvalidBDKLength
	}
	if len(ksn) != 10 {
		return nil, ErrInvalidKSNLength
	}

	counter, err := ExtractTransactionCounter(ksn)
	if err != nil {
		return nil, err
	}

	ksnReg := make([]byte, 8)
	copy(ksnReg, ksn[2:10])
	ksnReg[5] &= 0xE0
	ksnReg[6] = 0x00
	ksnReg[7] = 0x00

	curKey := make([]byte, 16)
	copy(curKey, ipek)

	shiftReg := uint32(0x100000)
	for shiftReg > 0 {
		if (counter & shiftReg) > 0 {
			ksnReg[5] |= byte((shiftReg >> 16) & 0x1F)
			ksnReg[6] |= byte((shiftReg >> 8) & 0xFF)
			ksnReg[7] |= byte(shiftReg & 0xFF)

			nextKey, genErr := generateKey(curKey, ksnReg)
			if genErr != nil {
				ZeroBytes(curKey)
				return nil, genErr
			}
			ZeroBytes(curKey)
			curKey = nextKey
		}
		shiftReg >>= 1
	}

	return curKey, nil
}

// DerivePEK calculates the PIN Encryption Key (PEK) directly from BDK and KSN.
func DerivePEK(bdk []byte, ksn []byte) ([]byte, error) {
	ipek, err := DeriveIPEK(bdk, ksn)
	if err != nil {
		return nil, err
	}
	defer ZeroBytes(ipek)

	sessionKey, err := DeriveTransactionKey(ipek, ksn)
	if err != nil {
		return nil, err
	}
	defer ZeroBytes(sessionKey)

	pek := make([]byte, 16)
	for i := 0; i < 16; i++ {
		pek[i] = sessionKey[i] ^ pekMask[i]
	}
	return pek, nil
}

// EncryptPinBlock encrypts an 8-byte PIN block under the transaction PEK.
func EncryptPinBlock(pinBlock []byte, ksn []byte, bdk []byte) ([]byte, error) {
	if len(pinBlock) != 8 {
		return nil, ErrInvalidBlockLength
	}

	pek, err := DerivePEK(bdk, ksn)
	if err != nil {
		return nil, err
	}
	defer ZeroBytes(pek)

	return des3EncryptECB(pek, pinBlock)
}

// DecryptPinBlock decrypts an 8-byte PIN block using the transaction PEK.
func DecryptPinBlock(encrypted []byte, ksn []byte, bdk []byte) ([]byte, error) {
	if len(encrypted) != 8 {
		return nil, ErrInvalidBlockLength
	}

	pek, err := DerivePEK(bdk, ksn)
	if err != nil {
		return nil, err
	}
	defer ZeroBytes(pek)

	return des3DecryptECB(pek, encrypted)
}
