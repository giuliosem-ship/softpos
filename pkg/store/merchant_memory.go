package store

import (
	"context"
	"sync"
	"time"
)

// MerchantRepository is the persistence contract for merchant profiles, virtual
// CLABEs, encrypted CSDs and monthly RESICO sales accumulators. It is
// implemented by SQLMerchantRepository (PostgreSQL) and
// MemoryMerchantRepository (no-DB dev/test mode).
type MerchantRepository interface {
	SaveMerchant(ctx context.Context, m MerchantRecord) error
	GetMerchant(ctx context.Context, id string) (MerchantRecord, bool, error)
	GetMerchantByCLABE(ctx context.Context, clabe string) (MerchantRecord, bool, error)
	GetMerchantCLABE(ctx context.Context, merchantID string) (string, bool, error)
	SetMerchantCLABE(ctx context.Context, merchantID, clabe string) error
	SaveMerchantCSD(ctx context.Context, merchantID string, encryptedCSDJSON []byte) error
	GetMerchantCSD(ctx context.Context, merchantID string) ([]byte, bool, error)
	GetMonthlySalesCents(ctx context.Context, merchantID, yearMonth string) (int64, error)
	AddMonthlySalesCents(ctx context.Context, merchantID, yearMonth string, amountCents int64) (int64, error)
}

var (
	_ MerchantRepository = (*SQLMerchantRepository)(nil)
	_ MerchantRepository = (*MemoryMerchantRepository)(nil)
)

// MemoryMerchantRepository is a goroutine-safe in-process MerchantRepository
// with the same semantics as the SQL implementation. State is lost on restart.
type MemoryMerchantRepository struct {
	mu           sync.RWMutex
	merchants    map[string]MerchantRecord
	clabeOwner   map[string]string
	monthlySales map[string]int64 // merchantID + "|" + yearMonth
}

func NewMemoryMerchantRepository() *MemoryMerchantRepository {
	return &MemoryMerchantRepository{
		merchants:    make(map[string]MerchantRecord),
		clabeOwner:   make(map[string]string),
		monthlySales: make(map[string]int64),
	}
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	return append([]byte(nil), b...)
}

func (r *MemoryMerchantRepository) SaveMerchant(_ context.Context, m MerchantRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UTC()
	existing, ok := r.merchants[m.ID]
	if m.VirtualCLABE != "" {
		if owner, taken := r.clabeOwner[m.VirtualCLABE]; taken && owner != m.ID {
			return ErrDuplicateCLABE
		}
	}
	rec := MerchantRecord{ID: m.ID, RFC: m.RFC, Name: m.Name, IsRESICO: m.IsRESICO, UpdatedAt: now}
	if ok {
		rec.CreatedAt = existing.CreatedAt
		rec.VirtualCLABE = existing.VirtualCLABE
		rec.EncryptedCSDJSON = existing.EncryptedCSDJSON
	} else {
		rec.CreatedAt = now
	}
	if m.VirtualCLABE != "" {
		if rec.VirtualCLABE != "" && rec.VirtualCLABE != m.VirtualCLABE {
			delete(r.clabeOwner, rec.VirtualCLABE)
		}
		rec.VirtualCLABE = m.VirtualCLABE
		r.clabeOwner[m.VirtualCLABE] = m.ID
	}
	if len(m.EncryptedCSDJSON) > 0 {
		rec.EncryptedCSDJSON = cloneBytes(m.EncryptedCSDJSON)
	}
	r.merchants[m.ID] = rec
	return nil
}

func (r *MemoryMerchantRepository) GetMerchant(_ context.Context, id string) (MerchantRecord, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.merchants[id]
	if !ok {
		return MerchantRecord{}, false, nil
	}
	m.EncryptedCSDJSON = cloneBytes(m.EncryptedCSDJSON)
	return m, true, nil
}

func (r *MemoryMerchantRepository) GetMerchantByCLABE(ctx context.Context, clabe string) (MerchantRecord, bool, error) {
	r.mu.RLock()
	id, ok := r.clabeOwner[clabe]
	r.mu.RUnlock()
	if !ok {
		return MerchantRecord{}, false, nil
	}
	return r.GetMerchant(ctx, id)
}

func (r *MemoryMerchantRepository) GetMerchantCLABE(_ context.Context, merchantID string) (string, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.merchants[merchantID]
	if !ok || m.VirtualCLABE == "" {
		return "", false, nil
	}
	return m.VirtualCLABE, true, nil
}

func (r *MemoryMerchantRepository) SetMerchantCLABE(_ context.Context, merchantID, clabe string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.merchants[merchantID]
	if !ok {
		return ErrMerchantNotFound
	}
	if owner, taken := r.clabeOwner[clabe]; taken && owner != merchantID {
		return ErrDuplicateCLABE
	}
	if m.VirtualCLABE != "" && m.VirtualCLABE != clabe {
		delete(r.clabeOwner, m.VirtualCLABE)
	}
	m.VirtualCLABE = clabe
	m.UpdatedAt = time.Now().UTC()
	r.merchants[merchantID] = m
	r.clabeOwner[clabe] = merchantID
	return nil
}

func (r *MemoryMerchantRepository) SaveMerchantCSD(_ context.Context, merchantID string, encryptedCSDJSON []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.merchants[merchantID]
	if !ok {
		return ErrMerchantNotFound
	}
	m.EncryptedCSDJSON = cloneBytes(encryptedCSDJSON)
	m.UpdatedAt = time.Now().UTC()
	r.merchants[merchantID] = m
	return nil
}

func (r *MemoryMerchantRepository) GetMerchantCSD(_ context.Context, merchantID string) ([]byte, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.merchants[merchantID]
	if !ok || len(m.EncryptedCSDJSON) == 0 {
		return nil, false, nil
	}
	return cloneBytes(m.EncryptedCSDJSON), true, nil
}

func (r *MemoryMerchantRepository) GetMonthlySalesCents(_ context.Context, merchantID, yearMonth string) (int64, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.monthlySales[merchantID+"|"+yearMonth], nil
}

func (r *MemoryMerchantRepository) AddMonthlySalesCents(_ context.Context, merchantID, yearMonth string, amountCents int64) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := merchantID + "|" + yearMonth
	if amountCents > 0 {
		if _, ok := r.merchants[merchantID]; !ok {
			return 0, ErrMerchantNotFound
		}
		r.monthlySales[key] += amountCents
	}
	return r.monthlySales[key], nil
}
