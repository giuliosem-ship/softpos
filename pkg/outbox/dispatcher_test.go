package outbox

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"softpos/pkg/store"
)

// memSource is an in-memory Source with the same semantics as the SQL repo
// (leases with owners; reclaiming an expired lease counts as an attempt).
type memSource struct {
	mu      sync.Mutex
	events  map[int64]*store.OutboxEvent
	next    map[int64]time.Time
	owner   map[int64]string
	claimed int
}

func newMemSource(n, maxRetries int) *memSource {
	s := &memSource{events: map[int64]*store.OutboxEvent{}, next: map[int64]time.Time{}, owner: map[int64]string{}}
	for i := 1; i <= n; i++ {
		s.events[int64(i)] = &store.OutboxEvent{ID: int64(i), EventType: "payment.settled", AggregateID: "p", Status: "PENDING", MaxRetries: maxRetries}
	}
	return s
}

func (s *memSource) ClaimDueEvents(_ context.Context, limit int, now time.Time, lease time.Duration, owner string) ([]store.OutboxEvent, error) {
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
		reclaim := ev.Status == "PROCESSING" && !ev.UpdatedAt.After(now.Add(-lease))
		due := ev.RetryCount < ev.MaxRetries && ((ev.Status == "PENDING" && !s.next[id].After(now)) || reclaim)
		if !due {
			continue
		}
		ev.UpdatedAt = now
		if reclaim {
			ev.RetryCount++
			ev.LastError = store.LeaseExpiredError
			if ev.RetryCount >= ev.MaxRetries {
				ev.Status = "FAILED"
				s.owner[id] = ""
				continue
			}
		}
		ev.Status = "PROCESSING"
		s.owner[id] = owner
		out = append(out, *ev)
		s.claimed++
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *memSource) MarkPublished(_ context.Context, id int64, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.events[id].Status != "PROCESSING" || s.owner[id] != owner {
		return store.ErrLeaseLost
	}
	s.events[id].Status = "PUBLISHED"
	s.owner[id] = ""
	return nil
}

func (s *memSource) MarkRetry(_ context.Context, id int64, owner, msg string, next time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ev := s.events[id]
	if ev.Status != "PROCESSING" || s.owner[id] != owner {
		return store.ErrLeaseLost
	}
	ev.RetryCount++
	ev.LastError = msg
	if ev.RetryCount >= ev.MaxRetries {
		ev.Status = "FAILED"
	} else {
		ev.Status = "PENDING"
	}
	s.owner[id] = ""
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
	_, _ = src.ClaimDueEvents(context.Background(), 10, clock.Now(), time.Minute, "crashed-dispatcher")
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

func TestRunOnce_ReclaimCountsAsAttemptAndPoisonEventFails(t *testing.T) {
	src := newMemSource(1, 3)
	clock := &fakeClock{now: time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)}
	// Every dispatcher that claims the poison event crashes before recording a result.
	for i := 0; i < 3; i++ {
		evs, _ := src.ClaimDueEvents(context.Background(), 10, clock.Now(), time.Minute, fmt.Sprintf("crash-%d", i))
		if i < 3 && len(evs) != 1 {
			t.Fatalf("claim %d: got %d events", i, len(evs))
		}
		clock.Advance(time.Minute)
	}
	// The 3rd reclaim reaches max_retries: dead-lettered, never handed out again.
	d := NewDispatcher(src, NoopPublisher{}, Config{Lease: time.Minute, Now: clock.Now})
	res, err := d.RunOnce(context.Background())
	if err != nil || res.Published != 0 {
		t.Fatalf("poison event must not be published: %+v %v", res, err)
	}
	if st, retries, _ := src.status(1); st != "FAILED" || retries != 3 {
		t.Fatalf("want FAILED after 3 expired leases, got %s retries=%d", st, retries)
	}
}

