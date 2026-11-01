package crypto

import (
	"crypto/des"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrInvalidZPKLength = errors.New("invalid ZPK length, must be 16 bytes")
	ErrInvalidPAN       = errors.New("invalid PAN: must be 13 to 19 numeric digits")
	ErrInvalidPINLength = errors.New("invalid PIN length: must be between 4 and 12 digits")
	ErrInvalidPINFormat = errors.New("PIN must contain only numeric digits")
)

func cleanAndValidatePAN(pan string) (string, error) {
	clean := strings.TrimSpace(pan)
	clean = strings.ReplaceAll(clean, " ", "")
	clean = strings.ReplaceAll(clean, "-", "")

	if len(clean) < 13 || len(clean) > 19 {
		return "", ErrInvalidPAN
	}
	for _, c := range clean {
		if c < '0' || c > '9' {
			return "", ErrInvalidPAN
		}
	}
	return clean, nil
}

// FormatIso0PinBlock formats plaintext PIN and PAN into an ISO 9564-1 Format 0 PIN block.
// All temporary buffers are strictly zeroized.
func FormatIso0PinBlock(pin string, pan string) ([]byte, error) {
	if len(pin) < 4 || len(pin) > 12 {
		return nil, ErrInvalidPINLength
	}
	for _, c := range pin {
		if c < '0' || c > '9' {
			return nil, ErrInvalidPINFormat
		}
	}

	cleanPan, err := cleanAndValidatePAN(pan)
	if err != nil {
		return nil, err
	}

	pinField := make([]byte, 8)
	panField := make([]byte, 8)
	defer func() {
		ZeroBytes(pinField)
		ZeroBytes(panField)
	}()

	// 1. PIN Field: 0 || L || PIN || F...
	var pinHex strings.Builder
	pinHex.WriteString(fmt.Sprintf("0%X%s", len(pin), pin))
	for pinHex.Len() < 16 {
		pinHex.WriteString("F")
	}
	pinStr := pinHex.String()
	for i := 0; i < 8; i++ {
		var b byte
		fmt.Sscanf(pinStr[i*2:i*2+2], "%02X", &b)
		pinField[i] = b
	}

	// 2. PAN Field: 0000 || 12 rightmost digits excluding check digit
	pan12 := cleanPan[len(cleanPan)-13 : len(cleanPan)-1]
	panStr := "0000" + pan12
	for i := 0; i < 8; i++ {
		var b byte
		fmt.Sscanf(panStr[i*2:i*2+2], "%02X", &b)
		panField[i] = b
	}

	// 3. XOR PIN Field with PAN Field
	pinBlock := make([]byte, 8)
	for i := 0; i < 8; i++ {
		pinBlock[i] = pinField[i] ^ panField[i]
	}

	return pinBlock, nil
}

// ExtractPinFromIso0 extracts the cleartext PIN from an ISO 9564-1 Format 0 PIN block.
func ExtractPinFromIso0(pinBlock []byte, pan string) (string, error) {
	if len(pinBlock) != 8 {
		return "", ErrInvalidBlockLength
	}

	cleanPan, err := cleanAndValidatePAN(pan)
	if err != nil {
		return "", err
	}

	panField := make([]byte, 8)
	defer ZeroBytes(panField)

	pan12 := cleanPan[len(cleanPan)-13 : len(cleanPan)-1]
	panStr := "0000" + pan12
	for i := 0; i < 8; i++ {
		var b byte
		fmt.Sscanf(panStr[i*2:i*2+2], "%02X", &b)
		panField[i] = b
	}

	pinField := make([]byte, 8)
	defer ZeroBytes(pinField)
	for i := 0; i < 8; i++ {
		pinField[i] = pinBlock[i] ^ panField[i]
	}

	// Nibble 0 must be 0
	if (pinField[0] >> 4) != 0 {
		return "", errors.New("invalid ISO Format 0 PIN block header")
	}

	pinLen := int(pinField[0] & 0x0F)
	if pinLen < 4 || pinLen > 12 {
		return "", ErrInvalidPINLength
	}

	var pinBuilder strings.Builder
	for i := 0; i < pinLen; i++ {
		nibbleIdx := 2 + i
		byteIdx := nibbleIdx / 2
		var nibble byte
		if nibbleIdx%2 == 0 {
			nibble = pinField[byteIdx] >> 4
		} else {
			nibble = pinField[byteIdx] & 0x0F
		}
		if nibble > 9 {
			return "", ErrInvalidPINFormat
		}
		pinBuilder.WriteString(fmt.Sprintf("%d", nibble))
	}

	return pinBuilder.String(), nil
}

// TranslatePinBlock decrypts an incoming ISO 9564 Format 0 PIN block with DUKPT PEK,
// verifies PAN format, and re-encrypts under switch Zone PIN Key (ZPK).
// All intermediate sensitive buffers are zeroized upon function return.
func TranslatePinBlock(incomingBlock []byte, ksn []byte, bdk []byte, zpk []byte, pan string) ([]byte, error) {
	if len(incomingBlock) != 8 {
		return nil, ErrInvalidBlockLength
	}
	if len(ksn) != 10 {
		return nil, ErrInvalidKSNLength
	}
	if len(bdk) != 16 {
		return nil, ErrInvalidBDKLength
	}
	if len(zpk) != 16 {
		return nil, ErrInvalidZPKLength
	}

	if _, err := cleanAndValidatePAN(pan); err != nil {
		return nil, err
	}

	// 1. Decrypt incoming PIN block using DUKPT PEK
	plainPinBlock, err := DecryptPinBlock(incomingBlock, ksn, bdk)
	if err != nil {
		return nil, err
	}
	defer ZeroBytes(plainPinBlock)

	// 2. Re-encrypt under bank switch ZPK
	tripleZPK := append(zpk, zpk[:8]...)
	defer ZeroBytes(tripleZPK)

	cipherZPK, err := des.NewTripleDESCipher(tripleZPK)
	if err != nil {
		return nil, err
	}

	translatedBlock := make([]byte, 8)
	cipherZPK.Encrypt(translatedBlock, plainPinBlock)

	return translatedBlock, nil
}
