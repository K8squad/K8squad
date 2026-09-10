// changeref.go — M1.5 (ISI-4131): the agent-authored CHANGE-REPORT write path
// and its read side. Together with the existing post-comment (taskdetail.go
// AppendComment) and update-status (humanstate.go AgentTransitionState) paths,
// this completes the reporting half of the M1 core loop: a run's ticket shows
// agent-authored comments + status changes + change refs (commit SHAs / PR
// links) without human relay (epic ISI-4126).
//
// A change ref is APPEND-ONLY provenance, exactly like a comment (§6.1/§6.5):
// a fact the run reported at a point in time, never edited or deleted. The
// storage (db/migrations/0015_work_item_change_ref.sql, coord.change_ref)
// enforces that structurally with the shared reject_mutation trigger.
//
// It is deliberately NOT an artifact row: coord.artifact is content-addressed
// OUTPUT (sha256 NOT NULL, one row per (work_item, run, kind)); a run reports
// MANY commits/PRs and a PR link carries no sha — folding them there would lie
// about both surfaces.
package coord

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Change-ref kinds — the epic's "commits/PR links" (a tight enum; widening is
// a forward migration, never a silent widening).
const (
	ChangeKindCommit      = "commit"
	ChangeKindPullRequest = "pull_request"
)

// ErrInvalidChangeRef — the change-report input is malformed (empty ref, or a
// kind outside the enum). Maps to 400 on the task-io surface.
var ErrInvalidChangeRef = errors.New("coord: invalid change ref")

// ValidChangeKinds is the closed set of coord.change_ref.kind values.
var ValidChangeKinds = map[string]bool{
	ChangeKindCommit:      true,
	ChangeKindPullRequest: true,
}

// ChangeRef is one agent-reported change reference on a work item
// (coord.change_ref), in chronological order.
type ChangeRef struct {
	Kind      string    `json:"kind"`              // 'commit' | 'pull_request'
	Ref       string    `json:"ref"`               // commit SHA or PR URL
	Summary   string    `json:"summary,omitempty"` // one-line what-changed note
	Author    string    `json:"author"`            // server-supplied token principal
	RunID     string    `json:"runId,omitempty"`   // reporting Run (provenance)
	CreatedAt time.Time `json:"createdAt"`
}

// AppendChangeRef appends one provenanced change ref to a work item, atomically
// with a §6.5 'change_reported' audit row (fence NULL, ADR-037: reporting is
// not a custody operation; initiator "agent", mirroring AgentTransitionState).
// This is the SANCTIONED change-report write path — a plain INSERT into the
// append-only coord.change_ref table. The author is server-supplied (from the
// run token's principal), never client text. runID may be empty (provenance is
// best-effort; the task-io surface always passes the token's Run).
//
// Semantics:
//   - (ref, nil): the change ref exists; the audit row is committed with it.
//   - (zero, ErrInvalidChangeRef): empty workItemID/author/ref, or a kind
//     outside ValidChangeKinds (400).
//   - (zero, err): infrastructure failure (incl. a dangling work item tripping
//     the FK); nothing was written.
func AppendChangeRef(ctx context.Context, db *sql.DB, workItemID, author, runID, kind, ref, summary string) (ChangeRef, error) {
	if db == nil {
		return ChangeRef{}, errors.New("coord.AppendChangeRef: nil db")
	}
	if workItemID == "" || author == "" || ref == "" {
		return ChangeRef{}, fmt.Errorf("%w: workItemID, author and ref are required", ErrInvalidChangeRef)
	}
	if !ValidChangeKinds[kind] {
		return ChangeRef{}, fmt.Errorf("%w: kind %q not in (commit, pull_request)", ErrInvalidChangeRef, kind)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return ChangeRef{}, fmt.Errorf("coord.AppendChangeRef: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	runParam := sql.NullString{}
	if runID != "" {
		runParam = sql.NullString{String: runID, Valid: true}
	}
	summaryParam := sql.NullString{}
	if summary != "" {
		summaryParam = sql.NullString{String: summary, Valid: true}
	}

	out := ChangeRef{Kind: kind, Ref: ref, Summary: summary, Author: author, RunID: runID}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO coord.change_ref (work_item_id, run_id, kind, ref, summary, author_principal)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6)
		RETURNING created_at`,
		workItemID, runParam, kind, ref, summaryParam, author).Scan(&out.CreatedAt)
	if err != nil {
		// A dangling work item trips the FK (ON DELETE RESTRICT / missing parent).
		return ChangeRef{}, fmt.Errorf("coord.AppendChangeRef: insert change ref for %s: %w", workItemID, err)
	}

	// §6.5 audit provenance, same transaction; fence NULL (ADR-037), initiator
	// "agent" — this is the run reporting its own change summary.
	payload, err := json.Marshal(map[string]any{
		"initiator": "agent",
		"kind":      kind,
		"ref":       ref,
		"summary":   summary,
	})
	if err != nil {
		return ChangeRef{}, fmt.Errorf("coord.AppendChangeRef: audit payload: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coord.audit_log (work_item_id, run_id, event_type, principal, payload)
		VALUES ($1::uuid, $2::uuid, 'change_reported', $3, $4::jsonb)`,
		workItemID, runParam, author, string(payload)); err != nil {
		return ChangeRef{}, fmt.Errorf("coord.AppendChangeRef: audit: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return ChangeRef{}, fmt.Errorf("coord.AppendChangeRef: commit: %w", err)
	}
	return out, nil
}

// readChangeRefs loads the chronological change-ref thread for one work item —
// the change summary half of the richer read model (ReadTaskDetail).
func readChangeRefs(ctx context.Context, db *sql.DB, workItemID string) ([]ChangeRef, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT kind, ref, summary, author_principal, run_id::text, created_at
		  FROM coord.change_ref
		 WHERE work_item_id = $1::uuid
		 ORDER BY created_at ASC, id ASC`, workItemID)
	if err != nil {
		return nil, fmt.Errorf("coord.ReadTaskDetail: read change refs for %s: %w", workItemID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []ChangeRef
	for rows.Next() {
		var c ChangeRef
		var summary, runID sql.NullString
		if err := rows.Scan(&c.Kind, &c.Ref, &summary, &c.Author, &runID, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("coord.ReadTaskDetail: scan change ref: %w", err)
		}
		c.Summary = summary.String
		c.RunID = runID.String
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("coord.ReadTaskDetail: iterate change refs: %w", err)
	}
	return out, nil
}
