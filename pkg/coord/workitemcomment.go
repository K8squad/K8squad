// workitemcomment.go — Story ISI-4406: the HUMAN work-item comment write op, the
// Team-fenced sibling of the agent-only run-token path (pkg/taskio → the free
// coord.AppendComment primitive in taskdetail.go).
//
// coord.AppendComment is a pure INSERT primitive: it takes no teamID and does NO
// tenancy check, because its only caller (the agent post-comment path) has already
// been authorised by a run token bound to that very work item. The human console
// path has no such binding — it is keyed by item id behind the §13 BFF choke point,
// exactly like the field-edit and lane-move ops — so it MUST fence tenancy itself or
// it would leak the existence of another Team's item. This method is that fence: it
// locks + reads the item, refuses a cross-tenant id as ErrWorkItemNotFound (404,
// existence-hiding, never a 403), then appends the comment and a §6.5
// 'work_item_commented' audit row in the SAME transaction. The author is
// server-supplied (the session principal), never client text — identical to every
// other write on this store.
package coord

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AppendHumanComment appends one human-authored comment to a work item under a Team
// fence and records its §6.5 audit provenance atomically.
//
// Semantics (mirroring UpdateWorkItem so the whole human write surface answers one
// shape):
//   - (comment, nil): the comment is persisted; Author is the server-stamped principal.
//   - (zero, ErrInvalidWorkItem): workItemID/principal missing, or an empty body (400).
//   - (zero, ErrWorkItemNotFound): no such item, or one outside teamID — existence-
//     hiding (404). Pass teamID == "" only for a trusted, already-tenancy-checked
//     caller (fleet-admin, ISI-3937), exactly like UpdateWorkItem.
//   - (zero, err): infrastructure failure; nothing was written.
func (s *WorkItemWriteStore) AppendHumanComment(ctx context.Context, workItemID, teamID, principal, body string) (TaskComment, error) {
	if workItemID == "" || principal == "" {
		return TaskComment{}, fmt.Errorf("%w: workItemID and principal are required", ErrInvalidWorkItem)
	}
	if body == "" {
		return TaskComment{}, fmt.Errorf("%w: comment body may not be empty", ErrInvalidWorkItem)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TaskComment{}, fmt.Errorf("coord.AppendHumanComment: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Tenancy fence (§12.1): lock + read the target; an item outside the caller's
	// Team is 404 (existence-hiding), never a cross-tenant 403 — the same wall the
	// field-edit and lane-move ops present. FOR UPDATE serialises against a racing
	// delete/reparent so the FK the INSERT depends on cannot vanish mid-txn.
	var teamOut sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT team_id::text FROM coord.work_item WHERE id = $1::uuid FOR UPDATE`,
		workItemID).Scan(&teamOut)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return TaskComment{}, ErrWorkItemNotFound
	case err != nil:
		return TaskComment{}, fmt.Errorf("coord.AppendHumanComment: read item: %w", err)
	}
	if teamID != "" && (!teamOut.Valid || teamOut.String != teamID) {
		return TaskComment{}, ErrWorkItemNotFound
	}

	var created time.Time
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO coord.comment (work_item_id, author_principal, body)
		VALUES ($1::uuid, $2, $3)
		RETURNING created_at`, workItemID, principal, body).Scan(&created); err != nil {
		return TaskComment{}, fmt.Errorf("coord.AppendHumanComment: insert: %w", err)
	}

	// §6.5 provenance in the same txn, fence NULL (ADR-037: a comment is neither a
	// custody nor a lease op) — the audit trail records WHO commented, the body
	// itself lives in the append-only coord.comment row.
	if err := s.writeAudit(ctx, tx, workItemID, "work_item_commented", principal, "", map[string]any{}); err != nil {
		return TaskComment{}, err
	}
	if err := tx.Commit(); err != nil {
		return TaskComment{}, fmt.Errorf("coord.AppendHumanComment: commit: %w", err)
	}
	return TaskComment{Author: principal, Body: body, CreatedAt: created}, nil
}
