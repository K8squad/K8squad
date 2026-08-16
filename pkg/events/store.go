package events

import (
	"context"
	"database/sql"
	"fmt"
)

// OutboxRow is one unflushed event the relay is about to publish. Only the
// fields the relay needs to compose a subject and stamp the flush are read; the
// payload rides along as the message body.
type OutboxRow struct {
	ID        int64
	Entity    string
	ProjectID string
	Squad     string // "" when the column is NULL
	EventType string
	Payload   []byte
}

// OutboxStore is the relay's view of coord.outbox: read the unflushed backlog in
// scan order and stamp a row published. Extracting it as an interface lets the
// relay logic (relay.go) be unit-tested against an in-memory fake with no
// Postgres, exactly as the falsification bench models the Store — while the
// production SQLStore binds it to the real table.
type OutboxStore interface {
	// Unpublished returns rows with published_at IS NULL, ascending by id (the
	// monotonic relay scan order), up to limit (<=0 ⇒ no cap).
	Unpublished(ctx context.Context, limit int) ([]OutboxRow, error)
	// MarkPublished stamps published_at for id. The schema's set-once guard makes
	// a re-stamp of an already-published row a no-op error path, so the relay is
	// idempotent under republish.
	MarkPublished(ctx context.Context, id int64) error
	// Depth reports total rows (outbox_depth §17.2) and unflushed rows
	// (unflushed_lag §17.2) in one round trip for the observability gauges.
	Depth(ctx context.Context) (total, unflushed int64, err error)
}

// SQLStore binds OutboxStore to coord.outbox on a *sql.DB.
type SQLStore struct{ db *sql.DB }

// NewSQLStore returns an OutboxStore over db.
func NewSQLStore(db *sql.DB) *SQLStore { return &SQLStore{db: db} }

// Unpublished scans the partial-indexed backlog (idx_outbox_unpublished) in id
// order. squad is read through COALESCE-to-"" so a NULL team_id surfaces as the
// empty string the subject composer maps to the "_" token.
func (s *SQLStore) Unpublished(ctx context.Context, limit int) ([]OutboxRow, error) {
	q := `SELECT id, entity, project_id::text, COALESCE(squad, ''), event_type, payload
	        FROM coord.outbox
	       WHERE published_at IS NULL
	       ORDER BY id`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("events.SQLStore.Unpublished: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []OutboxRow
	for rows.Next() {
		var r OutboxRow
		if err := rows.Scan(&r.ID, &r.Entity, &r.ProjectID, &r.Squad, &r.EventType, &r.Payload); err != nil {
			return nil, fmt.Errorf("events.SQLStore.Unpublished: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events.SQLStore.Unpublished: rows: %w", err)
	}
	return out, nil
}

// MarkPublished stamps published_at=now() for a still-unflushed row. The
// WHERE published_at IS NULL clause keeps the UPDATE idempotent (a concurrent or
// repeated flush affects zero rows instead of tripping the set-once guard) —
// republishing a row already marked is a no-op, not an error.
func (s *SQLStore) MarkPublished(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE coord.outbox SET published_at = now() WHERE id = $1 AND published_at IS NULL`,
		id); err != nil {
		return fmt.Errorf("events.SQLStore.MarkPublished(%d): %w", id, err)
	}
	return nil
}

// Depth reads both observability counts in one statement.
func (s *SQLStore) Depth(ctx context.Context) (total, unflushed int64, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT count(*), count(*) FILTER (WHERE published_at IS NULL) FROM coord.outbox`).
		Scan(&total, &unflushed)
	if err != nil {
		return 0, 0, fmt.Errorf("events.SQLStore.Depth: %w", err)
	}
	return total, unflushed, nil
}
