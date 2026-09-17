package api

import (
	"sync"
	"time"
)

type IdempotencyRecord struct {
	StatusCode int
	Body       []byte
	ExpiresAt  time.Time
}

type IdempotencyStore struct {
	mu      sync.RWMutex
	records map[string]IdempotencyRecord
}

func NewIdempotencyStore() *IdempotencyStore {
	return &IdempotencyStore{
		records: make(map[string]IdempotencyRecord),
	}
}

func (s *IdempotencyStore) Get(key string) (IdempotencyRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

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

	s.records[key] = IdempotencyRecord{
		StatusCode: statusCode,
		Body:       body,
		ExpiresAt:  time.Now().Add(ttl),
	}
}
