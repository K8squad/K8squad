package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/K8squad/K8squad/pkg/telemetry"
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
	       (entity, project_id, squad, event_type, work_item_id, run_id, payload, trace_carrier)
	VALUES ($1, $2::uuid, $3, $4, $5::uuid, $6::uuid, $7::jsonb, $8::jsonb)`

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
		nullable(ev.WorkItemID), nullable(ev.RunID), string(payload),
		traceCarrierJSON(ctx)); err != nil {
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
	       (entity, project_id, squad, event_type, work_item_id, run_id, payload, trace_carrier)
	SELECT 'work_item', w.project_id, w.team_id, $2, w.id, $3::uuid, $4::jsonb, $5::jsonb
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
		workItemID, eventType, nullable(runID), string(payload),
		traceCarrierJSON(ctx)).Scan(&one)
	if err == sql.ErrNoRows {
		return fmt.Errorf("events.CaptureForWorkItem: work_item %s not found (no event captured)", workItemID)
	}
	if err != nil {
		return fmt.Errorf("events.CaptureForWorkItem(%s/%s): %w", workItemID, eventType, err)
	}
	return nil
}

// captureRunForWorkItemInsert mirrors captureForWorkItemInsert — deriving
// project_id/squad from the work_item row so the caller need not thread tenancy
// fields — but stamps a RUN-entity event with run_id set. That distinction is
// load-bearing: the apiserver SSE projector fans only rows matching
// `entity='run' AND run_id IS NOT NULL` to the per-run hub (see
// events.SQLStore.RunEventsAfter), so a work_item-entity row would never reach
// the live run stream. Used by the operator's ProgressMirror to make the ticket
// Activity feed live on the EXISTING per-run EventSource (ISI-5193, plan
// ISI-5185/WS-A): the durable coord.comment row stays the source of truth, while
// this run event is the transient liveness signal AND the Last-Event-ID replay
// tail for a mid-run reconnect.
const captureRunForWorkItemInsert = `
	INSERT INTO coord.outbox
	       (entity, project_id, squad, event_type, work_item_id, run_id, payload, trace_carrier)
	SELECT 'run', w.project_id, w.team_id, $2, w.id, $3::uuid, $4::jsonb, $5::jsonb
	  FROM coord.work_item w
	 WHERE w.id = $1::uuid
	RETURNING 1`

// CaptureRunForWorkItem appends a run-entity outbox event correlated to
// workItemID, deriving project_id/squad from the work_item row (like
// CaptureForWorkItem) so a producer that already holds a work_item id — the
// operator's progress mirror — captures a run event without a separate tenancy
// lookup. runID is REQUIRED: a run event with a NULL run_id keys no SSE fan-out
// and the projector skips it, so an empty runID is a caller bug, not a NULL row.
// Returns an error if workItemID names no row (which would otherwise silently
// drop the event). Same transaction discipline as Capture: pass an in-flight
// *sql.Tx to commit atomically with the state change; a *sql.DB emits the event
// in its own transaction (acceptable only for a best-effort projection whose
// durable record is written separately, e.g. the progress mirror).
func CaptureRunForWorkItem(ctx context.Context, tx QueryExecer, workItemID, runID, eventType string, payload []byte) error {
	if workItemID == "" {
		return fmt.Errorf("events.CaptureRunForWorkItem: workItemID is required")
	}
	if runID == "" {
		return fmt.Errorf("events.CaptureRunForWorkItem: runID is required")
	}
	if eventType == "" {
		return fmt.Errorf("events.CaptureRunForWorkItem: eventType is required")
	}
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	var one int
	err := tx.QueryRowContext(ctx, captureRunForWorkItemInsert,
		workItemID, eventType, runID, string(payload),
		traceCarrierJSON(ctx)).Scan(&one)
	if err == sql.ErrNoRows {
		return fmt.Errorf("events.CaptureRunForWorkItem: work_item %s not found (no event captured)", workItemID)
	}
	if err != nil {
		return fmt.Errorf("events.CaptureRunForWorkItem(%s/%s): %w", workItemID, eventType, err)
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

// traceCarrierJSON serializes the active span context in ctx as a W3C carrier
// map ({"traceparent":…,"tracestate":…}) for the coord.outbox.trace_carrier
// column, so the relay can Extract it and make the NATS hop a span in the run
// trace (ISI-4440). It returns a SQL NULL — not "{}" — when ctx has no active
// span (Inject writes nothing), so a row captured off the trace stays cleanly
// NULL and the relay falls back to the payload trace_id / a fresh trace. Best
// effort by construction: a marshal error (never expected for a small string
// map) also yields NULL rather than failing the capture, because an outbox
// write must never depend on the tracer.
func traceCarrierJSON(ctx context.Context) any {
	carrier := map[string]string{}
	telemetry.Inject(ctx, carrier)
	if len(carrier) == 0 {
		return nil
	}
	b, err := json.Marshal(carrier)
	if err != nil {
		return nil
	}
	return string(b)
}
