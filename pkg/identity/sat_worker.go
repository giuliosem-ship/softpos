package identity

import (
	"crypto/sha256"
	"encoding/csv"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"
)

type SyncResult struct {
	Status             string
	TotalRecordsParsed int
	RecordsUpdated     int
	SHA256Hash         string
}

type FlaggedMerchant struct {
	RFC       string
	Status    BlacklistStatus
	OficioSAT string
}

type SATBlacklistRecord struct {
	RFC       string
	Status    BlacklistStatus
	OficioSAT string
}

type SATIngestionWorker struct {
	blacklistService *BlacklistService
	lastHash         string
	mu               sync.Mutex
}

func NewSATIngestionWorker(service *BlacklistService) *SATIngestionWorker {
	return &SATIngestionWorker{
		blacklistService: service,
		lastHash:         "",
	}
}

func decodeLatin1OrUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	runes := make([]rune, len(b))
	for i, c := range b {
		runes[i] = rune(c)
	}
	return string(runes)
}

func (w *SATIngestionWorker) ParseCSVContent(csvBytes []byte) ([]SATBlacklistRecord, error) {
	text := decodeLatin1OrUTF8(csvBytes)
	reader := csv.NewReader(strings.NewReader(text))
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true

	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("failed to parse CSV: %w", err)
	}

	var blacklistRecords []SATBlacklistRecord
	headerFound := false

	for _, record := range records {
		joined := strings.Join(record, ",")
		if !headerFound {
			if strings.Contains(joined, "RFC") {
				headerFound = true
			}
			continue
		}

		if len(record) < 3 {
			continue
		}

		rfc := strings.TrimSpace(record[1])
		if len(rfc) < 12 {
			continue
		}

		var statusText string
		if len(record) > 3 {
			statusText = record[3]
		}
		status := normalizeStatus(statusText)
		if status == "" {
			continue
		}

		oficioSAT := ""
		for _, field := range record {
			cleanField := strings.TrimSpace(field)
			if strings.HasPrefix(cleanField, "500-") {
				oficioSAT = cleanField
				break
			}
		}

		blacklistRecords = append(blacklistRecords, SATBlacklistRecord{
			RFC:       rfc,
			Status:    status,
			OficioSAT: oficioSAT,
		})
	}

	return blacklistRecords, nil
}

func normalizeStatus(statusText string) BlacklistStatus {
	upper := strings.TrimSpace(strings.ToUpper(statusText))
	switch {
	case strings.Contains(upper, "DEFINITIVO"):
		return BlacklistDefinitivo
	case strings.Contains(upper, "PRESUNTO"):
		return BlacklistPresunto
	case strings.Contains(upper, "DESVIRTUADO"):
		return BlacklistDesvirtuado
	case strings.Contains(upper, "SENTENCIA FAVORABLE"):
		return BlacklistSentenciaFavorable
	default:
		return ""
	}
}

func (w *SATIngestionWorker) SyncFromBytes(csvBytes []byte, force bool) (SyncResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	hash := sha256.Sum256(csvBytes)
	hashHex := fmt.Sprintf("%x", hash)

	if !force && hashHex == w.lastHash {
		return SyncResult{
			Status:             "SKIPPED_UNCHANGED",
			TotalRecordsParsed: 0,
			RecordsUpdated:     0,
			SHA256Hash:         hashHex,
		}, nil
	}

	records, err := w.ParseCSVContent(csvBytes)
	if err != nil {
		return SyncResult{}, fmt.Errorf("failed to parse CSV content: %w", err)
	}

	for _, record := range records {
		w.blacklistService.AddEntry(record.RFC, record.Status, record.OficioSAT)
	}

	w.lastHash = hashHex

	return SyncResult{
		Status:             "SUCCESS",
		TotalRecordsParsed: len(records),
		RecordsUpdated:     len(records),
		SHA256Hash:         hashHex,
	}, nil
}

func (w *SATIngestionWorker) RetroscreenMerchants(activeRFCs []string) []FlaggedMerchant {
	var flaggedMerchants []FlaggedMerchant

	for _, rfc := range activeRFCs {
		status := w.blacklistService.GetStatus(rfc)
		if status == BlacklistDefinitivo || status == BlacklistPresunto {
			flaggedMerchants = append(flaggedMerchants, FlaggedMerchant{
				RFC:       rfc,
				Status:    status,
				OficioSAT: w.blacklistService.GetOficio(rfc),
			})
		}
	}

	return flaggedMerchants
}
