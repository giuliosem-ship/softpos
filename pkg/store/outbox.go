package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// OutboxEvent represents an asynchronous event stored atomically alongside business transactions.
type OutboxEvent struct {
	ID          int64           `json:"id"`
	EventType   string          `json:"event_type"`
	AggregateID string          `json:"aggregate_id"`
	PayloadJSON json.RawMessage `json:"payload_json"`
	Status      string          `json:"status"` // PENDING, PROCESSING, PUBLISHED, FAILED
	RetryCount  int             `json:"retry_count"`
	MaxRetries  int             `json:"max_retries"`
	LastError   string          `json:"last_error"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// SQLOutboxRepository implements the transactional outbox pattern on PostgreSQL.
type SQLOutboxRepository struct {
	db *sql.DB
}

func NewSQLOutboxRepository(db *sql.DB) *SQLOutboxRepository {
	return &SQLOutboxRepository{db: db}
}

// SaveEventTx records an outbox event inside an existing database transaction.
func (r *SQLOutboxRepository) SaveEventTx(
	ctx context.Context,
	tx *sql.Tx,
	eventType string,
	aggregateID string,
	payload any,
) error {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal outbox event payload: %w", err)
	}

	now := time.Now().UTC()
	query := `
		INSERT INTO outbox_events (
			event_type, aggregate_id, payload_json, status,
			retry_count, max_retries, last_error, created_at, updated_at
		) VALUES ($1, $2, $3, 'PENDING', 0, 5, '', $4, $4)
	`

	_, err = tx.ExecContext(ctx, query, eventType, aggregateID, payloadBytes, now)
	if err != nil {
		return fmt.Errorf("failed to save outbox event in tx: %w", err)
	}
	return nil
}

// FetchPendingEventsWithLock retrieves pending events up to limit.
func (r *SQLOutboxRepository) FetchPendingEventsWithLock(ctx context.Context, limit int) ([]OutboxEvent, error) {
	if limit <= 0 {
		limit = 10
	}

	query := `
		SELECT id, event_type, aggregate_id, payload_json, status,
		       retry_count, max_retries, last_error, created_at, updated_at
		FROM outbox_events
		WHERE status = 'PENDING' AND retry_count < max_retries
		ORDER BY id ASC
		LIMIT $1
	`

	rows, err := r.db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch pending outbox events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var events []OutboxEvent
	for rows.Next() {
		var ev OutboxEvent
		var payload []byte
		if err := rows.Scan(
			&ev.ID,
			&ev.EventType,
			&ev.AggregateID,
			&payload,
			&ev.Status,
			&ev.RetryCount,
			&ev.MaxRetries,
			&ev.LastError,
			&ev.CreatedAt,
			&ev.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan outbox event: %w", err)
		}
		ev.PayloadJSON = payload
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

// MarkPublished marks an event as successfully delivered.
func (r *SQLOutboxRepository) MarkPublished(ctx context.Context, eventID int64) error {
	now := time.Now().UTC()
	query := `UPDATE outbox_events SET status = 'PUBLISHED', updated_at = $1 WHERE id = $2`
	_, err := r.db.ExecContext(ctx, query, now, eventID)
	if err != nil {
		return fmt.Errorf("failed to mark outbox event published: %w", err)
	}
	return nil
}

// MarkFailed increments retry_count and sets error message; transitions to FAILED if max retries exceeded.
func (r *SQLOutboxRepository) MarkFailed(ctx context.Context, eventID int64, errMsg string) error {
	now := time.Now().UTC()
	query := `
		UPDATE outbox_events
		SET retry_count = retry_count + 1,
		    last_error = $1,
		    status = CASE WHEN retry_count + 1 >= max_retries THEN 'FAILED' ELSE 'PENDING' END,
		    updated_at = $2
		WHERE id = $3
	`
	_, err := r.db.ExecContext(ctx, query, errMsg, now, eventID)
	if err != nil {
		return fmt.Errorf("failed to update outbox event failure: %w", err)
	}
	return nil
}

// ClaimDueEvents atomically claims up to limit events for dispatch and marks
// them PROCESSING. An event is due when it is PENDING with next_attempt_at <= now,
// or PROCESSING with a lease older than lease (its dispatcher crashed). Rows are
// locked with FOR UPDATE SKIP LOCKED, so concurrent dispatchers never claim the
// same event. Events are returned ordered by id.
func (r *SQLOutboxRepository) ClaimDueEvents(ctx context.Context, limit int, now time.Time, lease time.Duration) ([]OutboxEvent, error) {
	if limit <= 0 {
		limit = 10
	}
	now = now.UTC()
	rows, err := r.db.QueryContext(ctx, `
		UPDATE outbox_events
		SET status = 'PROCESSING', updated_at = $1
		WHERE id IN (
			SELECT id FROM outbox_events
			WHERE retry_count < max_retries
			  AND ((status = 'PENDING' AND next_attempt_at <= $1)
			    OR (status = 'PROCESSING' AND updated_at <= $2))
			ORDER BY id ASC
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, event_type, aggregate_id, payload_json, status,
		          retry_count, max_retries, last_error, created_at, updated_at
	`, now, now.Add(-lease), limit)
	if err != nil {
		return nil, fmt.Errorf("failed to claim outbox events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var events []OutboxEvent
	for rows.Next() {
		var ev OutboxEvent
		var payload []byte
		if err := rows.Scan(&ev.ID, &ev.EventType, &ev.AggregateID, &payload, &ev.Status,
			&ev.RetryCount, &ev.MaxRetries, &ev.LastError, &ev.CreatedAt, &ev.UpdatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan claimed outbox event: %w", err)
		}
		ev.PayloadJSON = payload
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(events, func(i, j int) bool { return events[i].ID < events[j].ID })
	return events, nil
}

// MarkRetry records a failed delivery attempt: increments retry_count, stores
// the error and schedules the next attempt; the event becomes FAILED (dead
// letter) once retry_count reaches max_retries.
func (r *SQLOutboxRepository) MarkRetry(ctx context.Context, eventID int64, errMsg string, nextAttemptAt time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE outbox_events
		SET retry_count = retry_count + 1,
		    last_error = $1,
		    status = CASE WHEN retry_count + 1 >= max_retries THEN 'FAILED' ELSE 'PENDING' END,
		    next_attempt_at = $2,
		    updated_at = $3
		WHERE id = $4
	`, errMsg, nextAttemptAt.UTC(), time.Now().UTC(), eventID)
	if err != nil {
		return fmt.Errorf("failed to schedule outbox retry: %w", err)
	}
	return nil
}
