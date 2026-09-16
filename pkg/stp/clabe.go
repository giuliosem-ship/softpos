package stp

import (
	"errors"
	"fmt"
	"unicode"
)

var ErrInvalidCLABE = errors.New("invalid CLABE format or check digit")

var CLABEWeights = [17]int{3, 7, 1, 3, 7, 1, 3, 7, 1, 3, 7, 1, 3, 7, 1, 3, 7}

func ComputeControlDigit(first17Digits string) (int, error) {
	if len(first17Digits) != 17 || !isNumeric(first17Digits) {
		return 0, ErrInvalidCLABE
	}

	sumProducts := 0
	for i, digitRune := range first17Digits {
		digit := int(digitRune - '0')
		sumProducts += (digit * CLABEWeights[i]) % 10
	}

	return (10 - (sumProducts % 10)) % 10, nil
}

func ValidateCLABE(clabe string) bool {
	if len(clabe) != 18 || !isNumeric(clabe) {
		return false
	}

	first17 := clabe[:17]
	expectedDigit, err := ComputeControlDigit(first17)
	if err != nil {
		return false
	}

	actualDigit := int(clabe[17] - '0')
	return actualDigit == expectedDigit
}

func GenerateCLABE(accountSeq int64, plaza string, bank string) (string, error) {
	if accountSeq < 0 || accountSeq > 99_999_999_999 {
		return "", ErrInvalidCLABE
	}

	if len(plaza) != 3 || !isNumeric(plaza) {
		return "", ErrInvalidCLABE
	}

	if len(bank) != 3 || !isNumeric(bank) {
		return "", ErrInvalidCLABE
	}

	accountSeqStr := fmt.Sprintf("%011d", accountSeq)
	first17 := bank + plaza + accountSeqStr

	controlDigit, err := ComputeControlDigit(first17)
	if err != nil {
		return "", err
	}

	return first17 + string(rune('0'+controlDigit)), nil
}

func isNumeric(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}
