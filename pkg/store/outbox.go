package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
