package outbox

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"softpos/pkg/store"
)

// memSource is an in-memory Source with the same semantics as the SQL repo.
type memSource struct {
	mu      sync.Mutex
	events  map[int64]*store.OutboxEvent
	next    map[int64]time.Time
	claimed int
}

func newMemSource(n, maxRetries int) *memSource {
	s := &memSource{events: map[int64]*store.OutboxEvent{}, next: map[int64]time.Time{}}
	for i := 1; i <= n; i++ {
		s.events[int64(i)] = &store.OutboxEvent{ID: int64(i), EventType: "payment.settled", AggregateID: "p", Status: "PENDING", MaxRetries: maxRetries}
	}
	return s
}

func (s *memSource) ClaimDueEvents(_ context.Context, limit int, now time.Time, lease time.Duration) ([]store.OutboxEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.OutboxEvent
	ids := make([]int64, 0, len(s.events))
	for id := range s.events {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		ev := s.events[id]
		due := ev.RetryCount < ev.MaxRetries &&
			((ev.Status == "PENDING" && !s.next[id].After(now)) || (ev.Status == "PROCESSING" && !ev.UpdatedAt.After(now.Add(-lease))))
		if !due {
			continue
		}
		ev.Status = "PROCESSING"
		ev.UpdatedAt = now
		out = append(out, *ev)
		s.claimed++
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *memSource) MarkPublished(_ context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events[id].Status = "PUBLISHED"
	return nil
}

func (s *memSource) MarkRetry(_ context.Context, id int64, msg string, next time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ev := s.events[id]
	ev.RetryCount++
	ev.LastError = msg
	if ev.RetryCount >= ev.MaxRetries {
		ev.Status = "FAILED"
	} else {
		ev.Status = "PENDING"
	}
	s.next[id] = next
	return nil
}

func (s *memSource) status(id int64) (string, int, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.events[id].Status, s.events[id].RetryCount, s.next[id]
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func TestBackoff_ExponentialAndCapped(t *testing.T) {
	cases := map[int]time.Duration{0: time.Second, 1: 2 * time.Second, 3: 8 * time.Second, 10: time.Minute, 200: time.Minute, -1: time.Second}
	for retry, want := range cases {
		if got := Backoff(retry, time.Second, time.Minute); got != want {
			t.Errorf("Backoff(%d)=%v want %v", retry, got, want)
		}
	}
}

func TestRunOnce_PublishesAndMarks(t *testing.T) {
	src := newMemSource(3, 5)
	var mu sync.Mutex
	var got []int64
	pub := PublisherFunc(func(_ context.Context, ev store.OutboxEvent) error {
		mu.Lock()
		got = append(got, ev.ID)
		mu.Unlock()
		return nil
	})
	res, err := NewDispatcher(src, pub, Config{BatchSize: 10}).RunOnce(context.Background())
	if err != nil || res.Published != 3 || res.Retried != 0 {
		t.Fatalf("RunOnce: %+v %v", res, err)
	}
	if len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Fatalf("expected ordered delivery 1..3, got %v", got)
	}
	for id := int64(1); id <= 3; id++ {
		if st, _, _ := src.status(id); st != "PUBLISHED" {
			t.Fatalf("event %d status %s", id, st)
		}
	}
	// Nothing left to dispatch.
	if res, _ := NewDispatcher(src, pub, Config{}).RunOnce(context.Background()); res.Published != 0 {
		t.Fatalf("expected no redelivery, got %+v", res)
	}
}

func TestRunOnce_RetriesWithBackoffThenDeadLetters(t *testing.T) {
	src := newMemSource(1, 3)
	clock := &fakeClock{now: time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)}
	attempts := 0
	pub := PublisherFunc(func(context.Context, store.OutboxEvent) error {
		attempts++
		return errors.New("broker down")
	})
	d := NewDispatcher(src, pub, Config{BaseBackoff: time.Second, MaxBackoff: time.Minute, Now: clock.Now})

	// Attempt 1 fails -> retry scheduled at now+1s.
	if res, err := d.RunOnce(context.Background()); err != nil || res.Retried != 1 {
		t.Fatalf("attempt 1: %+v %v", res, err)
	}
	st, retries, next := src.status(1)
	if st != "PENDING" || retries != 1 || !next.Equal(clock.Now().Add(time.Second)) {
		t.Fatalf("after attempt 1: %s retries=%d next=%v", st, retries, next)
	}
	// Not due yet: no attempt.
	if res, _ := d.RunOnce(context.Background()); res.Retried != 0 || attempts != 1 {
		t.Fatalf("event dispatched before its backoff elapsed: %+v attempts=%d", res, attempts)
	}
	// Attempt 2 after 1s -> next delay 2s.
	clock.Advance(time.Second)
	_, _ = d.RunOnce(context.Background())
	_, retries, next = src.status(1)
	if retries != 2 || !next.Equal(clock.Now().Add(2*time.Second)) {
		t.Fatalf("after attempt 2: retries=%d next=%v", retries, next)
	}
	// Attempt 3 reaches max_retries -> FAILED, never retried again.
	clock.Advance(2 * time.Second)
	_, _ = d.RunOnce(context.Background())
	clock.Advance(time.Hour)
	_, _ = d.RunOnce(context.Background())
	st, retries, _ = src.status(1)
	if st != "FAILED" || retries != 3 || attempts != 3 {
		t.Fatalf("expected dead letter after 3 attempts: %s retries=%d attempts=%d", st, retries, attempts)
	}
}

