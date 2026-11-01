package clearing

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSTECBatch_GenerateAndParse100Transactions(t *testing.T) {
	cutoffDate := time.Date(2026, 10, 31, 23, 59, 59, 0, time.UTC)
	batch := NewBatchFile("SPOS", "PROSA", 1, cutoffDate)

	var expectedSales int64
	var expectedRefunds int64

	// Generate 100 transactions: 85 sales, 15 refunds
	for i := 1; i <= 100; i++ {
		isRefund := (i % 7 == 0) // some refunds
		amount := int64(i * 1250) // in cents: $12.50, $25.00, etc.

		op := OpPurchase
		if isRefund {
			op = OpRefund
			expectedRefunds += amount
		} else {
			expectedSales += amount
		}

		network := NetworkVisa
		panPrefix := "400012"
		if i%3 == 1 {
			network = NetworkMastercard
			panPrefix = "550000"
		} else if i%3 == 2 {
			network = NetworkCarnet
			panPrefix = "506300"
		}

		maskedPan := fmt.Sprintf("%s******%04d", panPrefix, i%10000)
		authCode := fmt.Sprintf("A%05d", i)
		rrn := fmt.Sprintf("REF%09d", i)

		detail := BatchDetail{
			STAN:        fmt.Sprintf("%06d", i),
			Network:     network,
			Operation:   op,
			MaskedPAN:   maskedPan,
			AmountCents: amount,
			AuthCode:    authCode,
			Timestamp:   cutoffDate.Add(-time.Duration(100-i) * time.Minute),
			MID:         "MERCHANT01",
			RRN:         rrn,
		}
		if err := batch.AddDetail(detail); err != nil {
			t.Fatalf("Failed to add detail %d: %v", i, err)
		}
	}

	rawFile, err := batch.Generate()
	if err != nil {
		t.Fatalf("batch.Generate() failed: %v", err)
	}

	// 1. Verify fixed line length
	lines := strings.Split(strings.TrimRight(rawFile, "\n"), "\n")
	if len(lines) != 102 { // 1 Header + 100 Details + 1 Trailer
		t.Fatalf("Expected 102 lines, got %d", len(lines))
	}

	expectedLineWidth := RecordWidth
	for idx, line := range lines {
		if len(line) != expectedLineWidth {
			t.Fatalf("Line %d has width %d, expected fixed width %d. Content: '%s'", idx+1, len(line), expectedLineWidth, line)
		}
	}

	// 2. Parse and validate round trip
	parsed, err := ParseBatchFile(rawFile)
	if err != nil {
		t.Fatalf("ParseBatchFile failed: %v", err)
	}

	if parsed.Trailer.TotalRecords != 100 {
		t.Errorf("Trailer TotalRecords = %d, want 100", parsed.Trailer.TotalRecords)
	}
	if parsed.Trailer.TotalSalesCents != expectedSales {
		t.Errorf("Trailer TotalSalesCents = %d, want %d", parsed.Trailer.TotalSalesCents, expectedSales)
	}
	if parsed.Trailer.TotalRefundsCents != expectedRefunds {
		t.Errorf("Trailer TotalRefundsCents = %d, want %d", parsed.Trailer.TotalRefundsCents, expectedRefunds)
	}
	if parsed.Trailer.NetCents != (expectedSales - expectedRefunds) {
		t.Errorf("Trailer NetCents = %d, want %d", parsed.Trailer.NetCents, expectedSales-expectedRefunds)
	}

	// Validate integrity checksum
	if err := parsed.Validate(); err != nil {
		t.Fatalf("parsed.Validate() failed: %v", err)
	}
}

func TestSTECBatch_TamperedDetailRejection(t *testing.T) {
	batch := NewBatchFile("SPOS", "E-GLOBAL", 2, time.Now())
	_ = batch.AddDetail(BatchDetail{
		STAN:        "000001",
		Network:     NetworkVisa,
		Operation:   OpPurchase,
		MaskedPAN:   "400012******9010",
		AmountCents: 50000,
		AuthCode:    "AUTH01",
		Timestamp:   time.Now(),
		MID:         "MERCHANT01",
		RRN:         "RRN000000001",
	})

	rawFile, err := batch.Generate()
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	// Tamper amount in raw line
	tampered := strings.Replace(rawFile, "000000050000", "000000099999", 1)
	parsed, err := ParseBatchFile(tampered)
	if err != nil {
		t.Fatalf("ParseBatchFile failed: %v", err)
	}

	// Validate must fail due to checksum mismatch and trailer total mismatch
	err = parsed.Validate()
	if err == nil {
		t.Fatal("Expected validation error for tampered batch file, got nil")
	}
}

func TestSTECBatch_EmptyBatchRejection(t *testing.T) {
	batch := NewBatchFile("SPOS", "PROSA", 3, time.Now())
	_, err := batch.Generate()
	if err == nil {
		t.Fatal("Expected error generating empty batch file, got nil")
	}
}
