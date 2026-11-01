package iso8583

import (
	"crypto/des"
	"crypto/subtle"
	"errors"
)

var (
	ErrInvalidKeyLength       = errors.New("invalid key length: key must be 16 bytes")
	ErrInvalidTransactionData = errors.New("transaction data cannot be empty")
	ErrInvalidResponseCode    = errors.New("response code (ARC) must be exactly 2 characters")
)

// ComputeARQC computes an 8-byte EMV Application Request Cryptogram (ARQC)
// using ISO/IEC 9797-1 Algorithm 3 (Retail MAC / ANSI X9.19 MAC) with Padding Method 2.
func ComputeARQC(sessionKey []byte, transactionData []byte) ([]byte, error) {
	if len(sessionKey) != 16 {
		return nil, ErrInvalidKeyLength
	}
	if len(transactionData) == 0 {
		return nil, ErrInvalidTransactionData
	}

	kL := sessionKey[:8]
	kR := sessionKey[8:]

	desL, err := des.NewCipher(kL)
	if err != nil {
		return nil, err
	}
	desR, err := des.NewCipher(kR)
	if err != nil {
		return nil, err
	}

	// ISO/IEC 9797-1 Padding Method 2 (0x80 followed by 0x00s up to 8-byte multiple)
	padLen := 8 - (len(transactionData) % 8)
	padded := make([]byte, len(transactionData)+padLen)
	copy(padded, transactionData)
	padded[len(transactionData)] = 0x80

	current := make([]byte, 8)
	numBlocks := len(padded) / 8

	for i := 0; i < numBlocks; i++ {
		block := padded[i*8 : (i+1)*8]
		for j := 0; j < 8; j++ {
			current[j] ^= block[j]
		}
		desL.Encrypt(current, current)
	}

	// Retail MAC final stage: decrypt with kR, then encrypt with kL
	desR.Decrypt(current, current)
	desL.Encrypt(current, current)

	return current, nil
}

// VerifyARQC validates that candidateARQC matches the expected cryptogram for the given transaction data.
func VerifyARQC(sessionKey []byte, transactionData []byte, candidateARQC []byte) (bool, error) {
	expected, err := ComputeARQC(sessionKey, transactionData)
	if err != nil {
		return false, err
	}

	if len(candidateARQC) != len(expected) {
		return false, nil
	}

	match := subtle.ConstantTimeCompare(expected, candidateARQC) == 1
	return match, nil
}

// GenerateARPC generates an 8-byte Application Response Cryptogram (ARPC) per EMV Method 1.
// ARPC = 3DES(sessionKey, ARQC XOR (ARC || 0x00 * 6)).
func GenerateARPC(sessionKey []byte, arqc []byte, responseCode string) ([]byte, error) {
	if len(sessionKey) != 16 {
		return nil, ErrInvalidKeyLength
	}
	if len(arqc) != 8 {
		return nil, errors.New("ARQC must be 8 bytes")
	}
	if len(responseCode) != 2 {
		return nil, ErrInvalidResponseCode
	}

	kL := sessionKey[:8]
	kR := sessionKey[8:]

	desL, err := des.NewCipher(kL)
	if err != nil {
		return nil, err
	}
	desR, err := des.NewCipher(kR)
	if err != nil {
		return nil, err
	}

	// Pad 2-character response code with 6 zero bytes
	paddedARC := make([]byte, 8)
	copy(paddedARC, []byte(responseCode))

	arpc := make([]byte, 8)
	for i := 0; i < 8; i++ {
		arpc[i] = arqc[i] ^ paddedARC[i]
	}

	// 3DES: Encrypt(kL) -> Decrypt(kR) -> Encrypt(kL)
	desL.Encrypt(arpc, arpc)
	desR.Decrypt(arpc, arpc)
	desL.Encrypt(arpc, arpc)

	return arpc, nil
}
