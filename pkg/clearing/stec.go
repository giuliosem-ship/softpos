package clearing

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	RecordWidth = 120

	TypeHeader  = "01"
	TypeDetail  = "02"
	TypeTrailer = "09"

	OpPurchase = "00"
	OpRefund   = "20"

	NetworkVisa       = "VISA"
	NetworkMastercard = "MC"
	NetworkCarnet     = "CARN"
)

var (
	ErrEmptyBatch           = errors.New("cannot generate clearing batch with zero transactions")
	ErrInvalidRecordType    = errors.New("invalid STEC record type")
	ErrInvalidRecordWidth   = errors.New("record line does not match required fixed width")
	ErrInvalidTrailerTotals = errors.New("trailer record totals do not match detail records")
	ErrChecksumMismatch     = errors.New("integrity checksum mismatch in batch file")
	ErrInvalidAmount        = errors.New("transaction amount must be positive")
)

type BatchHeader struct {
	RecordType    string
	AcquirerID    string
	ClearingHouse string
	BatchNumber   int
	CutoffDate    time.Time
	CurrencyCode  string
}

func (h BatchHeader) Format() string {
	acquirer := fmt.Sprintf("%-4s", h.AcquirerID)[:4]
	clearing := fmt.Sprintf("%-8s", h.ClearingHouse)[:8]
	batchNum := fmt.Sprintf("%06d", h.BatchNumber)
	cutoff := h.CutoffDate.Format("20060102")
	currency := fmt.Sprintf("%-3s", h.CurrencyCode)[:3]

	line := fmt.Sprintf("%s%s%s%s%s%s", TypeHeader, acquirer, clearing, batchNum, cutoff, currency)
	if len(line) < RecordWidth {
		line += strings.Repeat(" ", RecordWidth-len(line))
	}
	return line
}

type BatchDetail struct {
	RecordType  string
	STAN        string
	Network     string
	Operation   string
	MaskedPAN   string
	AmountCents int64
	AuthCode    string
	Timestamp   time.Time
	MID         string
	RRN         string
}

func (d BatchDetail) Format() string {
	stan := fmt.Sprintf("%06s", d.STAN)[:6]
	network := fmt.Sprintf("%-4s", d.Network)[:4]
	op := fmt.Sprintf("%02s", d.Operation)[:2]
	pan := fmt.Sprintf("%-16s", d.MaskedPAN)[:16]
	amount := fmt.Sprintf("%012d", d.AmountCents)
	auth := fmt.Sprintf("%-6s", d.AuthCode)[:6]
	ts := d.Timestamp.Format("20060102150405")
	mid := fmt.Sprintf("%-10s", d.MID)[:10]
	rrn := fmt.Sprintf("%-12s", d.RRN)[:12]

	line := fmt.Sprintf("%s%s%s%s%s%s%s%s%s%s", TypeDetail, stan, network, op, pan, amount, auth, ts, mid, rrn)
	if len(line) < RecordWidth {
		line += strings.Repeat(" ", RecordWidth-len(line))
	}
	return line
}

type BatchTrailer struct {
	RecordType        string
	TotalRecords      int
	TotalSalesCents   int64
	TotalRefundsCents int64
	NetCents          int64
	ChecksumSHA256    string
}

func (t BatchTrailer) Format() string {
	recCount := fmt.Sprintf("%06d", t.TotalRecords)
	sales := fmt.Sprintf("%016d", t.TotalSalesCents)
	refunds := fmt.Sprintf("%016d", t.TotalRefundsCents)

	var netSign string
	absNet := t.NetCents
	if t.NetCents >= 0 {
		netSign = "+"
	} else {
		netSign = "-"
		absNet = -t.NetCents
	}
	net := fmt.Sprintf("%s%015d", netSign, absNet)
	checksum := fmt.Sprintf("%-64s", t.ChecksumSHA256)[:64]

	line := fmt.Sprintf("%s%s%s%s%s%s", TypeTrailer, recCount, sales, refunds, net, checksum)
	if len(line) < RecordWidth {
		line += strings.Repeat(" ", RecordWidth-len(line))
	}
	return line
}