func TestRunOnce_PublisherPanicIsRetried(t *testing.T) {
	src := newMemSource(1, 5)
	pub := PublisherFunc(func(context.Context, store.OutboxEvent) error { panic("boom") })
	res, err := NewDispatcher(src, pub, Config{}).RunOnce(context.Background())
	if err != nil || res.Retried != 1 {
		t.Fatalf("expected panic to be converted into a retry: %+v %v", res, err)
	}
}

func TestRunOnce_ReclaimsExpiredLease(t *testing.T) {
	src := newMemSource(1, 5)
	clock := &fakeClock{now: time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)}
	// Simulate a crashed dispatcher that claimed the event.
	_, _ = src.ClaimDueEvents(context.Background(), 10, clock.Now(), time.Minute)
	d := NewDispatcher(src, NoopPublisher{}, Config{Lease: time.Minute, Now: clock.Now})
	if res, _ := d.RunOnce(context.Background()); res.Published != 0 {
		t.Fatalf("event with live lease must not be reclaimed")
	}
	clock.Advance(time.Minute)
	if res, _ := d.RunOnce(context.Background()); res.Published != 1 {
		t.Fatalf("expected expired lease to be reclaimed, got %+v", res)
	}
}

func TestRun_DispatchesAndStopsOnCancel(t *testing.T) {
	src := newMemSource(5, 5)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- NewDispatcher(src, LogPublisher{}, Config{PollInterval: 5 * time.Millisecond, BatchSize: 2}).Run(ctx)
	}()

	deadline := time.After(2 * time.Second)
	for {
		if st, _, _ := src.status(5); st == "PUBLISHED" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("dispatcher did not publish all events in time")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v on cancel", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("Run did not stop after context cancellation")
	}
}

func TestRunOnce_CancelledContextStopsBatchButRecordsCurrent(t *testing.T) {
	src := newMemSource(3, 5)
	ctx, cancel := context.WithCancel(context.Background())
	pub := PublisherFunc(func(context.Context, store.OutboxEvent) error {
		cancel() // shutdown arrives while publishing the first event
		return nil
	})
	res, err := NewDispatcher(src, pub, Config{}).RunOnce(ctx)
	if err != nil || res.Published != 1 {
		t.Fatalf("expected only the in-flight event to complete: %+v %v", res, err)
	}
	if st, _, _ := src.status(1); st != "PUBLISHED" {
		t.Fatalf("in-flight event must still be marked published after cancel, got %s", st)
	}
	if st, _, _ := src.status(2); st != "PROCESSING" {
		t.Fatalf("unprocessed claimed event should stay PROCESSING for lease reclaim, got %s", st)
	}
}
