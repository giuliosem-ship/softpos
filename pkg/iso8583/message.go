package iso8583

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
)

var (
	ErrInvalidMessage = errors.New("invalid or malformed ISO 8583 message")
	ErrFieldNotFound  = errors.New("field not present in message")
)

type FieldType int

const (
	TypeFixed FieldType = iota
	TypeLLVAR
	TypeLLLVAR
)

type FieldDef struct {
	Length int
	Type   FieldType
}

var fieldDefs = map[int]FieldDef{
	2:  {Length: 19, Type: TypeLLVAR},
	3:  {Length: 6, Type: TypeFixed},
	4:  {Length: 12, Type: TypeFixed},
	11: {Length: 6, Type: TypeFixed},
	12: {Length: 6, Type: TypeFixed},
	13: {Length: 4, Type: TypeFixed},
	22: {Length: 3, Type: TypeFixed},
	38: {Length: 6, Type: TypeFixed},
	39: {Length: 2, Type: TypeFixed},
	41: {Length: 8, Type: TypeFixed},
	42: {Length: 15, Type: TypeFixed},
	49: {Length: 3, Type: TypeFixed},
	52: {Length: 16, Type: TypeFixed},
	55: {Length: 999, Type: TypeLLLVAR},
	70: {Length: 3, Type: TypeFixed},
}

// Message represents an ISO 8583:1987 / 1993 financial message.
type Message struct {
	MTI    string
	Fields map[int]string
}

// NewMessage constructs an empty ISO 8583 message with the specified MTI.
func NewMessage(mti string) *Message {
	return &Message{
		MTI:    mti,
		Fields: make(map[int]string),
	}
}

// Set stores a data element value by field number.
func (m *Message) Set(field int, val string) {
	m.Fields[field] = val
}

// Get retrieves a data element value by field number.
func (m *Message) Get(field int) (string, bool) {
	val, ok := m.Fields[field]
	return val, ok
}

// Pack serializes the Message into standard ISO 8583 wire format (MTI + Bitmaps + Fields).
func (m *Message) Pack() ([]byte, error) {
	if len(m.MTI) != 4 {
		return nil, ErrInvalidMessage
	}

	hasSecondary := false
	for de := range m.Fields {
		if de > 64 {
			hasSecondary = true
			break
		}
	}

	bitmapLen := 8
	if hasSecondary {
		bitmapLen = 16
	}
	bitmap := make([]byte, bitmapLen)

	if hasSecondary {
		// Set bit 1 for secondary bitmap presence
		bitmap[0] |= 0x80
	}

	for de := range m.Fields {
		if de < 2 || de > bitmapLen*8 {
			continue
		}
		byteIndex := (de - 1) / 8
		bitIndex := 7 - ((de - 1) % 8)
		bitmap[byteIndex] |= (1 << bitIndex)
	}

	buf := make([]byte, 0, 128)
	buf = append(buf, []byte(m.MTI)...)
	buf = append(buf, bitmap...)

	// Sort fields in ascending numerical order
	keys := make([]int, 0, len(m.Fields))
	for k := range m.Fields {
		keys = append(keys, k)
	}
	sort.Ints(keys)

	for _, de := range keys {
		val := m.Fields[de]
		def, ok := fieldDefs[de]
		if !ok {
			// Fallback: default to LLVAR for uncataloged fields
			def = FieldDef{Length: len(val), Type: TypeLLVAR}
		}

		switch def.Type {
		case TypeFixed:
			if len(val) != def.Length {
				// Pad or format to exact length
				val = fmt.Sprintf("%*s", def.Length, val)
				if len(val) > def.Length {
					val = val[:def.Length]
				}
			}
			buf = append(buf, []byte(val)...)
		case TypeLLVAR:
			lenPrefix := fmt.Sprintf("%02d", len(val))
			buf = append(buf, []byte(lenPrefix)...)
			buf = append(buf, []byte(val)...)
		case TypeLLLVAR:
			lenPrefix := fmt.Sprintf("%03d", len(val))
			buf = append(buf, []byte(lenPrefix)...)
			buf = append(buf, []byte(val)...)
		}
	}

	return buf, nil
}

// UnpackMessage parses raw bytes into a structured Message instance.
func UnpackMessage(data []byte) (*Message, error) {
	if len(data) < 12 { // 4 bytes MTI + 8 bytes primary bitmap
		return nil, ErrInvalidMessage
	}

	mti := string(data[:4])
	msg := NewMessage(mti)

	hasSecondary := (data[4] & 0x80) != 0
	bitmapLen := 8
	if hasSecondary {
		bitmapLen = 16
	}

	if len(data) < 4+bitmapLen {
		return nil, ErrInvalidMessage
	}

	bitmap := data[4 : 4+bitmapLen]
	offset := 4 + bitmapLen

	for de := 2; de <= bitmapLen*8; de++ {
		byteIndex := (de - 1) / 8
		bitIndex := 7 - ((de - 1) % 8)
		if (bitmap[byteIndex] & (1 << bitIndex)) == 0 {
			continue
		}

		def, ok := fieldDefs[de]
		if !ok {
			def = FieldDef{Length: 0, Type: TypeLLVAR}
		}

		switch def.Type {
		case TypeFixed:
			if offset+def.Length > len(data) {
				return nil, ErrInvalidMessage
			}
			val := string(data[offset : offset+def.Length])
			offset += def.Length
			msg.Set(de, val)
		case TypeLLVAR:
			if offset+2 > len(data) {
				return nil, ErrInvalidMessage
			}
			l, err := strconv.Atoi(string(data[offset : offset+2]))
			if err != nil || offset+2+l > len(data) {
				return nil, ErrInvalidMessage
			}
			offset += 2
			val := string(data[offset : offset+l])
			offset += l
			msg.Set(de, val)
		case TypeLLLVAR:
			if offset+3 > len(data) {
				return nil, ErrInvalidMessage
			}
			l, err := strconv.Atoi(string(data[offset : offset+3]))
			if err != nil || offset+3+l > len(data) {
				return nil, ErrInvalidMessage
			}
			offset += 3
			val := string(data[offset : offset+l])
			offset += l
			msg.Set(de, val)
		}
	}

	return msg, nil
}
