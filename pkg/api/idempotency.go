package api

import (
	"context"
	"sync"
	"time"
)

type IdempotencyRecord struct {
	StatusCode int
	Body       []byte
	ExpiresAt  time.Time
}

// IdempotencyBackend allows plugging in Redis or PostgreSQL for distributed idempotency storage.
type IdempotencyBackend interface {
	Get(ctx context.Context, key string) (statusCode int, body []byte, exists bool, err error)
	Set(ctx context.Context, key string, statusCode int, body []byte, ttl time.Duration) error
}

type IdempotencyStore struct {
	mu      sync.RWMutex
	backend IdempotencyBackend
	records map[string]IdempotencyRecord
}

func NewIdempotencyStore() *IdempotencyStore {
	return &IdempotencyStore{
		records: make(map[string]IdempotencyRecord),
	}
}

func (s *IdempotencyStore) SetBackend(b IdempotencyBackend) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.backend = b
}

func (s *IdempotencyStore) Get(key string) (IdempotencyRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.backend != nil {
		if code, body, exists, err := s.backend.Get(context.Background(), key); err == nil && exists {
			return IdempotencyRecord{StatusCode: code, Body: body}, true
		}
	}

	rec, ok := s.records[key]
	if !ok {
		return IdempotencyRecord{}, false
	}
	if time.Now().After(rec.ExpiresAt) {
		return IdempotencyRecord{}, false
	}
	return rec, true
}

func (s *IdempotencyStore) Set(key string, statusCode int, body []byte, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.backend != nil {
		_ = s.backend.Set(context.Background(), key, statusCode, body, ttl)
	}

	s.records[key] = IdempotencyRecord{
		StatusCode: statusCode,
		Body:       body,
		ExpiresAt:  time.Now().Add(ttl),
	}
}
