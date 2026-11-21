package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ErrBlacklistedEntity is returned when a merchant is blacklisted under SAT Article 69-B.
var ErrBlacklistedEntity = errors.New("merchant is blacklisted under SAT Article 69-B")

var (
	// ErrBlacklistNotLoaded: no SAT 69-B list has been loaded, so an RFC that
	// is not found is not known to be clean.
	ErrBlacklistNotLoaded = errors.New("SAT 69-B list not loaded")
	// ErrBlacklistUnavailable: the 69-B repository lookup failed.
	ErrBlacklistUnavailable = errors.New("SAT 69-B lookup unavailable")
)

// blacklistPresence is implemented by repositories that can tell whether any
// 69-B record was ever loaded (SQL: rows in sat_blacklist).
type blacklistPresence interface {
	HasEntries(ctx context.Context) (bool, error)
}

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

// list69BMetaSource is implemented by repositories that record which SAT
// list is loaded and when it was last verified (List69BStore).
type list69BMetaSource interface {
	List69BMeta(ctx context.Context) (List69BMeta, bool, error)
}

// BlacklistService manages the blacklist entries.
type BlacklistService struct {
	mu      sync.RWMutex
	repo    BlacklistRepository
	entries map[string]BlacklistEntry
	maxAge  time.Duration
	now     func() time.Time
}

// DefaultList69BMaxAge is how long a loaded list counts as current without a
// successful re-verification against the SAT source.
const DefaultList69BMaxAge = 7 * 24 * time.Hour

// SetMaxAge sets the staleness limit of a loaded list (<= 0: default).
func (s *BlacklistService) SetMaxAge(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxAge = d
}

// MaxAge returns the effective staleness limit.
func (s *BlacklistService) MaxAge() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.maxAge <= 0 {
		return DefaultList69BMaxAge
	}
	return s.maxAge
}

// SetClock overrides the clock used for staleness (tests).
func (s *BlacklistService) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// Repository returns the configured repository (nil: in-memory entries only).
func (s *BlacklistService) Repository() BlacklistRepository {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.repo
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

// Lookup is the authoritative 69-B status of rfc for decisions (KYB): unlike
// GetStatus it never reports BlacklistClean for an RFC it could not check.
// It returns ErrBlacklistUnavailable when the repository fails,
// ErrBlacklistStale when the loaded SAT list was not verified within the max
// age, and ErrBlacklistNotLoaded when the RFC is absent and no list was ever
// loaded.
func (s *BlacklistService) Lookup(ctx context.Context, rfc string) (BlacklistStatus, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	normalizedRFC := strings.ToUpper(strings.TrimSpace(rfc))
	listLoaded := false
	if src, ok := s.repo.(list69BMetaSource); ok {
		meta, found, err := src.List69BMeta(ctx)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrBlacklistUnavailable, err)
		}
		if found {
			maxAge := s.maxAge
			if maxAge <= 0 {
				maxAge = DefaultList69BMaxAge
			}
			now := time.Now
			if s.now != nil {
				now = s.now
			}
			if age := now().Sub(meta.VerifiedAt); age > maxAge {
				return "", fmt.Errorf("%w: last verified %s ago", ErrBlacklistStale, age.Truncate(time.Minute))
			}
			listLoaded = true
		}
	}
	if s.repo != nil {
		entry, exists, err := s.repo.GetEntry(ctx, normalizedRFC)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrBlacklistUnavailable, err)
		}
		if exists {
			return entry.Status, nil
		}
	}
	if entry, exists := s.entries[normalizedRFC]; exists {
		return entry.Status, nil
	}
	if listLoaded || len(s.entries) > 0 {
		return BlacklistClean, nil
	}
	if p, ok := s.repo.(blacklistPresence); ok {
		loaded, err := p.HasEntries(ctx)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrBlacklistUnavailable, err)
		}
		if loaded {
			return BlacklistClean, nil
		}
	}
	return "", ErrBlacklistNotLoaded
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
