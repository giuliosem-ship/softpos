package identity

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"
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

// ParseCSVContent parses a SAT 69-B CSV (see Parse69BCSV).
func (w *SATIngestionWorker) ParseCSVContent(csvBytes []byte) ([]SATBlacklistRecord, error) {
	parsed, err := Parse69BCSV(csvBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse CSV: %w", err)
	}
	out := make([]SATBlacklistRecord, 0, len(parsed.Rows))
	for _, r := range parsed.Rows {
		out = append(out, SATBlacklistRecord{RFC: r.RFC, Status: r.Status, OficioSAT: r.Oficio})
	}
	return out, nil
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