func TestRunOnce_PublishTimeoutIsRetried(t *testing.T) {
	src := newMemSource(1, 5)
	release := make(chan struct{})
	defer close(release)
	hung := PublisherFunc(func(context.Context, store.OutboxEvent) error {
		<-release // ignores its context entirely
		return nil
	})
	d := NewDispatcher(src, hung, Config{PublishTimeout: 20 * time.Millisecond, Lease: time.Minute})
	start := time.Now()
	res, err := d.RunOnce(context.Background())
	if err != nil || res.Retried != 1 || res.Published != 0 {
		t.Fatalf("hung publish must be retried: %+v %v", res, err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("publish timeout not enforced")
	}
	src.mu.Lock()
	lastErr := src.events[1].LastError
	src.mu.Unlock()
	if !strings.Contains(lastErr, ErrPublishTimeout.Error()) {
		t.Fatalf("last_error=%q", lastErr)
	}
}

func TestConfig_PublishTimeoutBelowLease(t *testing.T) {
	c := Config{Lease: time.Second, PublishTimeout: time.Minute}.withDefaults()
	if c.PublishTimeout >= c.Lease || c.Owner == "" {
		t.Fatalf("publish timeout %v must be < lease %v and owner set (%q)", c.PublishTimeout, c.Lease, c.Owner)
	}
	if NewDispatcher(newMemSource(0, 1), nil, Config{}).Owner() == NewDispatcher(newMemSource(0, 1), nil, Config{}).Owner() {
		t.Fatalf("dispatcher owners must be unique")
	}
}

func TestRunOnce_StaleOwnerResultIsDiscarded(t *testing.T) {
	src := newMemSource(1, 5)
	clock := &fakeClock{now: time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)}
	var b *Dispatcher
	// A's publish is slow: while it runs, its lease expires and B reclaims and publishes.
	slow := PublisherFunc(func(context.Context, store.OutboxEvent) error {
		clock.Advance(2 * time.Minute)
		if res, err := b.RunOnce(context.Background()); err != nil || res.Published != 1 {
			t.Errorf("B must reclaim and publish: %+v %v", res, err)
		}
		return errors.New("A failed late")
	})
	a := NewDispatcher(src, slow, Config{Lease: time.Minute, Now: clock.Now, Owner: "A"})
	b = NewDispatcher(src, NoopPublisher{}, Config{Lease: time.Minute, Now: clock.Now, Owner: "B"})
	res, err := a.RunOnce(context.Background())
	if err != nil || res.LeaseLost != 1 || res.Retried != 0 {
		t.Fatalf("A's late result must be discarded: %+v %v", res, err)
	}
	if st, retries, _ := src.status(1); st != "PUBLISHED" || retries != 1 {
		t.Fatalf("B's publish must stand: %s retries=%d", st, retries)
	}
}

// TestRunOnce_StopsBatchBeforeLeaseExpires: events later in a batch must not
// be published once their lease could expire during the publish (another
// dispatcher may already have reclaimed them -> concurrent duplicate publish).
func TestRunOnce_StopsBatchBeforeLeaseExpires(t *testing.T) {
	src := newMemSource(4, 5)
	clock := &fakeClock{now: time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)}
	var published []int64
	slow := PublisherFunc(func(_ context.Context, ev store.OutboxEvent) error {
		published = append(published, ev.ID)
		clock.Advance(20 * time.Second) // each publish takes up to the publish timeout
		return nil
	})
	d := NewDispatcher(src, slow, Config{Lease: time.Minute, PublishTimeout: 20 * time.Second, Now: clock.Now, BatchSize: 10})
	res, err := d.RunOnce(context.Background())
	if err != nil || res.Published != 2 {
		t.Fatalf("want 2 published before the lease could expire, got %+v %v (published %v)", res, err, published)
	}
	if len(published) != 2 {
		t.Fatalf("publisher called for %v; events 3 and 4 could outlive the lease", published)
	}
	if st, _, _ := src.status(3); st != "PROCESSING" {
		t.Fatalf("unpublished event must stay PROCESSING for lease reclaim, got %s", st)
	}
}