type BatchFile struct {
	Header  BatchHeader
	Details []BatchDetail
	Trailer BatchTrailer
}

func NewBatchFile(acquirerID, clearingHouse string, batchNumber int, cutoffDate time.Time) *BatchFile {
	return &BatchFile{
		Header: BatchHeader{
			RecordType:    TypeHeader,
			AcquirerID:    acquirerID,
			ClearingHouse: clearingHouse,
			BatchNumber:   batchNumber,
			CutoffDate:    cutoffDate,
			CurrencyCode:  "484",
		},
		Details: make([]BatchDetail, 0),
	}
}

func (b *BatchFile) AddDetail(detail BatchDetail) error {
	if detail.AmountCents <= 0 {
		return ErrInvalidAmount
	}
	detail.RecordType = TypeDetail
	b.Details = append(b.Details, detail)
	return nil
}

// computeChecksum calculates the SHA-256 hash of header line and all detail lines.
func (b *BatchFile) computeChecksum() string {
	hasher := sha256.New()
	hasher.Write([]byte(b.Header.Format() + "\n"))
	for _, d := range b.Details {
		hasher.Write([]byte(d.Format() + "\n"))
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func (b *BatchFile) Generate() (string, error) {
	if len(b.Details) == 0 {
		return "", ErrEmptyBatch
	}

	var totalSales int64
	var totalRefunds int64

	for _, d := range b.Details {
		if d.Operation == OpRefund {
			totalRefunds += d.AmountCents
		} else {
			totalSales += d.AmountCents
		}
	}

	checksum := b.computeChecksum()

	b.Trailer = BatchTrailer{
		RecordType:        TypeTrailer,
		TotalRecords:      len(b.Details),
		TotalSalesCents:   totalSales,
		TotalRefundsCents: totalRefunds,
		NetCents:          totalSales - totalRefunds,
		ChecksumSHA256:    checksum,
	}

	var sb strings.Builder
	sb.WriteString(b.Header.Format() + "\n")
	for _, d := range b.Details {
		sb.WriteString(d.Format() + "\n")
	}
	sb.WriteString(b.Trailer.Format() + "\n")

	return sb.String(), nil
}

func ParseBatchFile(content string) (*BatchFile, error) {
	lines := strings.Split(strings.TrimRight(content, "\r\n"), "\n")
	if len(lines) < 2 {
		return nil, errors.New("batch file must contain at least a header and trailer")
	}

	headerLine := strings.TrimRight(lines[0], "\r")
	if len(headerLine) != RecordWidth {
		return nil, fmt.Errorf("%w: header line length is %d, expected %d", ErrInvalidRecordWidth, len(headerLine), RecordWidth)
	}
	if headerLine[:2] != TypeHeader {
		return nil, fmt.Errorf("%w: expected header type '01', got '%s'", ErrInvalidRecordType, headerLine[:2])
	}

	batchNum, _ := strconv.Atoi(strings.TrimSpace(headerLine[14:20]))
	cutoff, err := time.Parse("20060102", headerLine[20:28])
	if err != nil {
		cutoff = time.Now()
	}

	batch := &BatchFile{
		Header: BatchHeader{
			RecordType:    TypeHeader,
			AcquirerID:    strings.TrimSpace(headerLine[2:6]),
			ClearingHouse: strings.TrimSpace(headerLine[6:14]),
			BatchNumber:   batchNum,
			CutoffDate:    cutoff,
			CurrencyCode:  strings.TrimSpace(headerLine[28:31]),
		},
		Details: make([]BatchDetail, 0, len(lines)-2),
	}

	for i := 1; i < len(lines)-1; i++ {
		line := strings.TrimRight(lines[i], "\r")
		if len(line) != RecordWidth {
			return nil, fmt.Errorf("%w: detail line %d length is %d, expected %d", ErrInvalidRecordWidth, i+1, len(line), RecordWidth)
		}
		if line[:2] != TypeDetail {
			return nil, fmt.Errorf("%w: line %d expected type '02', got '%s'", ErrInvalidRecordType, i+1, line[:2])
		}

		amount, _ := strconv.ParseInt(strings.TrimSpace(line[30:42]), 10, 64)
		ts, _ := time.Parse("20060102150405", strings.TrimSpace(line[48:62]))

		detail := BatchDetail{
			RecordType:  TypeDetail,
			STAN:        strings.TrimSpace(line[2:8]),
			Network:     strings.TrimSpace(line[8:12]),
			Operation:   strings.TrimSpace(line[12:14]),
			MaskedPAN:   strings.TrimSpace(line[14:30]),
			AmountCents: amount,
			AuthCode:    strings.TrimSpace(line[42:48]),
			Timestamp:   ts,
			MID:         strings.TrimSpace(line[62:72]),
			RRN:         strings.TrimSpace(line[72:84]),
		}
		batch.Details = append(batch.Details, detail)
	}

	trailerLine := strings.TrimRight(lines[len(lines)-1], "\r")
	if len(trailerLine) != RecordWidth {
		return nil, fmt.Errorf("%w: trailer line length is %d, expected %d", ErrInvalidRecordWidth, len(trailerLine), RecordWidth)
	}
	if trailerLine[:2] != TypeTrailer {
		return nil, fmt.Errorf("%w: expected trailer type '09', got '%s'", ErrInvalidRecordType, trailerLine[:2])
	}

	totalRecs, _ := strconv.Atoi(strings.TrimSpace(trailerLine[2:8]))
	sales, _ := strconv.ParseInt(strings.TrimSpace(trailerLine[8:24]), 10, 64)
	refunds, _ := strconv.ParseInt(strings.TrimSpace(trailerLine[24:40]), 10, 64)
	netStr := strings.TrimSpace(trailerLine[40:56])
	net, _ := strconv.ParseInt(netStr, 10, 64)
	checksum := strings.TrimSpace(trailerLine[56:120])

	batch.Trailer = BatchTrailer{
		RecordType:        TypeTrailer,
		TotalRecords:      totalRecs,
		TotalSalesCents:   sales,
		TotalRefundsCents: refunds,
		NetCents:          net,
		ChecksumSHA256:    checksum,
	}

	return batch, nil
}

func (b *BatchFile) Validate() error {
	if len(b.Details) != b.Trailer.TotalRecords {
		return fmt.Errorf("%w: detail count %d != trailer count %d", ErrInvalidTrailerTotals, len(b.Details), b.Trailer.TotalRecords)
	}

	var actualSales int64
	var actualRefunds int64
	for _, d := range b.Details {
		if d.Operation == OpRefund {
			actualRefunds += d.AmountCents
		} else {
			actualSales += d.AmountCents
		}
	}

	if actualSales != b.Trailer.TotalSalesCents {
		return fmt.Errorf("%w: actual sales %d != trailer sales %d", ErrInvalidTrailerTotals, actualSales, b.Trailer.TotalSalesCents)
	}
	if actualRefunds != b.Trailer.TotalRefundsCents {
		return fmt.Errorf("%w: actual refunds %d != trailer refunds %d", ErrInvalidTrailerTotals, actualRefunds, b.Trailer.TotalRefundsCents)
	}
	if (actualSales - actualRefunds) != b.Trailer.NetCents {
		return fmt.Errorf("%w: actual net %d != trailer net %d", ErrInvalidTrailerTotals, actualSales-actualRefunds, b.Trailer.NetCents)
	}

	expectedChecksum := b.computeChecksum()
	if !strings.EqualFold(expectedChecksum, b.Trailer.ChecksumSHA256) {
		return fmt.Errorf("%w: calculated %s != trailer %s", ErrChecksumMismatch, expectedChecksum, b.Trailer.ChecksumSHA256)
	}

	return nil
}
