package events

import (
	"context"
	"database/sql"
	"fmt"
)

// Execer is the subset of *sql.Tx / *sql.DB that Capture needs. Callers pass
// their in-flight *sql.Tx so the outbox INSERT commits with the state change;
// passing a *sql.DB would emit the event in its own transaction (a dual-write
// hole) and is only ever appropriate for out-of-band backfills.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// QueryExecer additionally exposes QueryRowContext; CaptureForWorkItem needs it
// to derive project_id/squad from the work_item row in the same statement.
// *sql.Tx and *sql.DB both satisfy it.
type QueryExecer interface {
	Execer
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// captureInsert is the append-only outbox write. squad/work_item_id/run_id are
// nullable; payload defaults to an empty object (the column is NOT NULL). The
// row's occurred_at/published_at defaults are set by the schema (published_at
// starts NULL — the relay's set-once flush marker).
const captureInsert = `
	INSERT INTO coord.outbox
	       (entity, project_id, squad, event_type, work_item_id, run_id, payload)
	VALUES ($1, $2::uuid, $3, $4, $5::uuid, $6::uuid, $7::jsonb)`

// Capture appends ev to coord.outbox using the caller's transaction, so the
// event row and the state change commit as one atomic unit (AC-a / C1, §17.4).
//
// It performs exactly ONE INSERT and NEVER commits or rolls back tx — the
// caller owns the transaction boundary. If the enclosing transaction rolls
// back, the event vanishes with the state change (no phantom event); if it
// commits, the event is durable (no lost event). Pass an in-flight *sql.Tx;
// passing *sql.DB reintroduces the dual-write hole this seam exists to close.
func Capture(ctx context.Context, tx Execer, ev Event) error {
	if err := ev.validate(); err != nil {
		return err
	}
	payload := ev.Payload
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	if _, err := tx.ExecContext(ctx, captureInsert,
		ev.Entity, ev.ProjectID, nullable(ev.Squad), ev.EventType,
		nullable(ev.WorkItemID), nullable(ev.RunID), string(payload)); err != nil {
		return fmt.Errorf("events.Capture(%s/%s): %w", ev.Entity, ev.EventType, err)
	}
	return nil
}

// captureForWorkItem derives project_id and squad (team_id) from the work_item
// row so the §6.6 work-item write paths (claim/complete/handoff) don't have to
// carry tenancy fields they don't already hold. INSERT ... SELECT keeps it a
// single statement inside the caller's txn — the derivation and the append are
// atomic with the state change, and a missing work_item yields zero rows (a
// caught error) rather than a mis-tenanted event.
const captureForWorkItemInsert = `
	INSERT INTO coord.outbox
	       (entity, project_id, squad, event_type, work_item_id, run_id, payload)
	SELECT 'work_item', w.project_id, w.team_id, $2, w.id, $3::uuid, $4::jsonb
	  FROM coord.work_item w
	 WHERE w.id = $1::uuid
	RETURNING 1`

// CaptureForWorkItem appends a work_item-family event in the caller's
// transaction, deriving project_id and squad from coord.work_item(id) so the
// coordination write paths capture events without threading tenancy fields
// through their signatures. Returns an error if workItemID names no row (which
// would otherwise silently drop the event).
func CaptureForWorkItem(ctx context.Context, tx QueryExecer, workItemID, runID, eventType string, payload []byte) error {
	if workItemID == "" {
		return fmt.Errorf("events.CaptureForWorkItem: workItemID is required")
	}
	if eventType == "" {
		return fmt.Errorf("events.CaptureForWorkItem: eventType is required")
	}
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	var one int
	err := tx.QueryRowContext(ctx, captureForWorkItemInsert,
		workItemID, eventType, nullable(runID), string(payload)).Scan(&one)
	if err == sql.ErrNoRows {
		return fmt.Errorf("events.CaptureForWorkItem: work_item %s not found (no event captured)", workItemID)
	}
	if err != nil {
		return fmt.Errorf("events.CaptureForWorkItem(%s/%s): %w", workItemID, eventType, err)
	}
	return nil
}

// nullable maps "" to a SQL NULL so optional uuid/text columns store NULL rather
// than an empty string (which would fail a uuid cast and mis-token the subject).
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
