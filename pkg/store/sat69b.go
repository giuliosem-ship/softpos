package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"softpos/pkg/identity"
)

// SAT 69-B list ingestion over PostgreSQL (MVP-20, migration 015).
// SQLBlacklistRepository implements identity.List69BStore: ReplaceList
// fills sat_blacklist_staging and swaps it into sat_blacklist inside one
// transaction (MVCC: concurrent lookups keep reading the previous list until
// COMMIT), serialized by a row lock on sat_blacklist_meta.

var _ identity.List69BStore = (*SQLBlacklistRepository)(nil)

const list69BInsertBatch = 500

func (r *SQLBlacklistRepository) ReplaceList(ctx context.Context, rows []identity.List69BRow, meta identity.List69BMeta, guard identity.ReplaceGuard, actor string) (load identity.List69BLoad, err error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return load, fmt.Errorf("begin 69-B swap: %w", err)
	}
	defer func() {
		if err != nil && !errors.Is(err, identity.ErrList69BShrank) && !errors.Is(err, identity.ErrList69BChanged) {
			_ = tx.Rollback()
		}
	}()
	// Serialize loaders: lock the meta row (or the table when none exists yet).
	if _, err = tx.ExecContext(ctx, `LOCK TABLE sat_blacklist_meta IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return load, fmt.Errorf("lock 69-B meta: %w", err)
	}
	prev := 0
	if err = tx.QueryRowContext(ctx, `SELECT row_count FROM sat_blacklist_meta WHERE id = 1`).Scan(&prev); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return load, fmt.Errorf("read 69-B meta: %w", err)
	}
	ingested := !errors.Is(err, sql.ErrNoRows)
	if !ingested {
		// Rows written before the ingestion existed (AddEntry) count as the old list.
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sat_blacklist`).Scan(&prev); err != nil {
			return load, fmt.Errorf("count 69-B rows: %w", err)
		}
	}
	load = identity.List69BLoad{Version: meta.Version, Source: truncate(meta.Source, 512), RowCount: len(rows), PreviousCount: prev,
		MalformedCount: meta.MalformedCount, Actor: truncate(actor, 128), Override: guard.Override, OverrideChanges: guard.OverrideChanges,
		CreatedAt: meta.FetchedAt.UTC()}
	if !guard.Allows(prev, len(rows)) {
		load.Status, load.Detail = identity.Load69BRejectedShrink, fmt.Sprintf("%v: %d -> %d rows (max %d%%)", identity.ErrList69BShrank, prev, len(rows), guard.MaxShrinkPct)
		if load.ID, err = insertLoad(ctx, tx, load); err != nil {
			_ = tx.Rollback()
			return load, err
		}
		if err = tx.Commit(); err != nil {
			return load, fmt.Errorf("commit 69-B rejection: %w", err)
		}
		return load, identity.ErrList69BShrank
	}

	if _, err = tx.ExecContext(ctx, `TRUNCATE sat_blacklist_staging`); err != nil {
		return load, fmt.Errorf("truncate 69-B staging: %w", err)
	}
	for start := 0; start < len(rows); start += list69BInsertBatch {
		end := min(start+list69BInsertBatch, len(rows))
		var sb strings.Builder
		args := make([]any, 0, (end-start)*4)
		sb.WriteString(`INSERT INTO sat_blacklist_staging (rfc, status, oficio_sat, nombre) VALUES `)
		for i, row := range rows[start:end] {
			if i > 0 {
				sb.WriteByte(',')
			}
			n := len(args)
			fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d)", n+1, n+2, n+3, n+4)
			args = append(args, row.RFC, string(row.Status), truncate(row.Oficio, 128), truncate(row.Name, 255))
		}
		sb.WriteString(` ON CONFLICT (rfc) DO UPDATE SET status = EXCLUDED.status, oficio_sat = EXCLUDED.oficio_sat, nombre = EXCLUDED.nombre`)
		if _, err = tx.ExecContext(ctx, sb.String(), args...); err != nil {
			return load, fmt.Errorf("fill 69-B staging: %w", err)
		}
	}
	// Change protection: RFCs of the loaded list whose situation differs or
	// that were replaced (identity.GuardedChanges). Only against a list loaded
	// by the ingestion: pre-ingestion rows (AddEntry) are not a verified list.
	var loaded, situation, removed int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(*) FILTER (WHERE s.status <> b.status), COUNT(*) FILTER (WHERE s.rfc IS NULL)
		FROM sat_blacklist b LEFT JOIN sat_blacklist_staging s ON s.rfc = b.rfc`).Scan(&loaded, &situation, &removed); err != nil {
		return load, fmt.Errorf("compare 69-B lists: %w", err)
	}
	changed := identity.GuardedChanges(loaded, len(rows), situation, removed)
	if ingested && !guard.AllowsChanges(loaded, changed) {
		load.Status, load.Detail = identity.Load69BRejectedChanges, fmt.Sprintf("%v: %d of %d loaded RFCs changed or replaced (max %d%%)", identity.ErrList69BChanged, changed, loaded, guard.MaxChangePct)
		if _, err = tx.ExecContext(ctx, `TRUNCATE sat_blacklist_staging`); err != nil {
			_ = tx.Rollback()
			return load, fmt.Errorf("truncate 69-B staging: %w", err)
		}
		if load.ID, err = insertLoad(ctx, tx, load); err != nil {
			_ = tx.Rollback()
			return load, err
		}
		if err = tx.Commit(); err != nil {
			return load, fmt.Errorf("commit 69-B rejection: %w", err)
		}
		return load, identity.ErrList69BChanged
	}
	now := meta.FetchedAt.UTC()
	if _, err = tx.ExecContext(ctx, `DELETE FROM sat_blacklist`); err != nil {
		return load, fmt.Errorf("clear 69-B list: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO sat_blacklist (rfc, status, oficio_sat, nombre, updated_at)
		SELECT rfc, status, oficio_sat, nombre, $1 FROM sat_blacklist_staging`, now); err != nil {
		return load, fmt.Errorf("swap 69-B list: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `TRUNCATE sat_blacklist_staging`); err != nil {
		return load, fmt.Errorf("truncate 69-B staging: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO sat_blacklist_meta (id, list_version, source, published, row_count, malformed_count, fetched_at, verified_at, loaded_by)
		VALUES (1, $1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (id) DO UPDATE SET list_version = EXCLUDED.list_version, source = EXCLUDED.source, published = EXCLUDED.published,
			row_count = EXCLUDED.row_count, malformed_count = EXCLUDED.malformed_count, fetched_at = EXCLUDED.fetched_at,
			verified_at = EXCLUDED.verified_at, loaded_by = EXCLUDED.loaded_by`,
		meta.Version, truncate(meta.Source, 512), truncate(meta.Published, 128), len(rows), meta.MalformedCount, now, meta.VerifiedAt.UTC(), truncate(actor, 128)); err != nil {
		return load, fmt.Errorf("write 69-B meta: %w", err)
	}
	load.Status = identity.Load69BLoaded
	if load.ID, err = insertLoad(ctx, tx, load); err != nil {
		return load, err
	}
	if err = tx.Commit(); err != nil {
		return load, fmt.Errorf("commit 69-B swap: %w", err)
	}
	return load, nil
}

type execQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func insertLoad(ctx context.Context, q execQuerier, l identity.List69BLoad) (int64, error) {
	var id int64
	created := l.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	err := q.QueryRowContext(ctx, `INSERT INTO sat_blacklist_loads
		(status, list_version, source, row_count, previous_count, malformed_count, override_shrink, override_changes, detail, actor, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING id`,
		l.Status, l.Version, truncate(l.Source, 512), l.RowCount, l.PreviousCount, l.MalformedCount, l.Override, l.OverrideChanges, l.Detail, truncate(l.Actor, 128), created.UTC()).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("record 69-B load: %w", err)
	}
	return id, nil
}

func (r *SQLBlacklistRepository) List69BMeta(ctx context.Context) (identity.List69BMeta, bool, error) {
	var m identity.List69BMeta
	err := r.db.QueryRowContext(ctx, `SELECT list_version, source, published, row_count, malformed_count, fetched_at, verified_at, loaded_by
		FROM sat_blacklist_meta WHERE id = 1`).Scan(&m.Version, &m.Source, &m.Published, &m.RowCount, &m.MalformedCount, &m.FetchedAt, &m.VerifiedAt, &m.LoadedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.List69BMeta{}, false, nil
	}
	if err != nil {
		return identity.List69BMeta{}, false, fmt.Errorf("read 69-B meta: %w", err)
	}
	m.FetchedAt, m.VerifiedAt = m.FetchedAt.UTC(), m.VerifiedAt.UTC()
	return m, true, nil
}

func (r *SQLBlacklistRepository) MarkVerified(ctx context.Context, version string, at time.Time, actor string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var count int
	err = tx.QueryRowContext(ctx, `UPDATE sat_blacklist_meta SET verified_at = $2 WHERE id = 1 AND list_version = $1 RETURNING row_count`, version, at.UTC()).Scan(&count)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // another version was loaded meanwhile
	}
	if err != nil {
		return fmt.Errorf("mark 69-B verified: %w", err)
	}
	if _, err := insertLoad(ctx, tx, identity.List69BLoad{Status: identity.Load69BUnchanged, Version: version, RowCount: count,
		PreviousCount: count, Actor: actor, CreatedAt: at}); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *SQLBlacklistRepository) RecordLoadFailure(ctx context.Context, l identity.List69BLoad) error {
	_, err := insertLoad(ctx, r.db, l)
	return err
}

func (r *SQLBlacklistRepository) ListLoads(ctx context.Context, limit int) ([]identity.List69BLoad, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id, status, list_version, source, row_count, previous_count, malformed_count, override_shrink, override_changes, detail, actor, created_at
		FROM sat_blacklist_loads ORDER BY created_at DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list 69-B loads: %w", err)
	}
	defer rows.Close()
	var out []identity.List69BLoad
	for rows.Next() {
		var l identity.List69BLoad
		if err := rows.Scan(&l.ID, &l.Status, &l.Version, &l.Source, &l.RowCount, &l.PreviousCount, &l.MalformedCount, &l.Override, &l.OverrideChanges, &l.Detail, &l.Actor, &l.CreatedAt); err != nil {
			return nil, err
		}
		l.CreatedAt = l.CreatedAt.UTC()
		out = append(out, l)
	}
	return out, rows.Err()
}

// truncate cuts s to at most n runes (VARCHAR limits count characters).
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
