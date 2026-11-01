package iso8583

import (
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
)

var (
	ErrMissingMandatoryEMVTag = errors.New("missing mandatory EMV tag for online authorization")
	ErrMalformedDE55TLV       = errors.New("malformed DE 55 BER-TLV data")
)

var mandatoryOnlineTags = []string{
	"9F26", // Application Cryptogram (ARQC)
	"9F27", // Cryptogram Information Data (CID)
	"9F10", // Issuer Application Data (IAD)
	"9F37", // Unpredictable Number
	"9F36", // Application Transaction Counter (ATC)
	"95",   // Terminal Verification Results (TVR)
	"9A",   // Transaction Date
	"9C",   // Transaction Type
	"9F02", // Amount, Authorized
	"5F2A", // Transaction Currency Code
}

// BuildDE55 serializes a map of uppercase hex EMV tags and their byte values into a BER-TLV stream.
func BuildDE55(tags map[string][]byte) ([]byte, error) {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, strings.ToUpper(k))
	}
	sort.Strings(keys)

	var buf []byte
	for _, tagHex := range keys {
		val := tags[tagHex]
		tagBytes, err := hex.DecodeString(tagHex)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid hex tag %q", ErrMalformedDE55TLV, tagHex)
		}

		buf = append(buf, tagBytes...)

		// Length encoding
		l := len(val)
		if l < 128 {
			buf = append(buf, byte(l))
		} else if l <= 255 {
			buf = append(buf, 0x81, byte(l))
		} else {
			buf = append(buf, 0x82, byte(l>>8), byte(l&0xFF))
		}

		buf = append(buf, val...)
	}

	return buf, nil
}

// ParseDE55 decodes a BER-TLV byte stream into a map of uppercase hex tag keys.
func ParseDE55(data []byte) (map[string][]byte, error) {
	if len(data) == 0 {
		return nil, ErrMalformedDE55TLV
	}

	tags := make(map[string][]byte)
	i := 0
	n := len(data)

	for i < n {
		// 1. Parse Tag
		tagStart := i
		b1 := data[i]
		i++

		if (b1 & 0x1F) == 0x1F {
			// Multi-byte tag
			for i < n {
				bNext := data[i]
				i++
				if (bNext & 0x80) == 0 {
					break
				}
			}
		}

		tagHex := strings.ToUpper(hex.EncodeToString(data[tagStart:i]))

		// 2. Parse Length
		if i >= n {
			return nil, ErrMalformedDE55TLV
		}

		lenByte := data[i]
		i++

		var length int
		if (lenByte & 0x80) == 0 {
			length = int(lenByte)
		} else {
			numLenBytes := int(lenByte & 0x7F)
			if numLenBytes == 0 || i+numLenBytes > n {
				return nil, ErrMalformedDE55TLV
			}
			for j := 0; j < numLenBytes; j++ {
				length = (length << 8) | int(data[i])
				i++
			}
		}

		// 3. Parse Value
		if i+length > n {
			return nil, ErrMalformedDE55TLV
		}

		val := make([]byte, length)
		copy(val, data[i:i+length])
		i += length

		tags[tagHex] = val
	}

	return tags, nil
}

// ValidateMandatoryOnlineTags checks that all required EMV tags for an online authorization ARQC exist.
func ValidateMandatoryOnlineTags(tags map[string][]byte) error {
	for _, tag := range mandatoryOnlineTags {
		val, exists := tags[tag]
		if !exists || len(val) == 0 {
			return fmt.Errorf("%w: missing tag %s", ErrMissingMandatoryEMVTag, tag)
		}
	}
	return nil
}
