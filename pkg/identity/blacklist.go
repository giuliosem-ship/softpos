package identity

import (
	"context"
	"errors"
	"strings"
	"sync"
)

// ErrBlacklistedEntity is returned when a merchant is blacklisted under SAT Article 69-B.
var ErrBlacklistedEntity = errors.New("merchant is blacklisted under SAT Article 69-B")

// BlacklistStatus represents the status of a blacklist entry.
type BlacklistStatus string

const (
	BlacklistClean          BlacklistStatus = "CLEAN"
	BlacklistPresunto       BlacklistStatus = "PRESUNTO"
	BlacklistDesvirtuado   BlacklistStatus = "DESVIRTUADO"
	BlacklistDefinitivo     BlacklistStatus = "DEFINITIVO"
	BlacklistSentenciaFavorable BlacklistStatus = "SENTENCIA_FAVORABLE"
)

// BlacklistEntry represents an entry in the blacklist.
type BlacklistEntry struct {
	RFC       string
	Status    BlacklistStatus
	OficioSAT string
}

// BlacklistRepository persists SAT 69-B records in PostgreSQL.
type BlacklistRepository interface {
	AddEntry(ctx context.Context, entry BlacklistEntry) error
	GetEntry(ctx context.Context, rfc string) (BlacklistEntry, bool, error)
}

// BlacklistService manages the blacklist entries.
type BlacklistService struct {
	mu      sync.RWMutex
	repo    BlacklistRepository
	entries map[string]BlacklistEntry
}

// NewBlacklistService initializes a new BlacklistService.
func NewBlacklistService() *BlacklistService {
	return &BlacklistService{
		entries: make(map[string]BlacklistEntry),
	}
}

func (s *BlacklistService) SetRepository(repo BlacklistRepository) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repo = repo
}

// AddEntry adds a new entry to the blacklist.
func (s *BlacklistService) AddEntry(rfc string, status BlacklistStatus, oficioSAT string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	normalizedRFC := strings.ToUpper(strings.TrimSpace(rfc))
	entry := BlacklistEntry{
		RFC:       normalizedRFC,
		Status:    status,
		OficioSAT: oficioSAT,
	}

	if s.repo != nil {
		_ = s.repo.AddEntry(context.Background(), entry)
	}

	s.entries[normalizedRFC] = entry
}

// GetStatus returns the blacklist status for a given RFC.
func (s *BlacklistService) GetStatus(rfc string) BlacklistStatus {
	entry, exists := s.GetEntry(rfc)
	if !exists {
		return BlacklistClean
	}
	return entry.Status
}

// GetEntry returns the blacklist entry for a given RFC.
func (s *BlacklistService) GetEntry(rfc string) (BlacklistEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	normalizedRFC := strings.ToUpper(strings.TrimSpace(rfc))
	if s.repo != nil {
		if entry, exists, err := s.repo.GetEntry(context.Background(), normalizedRFC); err == nil && exists {
			return entry, true
		}
	}

	entry, exists := s.entries[normalizedRFC]
	return entry, exists
}

// GetOficio returns the OficioSAT for a given RFC.
func (s *BlacklistService) GetOficio(rfc string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	normalizedRFC := strings.ToUpper(strings.TrimSpace(rfc))
	entry, exists := s.entries[normalizedRFC]
	if !exists {
		return ""
	}
	return entry.OficioSAT
}

// ValidateMerchantEligibility checks if a merchant is eligible based on their blacklist status.
func (s *BlacklistService) ValidateMerchantEligibility(rfc string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	normalizedRFC := strings.ToUpper(strings.TrimSpace(rfc))
	entry, exists := s.entries[normalizedRFC]
	if !exists {
		return true, nil
	}

	switch entry.Status {
	case BlacklistDefinitivo, BlacklistPresunto:
		return false, ErrBlacklistedEntity
	default:
		return true, nil
	}
}
