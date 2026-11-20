// Package outbox runs the transactional-outbox dispatcher: it polls
// outbox_events, hands each due event to a Publisher and records success or a
// retry with exponential backoff. Delivery is at-least-once; publishers must be
// idempotent on event ID.
package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"softpos/pkg/store"
)

// Source is the storage side of the dispatcher (store.SQLOutboxRepository).
type Source interface {
	ClaimDueEvents(ctx context.Context, limit int, now time.Time, lease time.Duration) ([]store.OutboxEvent, error)
	MarkPublished(ctx context.Context, eventID int64) error
	MarkRetry(ctx context.Context, eventID int64, errMsg string, nextAttemptAt time.Time) error
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
	Lease        time.Duration // default 5m; PROCESSING events older than this are reclaimed
	Logger       *slog.Logger
	Now          func() time.Time
}

const (
	DefaultPollInterval = 2 * time.Second
	DefaultBatchSize    = 50
	DefaultBaseBackoff  = time.Second
	DefaultMaxBackoff   = 5 * time.Minute
	DefaultLease        = 5 * time.Minute
)

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
}

// RunOnce claims one batch and processes it. When ctx is cancelled mid-batch,
// the current event is finished and the remaining claimed events are left
// PROCESSING; they are reclaimed after the lease expires.
func (d *Dispatcher) RunOnce(ctx context.Context) (Result, error) {
	var res Result
	events, err := d.src.ClaimDueEvents(ctx, d.cfg.BatchSize, d.cfg.Now(), d.cfg.Lease)
	if err != nil {
		return res, err
	}
	// Bookkeeping must be recorded even if shutdown cancels ctx.
	markCtx := context.WithoutCancel(ctx)
	for _, ev := range events {
		if ctx.Err() != nil {
			break
		}
		if pubErr := d.publish(ctx, ev); pubErr != nil {
			next := d.cfg.Now().Add(Backoff(ev.RetryCount, d.cfg.BaseBackoff, d.cfg.MaxBackoff))
			if err := d.src.MarkRetry(markCtx, ev.ID, pubErr.Error(), next); err != nil {
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
		if err := d.src.MarkPublished(markCtx, ev.ID); err != nil {
			return res, fmt.Errorf("mark published for event %d: %w", ev.ID, err)
		}
		res.Published++
	}
	return res, nil
}

func (d *Dispatcher) publish(ctx context.Context, ev store.OutboxEvent) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("publisher panic: %v", r)
		}
	}()
	return d.pub.Publish(ctx, ev)
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
