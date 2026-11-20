// Package outbox runs the transactional-outbox dispatcher: it polls
// outbox_events, hands each due event to a Publisher and records success or a
// retry with exponential backoff. Delivery is at-least-once; publishers must be
// idempotent on event ID.
package outbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"softpos/pkg/store"
)

// Source is the storage side of the dispatcher (store.SQLOutboxRepository).
// Claims are leased to an owner; results are recorded only by the current
// owner of a PROCESSING event (store.ErrLeaseLost otherwise).
type Source interface {
	ClaimDueEvents(ctx context.Context, limit int, now time.Time, lease time.Duration, owner string) ([]store.OutboxEvent, error)
	MarkPublished(ctx context.Context, eventID int64, owner string) error
	MarkRetry(ctx context.Context, eventID int64, owner, errMsg string, nextAttemptAt time.Time) error
}

var _ Source = (*store.SQLOutboxRepository)(nil)

// Publisher delivers one event to its destination (broker, webhook, PAC...).
type Publisher interface {
	Publish(ctx context.Context, ev store.OutboxEvent) error
}

// PublisherFunc adapts a function to Publisher.
type PublisherFunc func(ctx context.Context, ev store.OutboxEvent) error

func (f PublisherFunc) Publish(ctx context.Context, ev store.OutboxEvent) error { return f(ctx, ev) }

// NoopPublisher acknowledges every event without delivering it.
type NoopPublisher struct{}

func (NoopPublisher) Publish(context.Context, store.OutboxEvent) error { return nil }

// LogPublisher logs event metadata (never the payload, which may hold PII).
type LogPublisher struct{ Logger *slog.Logger }

func (p LogPublisher) Publish(_ context.Context, ev store.OutboxEvent) error {
	l := p.Logger
	if l == nil {
		l = slog.Default()
	}
	l.Info("outbox event published", "event_id", ev.ID, "event_type", ev.EventType,
		"aggregate_id", ev.AggregateID, "attempt", ev.RetryCount+1)
	return nil
}

// Config tunes the dispatcher. Zero values fall back to the defaults below.
type Config struct {
	PollInterval time.Duration // default 2s
	BatchSize    int           // default 50
	BaseBackoff  time.Duration // default 1s; delay = BaseBackoff * 2^retry_count
	MaxBackoff   time.Duration // default 5m
	Lease        time.Duration // default 5m; PROCESSING events older than this are reclaimed (counts as an attempt)
	// PublishTimeout bounds each Publish call (default 30s, always < Lease so
	// a hung publisher cannot outlive its lease).
	PublishTimeout time.Duration
	// Owner identifies this dispatcher's leases (default: random per instance).
	Owner  string
	Logger *slog.Logger
	Now    func() time.Time
}

const (
	DefaultPollInterval   = 2 * time.Second
	DefaultBatchSize      = 50
	DefaultBaseBackoff    = time.Second
	DefaultMaxBackoff     = 5 * time.Minute
	DefaultLease          = 5 * time.Minute
	DefaultPublishTimeout = 30 * time.Second
)

// ErrPublishTimeout is recorded when a publisher exceeds PublishTimeout.
var ErrPublishTimeout = errors.New("outbox publish timed out")

func (c Config) withDefaults() Config {
	if c.PollInterval <= 0 {
		c.PollInterval = DefaultPollInterval
	}
	if c.BatchSize <= 0 {
		c.BatchSize = DefaultBatchSize
	}
	if c.BaseBackoff <= 0 {
		c.BaseBackoff = DefaultBaseBackoff
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = DefaultMaxBackoff
	}
	if c.MaxBackoff < c.BaseBackoff {
		c.MaxBackoff = c.BaseBackoff
	}
	if c.Lease <= 0 {
		c.Lease = DefaultLease
	}
	if c.PublishTimeout <= 0 {
		c.PublishTimeout = DefaultPublishTimeout
	}
	if c.PublishTimeout >= c.Lease {
		c.PublishTimeout = c.Lease / 2
	}
	if c.Owner == "" {
		c.Owner = newOwnerID()
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// Backoff returns base * 2^retryCount, capped at max (no overflow).
func Backoff(retryCount int, base, max time.Duration) time.Duration {
	if retryCount < 0 {
		retryCount = 0
	}
	d := base
	for i := 0; i < retryCount; i++ {
		if d >= max/2 {
			return max
		}
		d *= 2
	}
	if d > max {
		return max
	}
	return d
}

// Dispatcher polls a Source and publishes due events.
type Dispatcher struct {
	src Source
	pub Publisher
	cfg Config
}

func NewDispatcher(src Source, pub Publisher, cfg Config) *Dispatcher {
	if pub == nil {
		pub = NoopPublisher{}
	}
	return &Dispatcher{src: src, pub: pub, cfg: cfg.withDefaults()}
}

// Result summarizes one dispatch pass.
type Result struct {
	Published int
	Retried   int
	LeaseLost int // results discarded: the lease expired and another dispatcher owns the event
}

// newOwnerID returns "<host>-<pid>-<random>" for lease ownership.
func newOwnerID() string {
	host, _ := os.Hostname()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), hex.EncodeToString(b))
}

