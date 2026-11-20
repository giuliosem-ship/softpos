package auth

import (
	"context"
	"sort"
	"sync"
	"time"
)

// MemoryRepository is a goroutine-safe in-process Repository (dev/test mode).
type MemoryRepository struct {
	mu          sync.Mutex
	users       map[string]User
	phoneIndex  map[string]string
	memberships map[string]map[string]Membership // user -> merchant -> m
	otps        map[string]OTPChallenge
	refresh     map[string]RefreshToken // id -> token
	refreshHash map[string]string       // hash -> id
	nonces      map[string]memNonce
}

type memNonce struct {
	expiresAt time.Time
	used      bool
}

var _ Repository = (*MemoryRepository)(nil)

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{
		users:       map[string]User{},
		phoneIndex:  map[string]string{},
		memberships: map[string]map[string]Membership{},
		otps:        map[string]OTPChallenge{},
		refresh:     map[string]RefreshToken{},
		refreshHash: map[string]string{},
		nonces:      map[string]memNonce{},
	}
}

func (r *MemoryRepository) CreateUser(_ context.Context, u User) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.users[u.ID]; ok {
		return ErrConflict
	}
	if _, ok := r.phoneIndex[u.Phone]; ok {
		return ErrConflict
	}
	r.users[u.ID] = u
	r.phoneIndex[u.Phone] = u.ID
	return nil
}

func (r *MemoryRepository) GetUser(_ context.Context, id string) (User, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.users[id]
	return u, ok, nil
}

func (r *MemoryRepository) GetUserByPhone(_ context.Context, phone string) (User, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.phoneIndex[phone]
	if !ok {
		return User{}, false, nil
	}
	return r.users[id], true, nil
}

func (r *MemoryRepository) AddMembership(_ context.Context, m Membership) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.users[m.UserID]; !ok {
		return ErrUserNotFound
	}
	byMerchant := r.memberships[m.UserID]
	if byMerchant == nil {
		byMerchant = map[string]Membership{}
		r.memberships[m.UserID] = byMerchant
	}
	if existing, ok := byMerchant[m.MerchantID]; ok {
		m.CreatedAt = existing.CreatedAt
	}
	byMerchant[m.MerchantID] = m
	return nil
}

func (r *MemoryRepository) GetMembership(_ context.Context, userID, merchantID string) (Membership, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.memberships[userID][merchantID]
	return m, ok, nil
}

func (r *MemoryRepository) ListMemberships(_ context.Context, userID string) ([]Membership, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Membership, 0, len(r.memberships[userID]))
	for _, m := range r.memberships[userID] {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MerchantID < out[j].MerchantID })
	return out, nil
}

func (r *MemoryRepository) SaveOTPChallenge(_ context.Context, c OTPChallenge) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c.Attempts = 0
	r.otps[c.Phone] = c
	return nil
}

func (r *MemoryRepository) GetOTPChallenge(_ context.Context, phone string) (OTPChallenge, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.otps[phone]
	return c, ok, nil
}

func (r *MemoryRepository) IncrementOTPAttempts(_ context.Context, phone string) (int, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.otps[phone]
	if !ok {
		return 0, false, nil
	}
	c.Attempts++
	r.otps[phone] = c
	return c.Attempts, true, nil
}

func (r *MemoryRepository) DeleteOTPChallenge(_ context.Context, phone string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.otps[phone]
	delete(r.otps, phone)
	return ok, nil
}

func (r *MemoryRepository) CreateRefreshToken(_ context.Context, t RefreshToken) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.insertRefreshLocked(t)
}

func (r *MemoryRepository) insertRefreshLocked(t RefreshToken) error {
	if _, ok := r.refresh[t.ID]; ok {
		return ErrConflict
	}
	if _, ok := r.refreshHash[t.TokenHash]; ok {
		return ErrConflict
	}
	r.refresh[t.ID] = t
	r.refreshHash[t.TokenHash] = t.ID
	return nil
}

func (r *MemoryRepository) GetRefreshTokenByHash(_ context.Context, hash string) (RefreshToken, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.refreshHash[hash]
	if !ok {
		return RefreshToken{}, false, nil
	}
	return cloneRefresh(r.refresh[id]), true, nil
}

func (r *MemoryRepository) RotateRefreshToken(_ context.Context, oldID string, next RefreshToken, now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	old, ok := r.refresh[oldID]
	if !ok {
		return ErrRefreshTokenInvalid
	}
	if old.RevokedAt != nil {
		return ErrRefreshTokenReused
	}
	if err := r.insertRefreshLocked(next); err != nil {
		return err
	}
	ts := now
	old.RevokedAt = &ts
	old.ReplacedBy = next.ID
	r.refresh[oldID] = old
	return nil
}

func cloneRefresh(t RefreshToken) RefreshToken {
	t.GraceSealed = append([]byte(nil), t.GraceSealed...)
	return t
}

func (r *MemoryRepository) GetRefreshTokenByID(_ context.Context, id string) (RefreshToken, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.refresh[id]
	return cloneRefresh(t), ok, nil
}

func (r *MemoryRepository) ClaimRefreshGrace(_ context.Context, id string, now time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.refresh[id]
	if !ok || t.GraceUsedAt != nil {
		return false, nil
	}
	ts := now
	t.GraceUsedAt = &ts
	r.refresh[id] = t
	return true, nil
}

func (r *MemoryRepository) RevokeRefreshFamily(_ context.Context, familyID string, now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, t := range r.refresh {
		if t.FamilyID == familyID && t.RevokedAt == nil {
			ts := now
			t.RevokedAt = &ts
			r.refresh[id] = t
		}
	}
	return nil
}

func (r *MemoryRepository) SaveAttestationNonce(_ context.Context, n AttestationNonce) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.nonces[n.NonceHash]; ok {
		return ErrConflict
	}
	now := time.Now()
	for k, v := range r.nonces { // bounded memory: drop expired nonces
		if now.After(v.expiresAt) {
			delete(r.nonces, k)
		}
	}
	r.nonces[n.NonceHash] = memNonce{expiresAt: n.ExpiresAt}
	return nil
}

func (r *MemoryRepository) ConsumeAttestationNonce(_ context.Context, hash string, now time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.nonces[hash]
	if !ok || n.used || !now.Before(n.expiresAt) {
		return false, nil
	}
	n.used = true
	r.nonces[hash] = n
	return true, nil
}
