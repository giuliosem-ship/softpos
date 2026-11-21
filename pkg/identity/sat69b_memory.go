package identity

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// MemoryList69BStore is the in-memory List69BStore + BlacklistRepository
// (no DATABASE_URL, tests). Replacement swaps the map under the lock.
type MemoryList69BStore struct {
	mu      sync.RWMutex
	entries map[string]BlacklistEntry
	meta    *List69BMeta
	loads   []List69BLoad
	seq     int64
}

// NewMemoryList69BStore returns an empty store (no list loaded).
func NewMemoryList69BStore() *MemoryList69BStore {
	return &MemoryList69BStore{entries: map[string]BlacklistEntry{}}
}

var (
	_ List69BStore        = (*MemoryList69BStore)(nil)
	_ BlacklistRepository = (*MemoryList69BStore)(nil)
)

func (m *MemoryList69BStore) AddEntry(_ context.Context, e BlacklistEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.RFC = strings.ToUpper(strings.TrimSpace(e.RFC))
	m.entries[e.RFC] = e
	return nil
}

func (m *MemoryList69BStore) GetEntry(_ context.Context, rfc string) (BlacklistEntry, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.entries[strings.ToUpper(strings.TrimSpace(rfc))]
	return e, ok, nil
}

func (m *MemoryList69BStore) HasEntries(context.Context) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.entries) > 0, nil
}

func (m *MemoryList69BStore) addLoad(l List69BLoad) List69BLoad {
	m.seq++
	l.ID = m.seq
	if l.CreatedAt.IsZero() {
		l.CreatedAt = time.Now().UTC()
	}
	m.loads = append(m.loads, l)
	return l
}

func (m *MemoryList69BStore) ReplaceList(_ context.Context, rows []List69BRow, meta List69BMeta, guard ReplaceGuard, actor string) (List69BLoad, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prev := 0
	if m.meta != nil {
		prev = m.meta.RowCount
	}
	load := List69BLoad{Version: meta.Version, Source: meta.Source, RowCount: len(rows), PreviousCount: prev,
		MalformedCount: meta.MalformedCount, Actor: actor, Override: guard.Override, OverrideChanges: guard.OverrideChanges, CreatedAt: meta.FetchedAt}
	if !guard.Allows(prev, len(rows)) {
		load.Status, load.Detail = Load69BRejectedShrink, ErrList69BShrank.Error()
		return m.addLoad(load), ErrList69BShrank
	}
	old := make(map[string]BlacklistStatus, len(m.entries))
	for rfc, e := range m.entries {
		old[rfc] = e.Status
	}
	if loaded, changed := ChangedSituations(old, rows); !guard.AllowsChanges(loaded, changed) {
		load.Status, load.Detail = Load69BRejectedChanges, fmt.Sprintf("%v: %d of %d loaded RFCs changed or replaced (max %d%%)", ErrList69BChanged, changed, loaded, guard.MaxChangePct)
		return m.addLoad(load), ErrList69BChanged
	}
	next := make(map[string]BlacklistEntry, len(rows))
	for _, r := range rows {
		next[r.RFC] = BlacklistEntry{RFC: r.RFC, Status: r.Status, OficioSAT: r.Oficio}
	}
	m.entries = next
	meta.RowCount = len(rows)
	m.meta = &meta
	load.Status = Load69BLoaded
	return m.addLoad(load), nil
}

func (m *MemoryList69BStore) List69BMeta(context.Context) (List69BMeta, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.meta == nil {
		return List69BMeta{}, false, nil
	}
	return *m.meta, true, nil
}

func (m *MemoryList69BStore) MarkVerified(_ context.Context, version string, at time.Time, actor string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.meta == nil || m.meta.Version != version {
		return nil
	}
	m.meta.VerifiedAt = at.UTC()
	m.addLoad(List69BLoad{Status: Load69BUnchanged, Version: version, Source: m.meta.Source, RowCount: m.meta.RowCount,
		PreviousCount: m.meta.RowCount, Actor: actor, CreatedAt: at.UTC()})
	return nil
}

func (m *MemoryList69BStore) RecordLoadFailure(_ context.Context, l List69BLoad) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addLoad(l)
	return nil
}

func (m *MemoryList69BStore) ListLoads(_ context.Context, limit int) ([]List69BLoad, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	out := make([]List69BLoad, 0, limit)
	for i := len(m.loads) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, m.loads[i])
	}
	return out, nil
}
