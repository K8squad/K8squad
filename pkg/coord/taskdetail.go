// taskdetail.go — the SHARED richer work-item read model (ISI-3601 S2 task 1,
// designed once with ISI-3600 S1). Both consumers use this ONE read:
//
//   - S1's context assembler `Sources.WorkItem` (the push side — snapshots the
//     task into SystemContext at dispatch).
//   - S2's agent-facing `get-task` endpoint (the pull side — pkg/taskio, which
//     projects a TaskDetail onto its JSON wire shape).
//
// The existing dispatch read (sqlDispatchSource.WorkItem, rundrive) returns
// only title+body — deliberately thin for the v1 envelope. This read is the
// richer one both stories called for: title, description, board state,
// blocked-reason, the comment thread, and the current claim/fence state.
//
// AcceptanceCriteria and Goals are part of the agreed shape but have NO
// first-class column in the 0001 coord schema yet, so ReadTaskDetail leaves
// them nil. Wiring a first-class AC/goals surface (a column or a structured
// body convention) is tracked follow-up work and must land in exactly one
// place so both consumers pick it up — do NOT parse them ad hoc per caller.
package coord

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// TaskComment is one append-only note on a work item (coord.comment), in
// chronological order.
type TaskComment struct {
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
}

// TaskDetail is the canonical richer read of one work item and its coordination
// state. It is the single shared shape (S1 push / S2 pull) — see file header.
type TaskDetail struct {
	WorkItemID    string
	Title         string
	Description   string // coord.work_item.body
	State         string
	BlockedReason string
	// AcceptanceCriteria / Goals: agreed shape, not yet backed by a column
	// (nil today). See file header — one wiring site when the surface lands.
	AcceptanceCriteria []string
	Goals              []string
	Comments           []TaskComment
	// Claim/fence state (coord.claim). FenceToken is the §6.2 monotonic token
	// every artifact write is checked against; Holder is the current lease
	// holder principal (empty ⇒ unclaimed); RunID is the holding run.
	FenceToken int64
	Holder     string
	RunID      string
}

// ReadTaskDetail reads the richer detail for one work item. It is read-only and
// safe for concurrent use. ErrWorkItemNotFound if the item does not exist.
func ReadTaskDetail(ctx context.Context, db *sql.DB, workItemID string) (TaskDetail, error) {
	if db == nil {
		return TaskDetail{}, errors.New("coord.ReadTaskDetail: nil db")
	}
	if workItemID == "" {
		return TaskDetail{}, fmt.Errorf("coord.ReadTaskDetail: workItemID required")
	}

	var (
		td            TaskDetail
		body          sql.NullString
		blockedReason sql.NullString
		holder        sql.NullString
		runID         sql.NullString
		fence         sql.NullInt64
	)
	// One row: the item joined to its (always-present, 0001 trigger-provisioned)
	// claim row. LEFT JOIN keeps the read robust even if a claim row were ever
	// missing (reads as unclaimed/fence 0 rather than erroring).
	err := db.QueryRowContext(ctx, `
		SELECT wi.id::text, wi.title, wi.body, wi.state, wi.blocked_reason,
		       c.holder_principal, c.run_id::text, c.fence_token
		  FROM coord.work_item wi
		  LEFT JOIN coord.claim c ON c.work_item_id = wi.id
		 WHERE wi.id = $1::uuid`, workItemID).
		Scan(&td.WorkItemID, &td.Title, &body, &td.State, &blockedReason,
			&holder, &runID, &fence)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return TaskDetail{}, ErrWorkItemNotFound
	case err != nil:
		return TaskDetail{}, fmt.Errorf("coord.ReadTaskDetail: read work item %s: %w", workItemID, err)
	}
	td.Description = body.String
	td.BlockedReason = blockedReason.String
	td.Holder = holder.String
	td.RunID = runID.String
	td.FenceToken = fence.Int64

	comments, err := readComments(ctx, db, workItemID)
	if err != nil {
		return TaskDetail{}, err
	}
	td.Comments = comments
	return td, nil
}

func readComments(ctx context.Context, db *sql.DB, workItemID string) ([]TaskComment, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT author_principal, body, created_at
		  FROM coord.comment
		 WHERE work_item_id = $1::uuid
		 ORDER BY created_at ASC, id ASC`, workItemID)
	if err != nil {
		return nil, fmt.Errorf("coord.ReadTaskDetail: read comments for %s: %w", workItemID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []TaskComment
	for rows.Next() {
		var c TaskComment
		if err := rows.Scan(&c.Author, &c.Body, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("coord.ReadTaskDetail: scan comment: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("coord.ReadTaskDetail: iterate comments: %w", err)
	}
	return out, nil
}

// AppendComment appends one provenanced comment to a work item and returns it.
// This is the SANCTIONED comment-write path (§6.1) both S2's post-comment and
// any other agent write must use — a plain INSERT into the append-only
// coord.comment table (the reject_mutation trigger forbids edit/delete). The
// author is server-supplied (from the run token's principal), never client
// text. ErrWorkItemNotFound if the item does not exist (surfaced from the FK).
func AppendComment(ctx context.Context, db *sql.DB, workItemID, author, body string) (TaskComment, error) {
	if db == nil {
		return TaskComment{}, errors.New("coord.AppendComment: nil db")
	}
	if workItemID == "" || author == "" || body == "" {
		return TaskComment{}, fmt.Errorf("coord.AppendComment: workItemID, author and body are required")
	}
	var created time.Time
	err := db.QueryRowContext(ctx, `
		INSERT INTO coord.comment (work_item_id, author_principal, body)
		VALUES ($1::uuid, $2, $3)
		RETURNING created_at`, workItemID, author, body).Scan(&created)
	if err != nil {
		// A dangling work item trips the FK (ON DELETE RESTRICT / missing parent).
		return TaskComment{}, fmt.Errorf("coord.AppendComment: insert comment for %s: %w", workItemID, err)
	}
	return TaskComment{Author: author, Body: body, CreatedAt: created}, nil
}
