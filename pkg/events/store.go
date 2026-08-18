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
	// LIMIT rides as a bind parameter (not string-concatenated) so the query text
	// stays a constant — avoids gosec G202 and keeps limit non-injectable.
	var args []any
	if limit > 0 {
		q += " LIMIT $1"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
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

// ---------------------------------------------------------------------------
// Run-event read side — the §4.4 SSE projection (ISI-2756).
//
// The apiserver SSE hub fans run progress to every live console surface. Its
// source is this read-only view of the run-entity rows on coord.outbox: the SAME
// durable journal the relay publishes to NATS, read directly so the console
// transport needs no NATS client. This is a downstream projection (§17.4): it
// only SELECTs, never writes the outbox, and never re-enters coordination.
//
// The row id — a bigserial, monotonic in commit order — is the SSE event id, so
// a reconnecting client's Last-Event-ID resumes exactly from the last row it saw
// (RunEventsForRun), while the live tail advances a single watermark over all
// runs (RunEventsAfter). run_id keys the hub fan-out.
// ---------------------------------------------------------------------------

// defaultRunEventLimit bounds a single run-event scan (live tail or replay) so a
// large backlog can neither block the poll loop nor flood a reconnecting stream
// in one shot; the remainder is taken on the next id-ordered scan.
const defaultRunEventLimit = 1000

// RunEvent is one run-entity outbox row projected to the SSE hub. ID (the
// bigserial row id) is the SSE event id / Last-Event-ID resume key; RunID keys
// the hub fan-out; EventType is the SSE event name; Payload is the jsonb body.
type RunEvent struct {
	ID        int64
	RunID     string
	EventType string
	Payload   []byte
}

// RunEventReader reads run-entity events from coord.outbox for the apiserver SSE
// projector. Both scans are ascending by id (the monotonic SSE event id) so a
// caller resumes exactly from the last id it saw. Read-only (§17.4): it never
// writes the outbox and nothing it returns re-enters coordination.
type RunEventReader interface {
	// LatestRunEventID returns the highest run-entity outbox id (0 when none).
	// The live-tail projector seeds its watermark with this at startup so it fans
	// only NEW events forward, leaving history to per-run Last-Event-ID replay.
	LatestRunEventID(ctx context.Context) (int64, error)
	// RunEventsAfter returns run-entity rows with id > afterID, ascending, up to
	// limit (<=0 ⇒ defaultRunEventLimit). The live-tail feed: the projector fans
	// each row to the hub keyed by run_id.
	RunEventsAfter(ctx context.Context, afterID int64, limit int) ([]RunEvent, error)
	// RunEventsForRun returns one run's rows with id > afterID, ascending, up to
	// limit. The replay feed for a reconnecting client's Last-Event-ID. A runID
	// that is not a uuid matches nothing (empty, no error) — replay is best-effort.
	RunEventsForRun(ctx context.Context, runID string, afterID int64, limit int) ([]RunEvent, error)
}

// LatestRunEventID reports the high-water mark of run-entity outbox ids.
func (s *SQLStore) LatestRunEventID(ctx context.Context) (int64, error) {
	var id int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(id), 0) FROM coord.outbox WHERE entity = 'run'`).
		Scan(&id); err != nil {
		return 0, fmt.Errorf("events.SQLStore.LatestRunEventID: %w", err)
	}
	return id, nil
}

// RunEventsAfter scans the run-entity tail (id > afterID) in id order. Rows with
// a NULL run_id are skipped — they cannot key a hub fan-out.
func (s *SQLStore) RunEventsAfter(ctx context.Context, afterID int64, limit int) ([]RunEvent, error) {
	if limit <= 0 {
		limit = defaultRunEventLimit
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id::text, event_type, payload
		  FROM coord.outbox
		 WHERE entity = 'run' AND run_id IS NOT NULL AND id > $1
		 ORDER BY id
		 LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("events.SQLStore.RunEventsAfter: %w", err)
	}
	return scanRunEvents(rows, "RunEventsAfter")
}

// RunEventsForRun scans one run's tail (id > afterID) in id order. runID must be
// a uuid (the outbox column type); a non-uuid raises a cast error the caller
// treats as an empty best-effort replay rather than a stream failure.
func (s *SQLStore) RunEventsForRun(ctx context.Context, runID string, afterID int64, limit int) ([]RunEvent, error) {
	if limit <= 0 {
		limit = defaultRunEventLimit
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id::text, event_type, payload
		  FROM coord.outbox
		 WHERE entity = 'run' AND run_id = $1::uuid AND id > $2
		 ORDER BY id
		 LIMIT $3`, runID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("events.SQLStore.RunEventsForRun(%s): %w", runID, err)
	}
	return scanRunEvents(rows, "RunEventsForRun")
}

// scanRunEvents drains a run-event result set, copying each payload so it does
// not alias the driver's row buffer after the next Scan.
func scanRunEvents(rows *sql.Rows, where string) ([]RunEvent, error) {
	defer func() { _ = rows.Close() }()
	var out []RunEvent
	for rows.Next() {
		var r RunEvent
		var payload []byte
		if err := rows.Scan(&r.ID, &r.RunID, &r.EventType, &payload); err != nil {
			return nil, fmt.Errorf("events.SQLStore.%s: scan: %w", where, err)
		}
		r.Payload = append([]byte(nil), payload...)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events.SQLStore.%s: rows: %w", where, err)
	}
	return out, nil
}