// Owner returns the lease owner id of this dispatcher.
func (d *Dispatcher) Owner() string { return d.cfg.Owner }

// RunOnce claims one batch and processes it. When ctx is cancelled mid-batch,
// the current event is finished and the remaining claimed events are left
// PROCESSING; they are reclaimed after the lease expires. The same happens to
// the rest of the batch once a publish could outlive the batch's lease (slow
// destination): another dispatcher may reclaim those events at expiry, so
// publishing them here would race a concurrent duplicate publish.
func (d *Dispatcher) RunOnce(ctx context.Context) (Result, error) {
	var res Result
	claimedAt := d.cfg.Now()
	events, err := d.src.ClaimDueEvents(ctx, d.cfg.BatchSize, claimedAt, d.cfg.Lease, d.cfg.Owner)
	if err != nil {
		return res, err
	}
	// Bookkeeping must be recorded even if shutdown cancels ctx.
	markCtx := context.WithoutCancel(ctx)
	for _, ev := range events {
		if ctx.Err() != nil {
			break
		}
		if d.cfg.Now().Sub(claimedAt)+d.cfg.PublishTimeout >= d.cfg.Lease {
			d.cfg.Logger.Warn("outbox batch stopped: lease would expire during publish; rest left for reclaim",
				"event_id", ev.ID, "owner", d.cfg.Owner, "lease", d.cfg.Lease)
			break
		}
		if pubErr := d.publish(ctx, ev); pubErr != nil {
			next := d.cfg.Now().Add(Backoff(ev.RetryCount, d.cfg.BaseBackoff, d.cfg.MaxBackoff))
			if err := d.src.MarkRetry(markCtx, ev.ID, d.cfg.Owner, pubErr.Error(), next); err != nil {
				if errors.Is(err, store.ErrLeaseLost) {
					res.LeaseLost++
					d.leaseLost(ctx, ev)
					continue
				}
				return res, fmt.Errorf("mark retry for event %d: %w", ev.ID, err)
			}
			res.Retried++
			level := slog.LevelWarn
			if ev.RetryCount+1 >= ev.MaxRetries {
				level = slog.LevelError // dead-lettered
			}
			d.cfg.Logger.Log(ctx, level, "outbox publish failed", "event_id", ev.ID, "event_type", ev.EventType,
				"attempt", ev.RetryCount+1, "max_retries", ev.MaxRetries, "next_attempt_at", next, "error", pubErr)
			continue
		}
		if err := d.src.MarkPublished(markCtx, ev.ID, d.cfg.Owner); err != nil {
			if errors.Is(err, store.ErrLeaseLost) {
				res.LeaseLost++
				d.leaseLost(ctx, ev)
				continue
			}
			return res, fmt.Errorf("mark published for event %d: %w", ev.ID, err)
		}
		res.Published++
	}
	return res, nil
}

func (d *Dispatcher) leaseLost(ctx context.Context, ev store.OutboxEvent) {
	d.cfg.Logger.Log(ctx, slog.LevelWarn, "outbox result discarded: lease lost", "event_id", ev.ID,
		"event_type", ev.EventType, "owner", d.cfg.Owner)
}

// publish runs the publisher with a per-event timeout. The in-flight publish
// is detached from shutdown (it finishes or times out, so its result can be
// recorded), and a publisher that ignores its context is abandoned at the
// deadline (its goroutine ends on its own) so one hung destination cannot
// stall the dispatcher past its lease.
func (d *Dispatcher) publish(ctx context.Context, ev store.OutboxEvent) error {
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.cfg.PublishTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("publisher panic: %v", r)
			}
		}()
		done <- d.pub.Publish(pctx, ev)
	}()
	select {
	case err := <-done:
		return err
	case <-pctx.Done():
		select {
		case err := <-done: // finished right at the deadline
			if err == nil {
				return nil
			}
		default:
		}
		return fmt.Errorf("%w after %s", ErrPublishTimeout, d.cfg.PublishTimeout)
	}
}

// Run polls until ctx is cancelled, then returns nil. A full batch triggers an
// immediate next pass; otherwise it waits PollInterval.
func (d *Dispatcher) Run(ctx context.Context) error {
	d.cfg.Logger.Info("outbox dispatcher started", "poll_interval", d.cfg.PollInterval, "batch_size", d.cfg.BatchSize)
	defer d.cfg.Logger.Info("outbox dispatcher stopped")
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		res, err := d.RunOnce(ctx)
		wait := d.cfg.PollInterval
		switch {
		case err != nil && !errors.Is(err, context.Canceled):
			d.cfg.Logger.Error("outbox dispatch pass failed", "error", err)
		case err == nil && res.Published+res.Retried >= d.cfg.BatchSize:
			wait = 0
		}
		timer.Reset(wait)
	}
}
