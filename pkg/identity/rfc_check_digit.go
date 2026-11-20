package identity

import "strings"

// rfcCheckDigitAlphabet is the SAT value table for the RFC verification
// digit ("dígito verificador" of the homoclave): '0'..'9' -> 0..9,
// 'A'..'N' -> 10..23, '&' -> 24, 'O'..'Z' -> 25..36, ' ' -> 37, 'Ñ' -> 38.
const rfcCheckDigitAlphabet = "0123456789ABCDEFGHIJKLMN&OPQRSTUVWXYZ Ñ"

// RFCCheckDigit computes the SAT verification digit of the first 12 (persona
// física) or 11 (persona moral, padded with a leading space) characters of
// an RFC. ok=false when a character is outside the SAT alphabet.
func RFCCheckDigit(rfc string) (digit rune, ok bool) {
	r := []rune(strings.ToUpper(strings.TrimSpace(rfc)))
	switch len(r) {
	case 12, 13:
		r = r[:len(r)-1] // drop the digit being verified
	case 11:
	default:
		return 0, false
	}
	if len(r) == 11 { // persona moral: 3-letter name part
		r = append([]rune{' '}, r...)
	}
	alphabet := []rune(rfcCheckDigitAlphabet)
	sum := 0
	for i, c := range r {
		v := -1
		for j, a := range alphabet {
			if a == c {
				v = j
				break
			}
		}
		if v < 0 {
			return 0, false
		}
		sum += v * (13 - i)
	}
	switch rem := sum % 11; rem {
	case 0:
		return '0', true
	case 1:
		return 'A', true
	default:
		return rune('0' + 11 - rem), true
	}
}

// ValidateRFCWithCheckDigit reports whether rfc has a valid SAT format
// (ValidateRFC) and a correct verification digit. The generic RFCs
// (XAXX010101000, XEXX010101000) are not merchant RFCs and are rejected.
func ValidateRFCWithCheckDigit(rfc string) bool {
	rfc = strings.ToUpper(strings.TrimSpace(rfc))
	if rfc == "XAXX010101000" || rfc == "XEXX010101000" || !ValidateRFC(rfc) {
		return false
	}
	want, ok := RFCCheckDigit(rfc)
	if !ok {
		return false
	}
	r := []rune(rfc)
	return r[len(r)-1] == want
}
