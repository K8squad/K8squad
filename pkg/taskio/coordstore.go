package taskio

import (
	"context"
	"database/sql"
	"errors"

	"github.com/K8squad/K8squad/pkg/coord"
)

// CoordStore is the production Store: it binds the four own-run task-io actions
// onto the coord Postgres schema.
//
//   - GetTask / PostComment use the SHARED richer read/write (coord.ReadTaskDetail
//     / coord.AppendComment) — the same read S1's assembler consumes (ISI-3600),
//     designed once and never duplicated.
//   - UpdateStatus uses the AGENT-initiated board transition (audit provenance
//     initiator="agent", not "human").
//   - Checkout CONFIRMS the caller's EXISTING claim custody and returns its fence.
//     It does NOT create a second claim: the dispatched run already holds the
//     item, so re-acquiring it from the claimable lane (coord.AcquireSpecific)
//     would be wrong — that primitive only claims an UNCLAIMED item. Custody lost
//     to another run ⇒ ErrStaleFence, and the fence is never mutated so §6.2
//     monotonicity trivially holds.
type CoordStore struct {
	db    *sql.DB
	state *coord.HumanStateStore
}

// NewCoordStore binds the adapter to the coord DB and its state-transition op.
func NewCoordStore(db *sql.DB, state *coord.HumanStateStore) (*CoordStore, error) {
	if db == nil || state == nil {
		return nil, errors.New("taskio.NewCoordStore: db and state are required")
	}
	return &CoordStore{db: db, state: state}, nil
}

// GetTask returns the shared richer detail projected onto the wire shape.
func (c *CoordStore) GetTask(ctx context.Context, workItemID string) (TaskDetail, error) {
	td, err := coord.ReadTaskDetail(ctx, c.db, workItemID)
	if err != nil {
		return TaskDetail{}, mapCoordErr(err)
	}
	return projectDetail(td), nil
}

// PostComment appends a provenanced comment via the sanctioned append-only path.
// The author is the token's principal — never client-supplied.
func (c *CoordStore) PostComment(ctx context.Context, workItemID, principal, body string) (Comment, error) {
	tc, err := coord.AppendComment(ctx, c.db, workItemID, principal, body)
	if err != nil {
		return Comment{}, mapCoordErr(err)
	}
	return Comment{Author: tc.Author, Body: tc.Body, CreatedAt: tc.CreatedAt}, nil
}

// UpdateStatus transitions the item to target as the agent and returns the lane
// it left (for the AC8 status.from span attribute). An empty fromState guard is
// passed — the token already fences the caller to its own run; the board's
// CHECK-constraint enum and no-op guards still reject an invalid target.
func (c *CoordStore) UpdateStatus(ctx context.Context, workItemID, principal, target string) (string, error) {
	st, err := c.state.AgentTransitionState(ctx, workItemID, target, "", principal)
	if err != nil {
		return "", mapCoordErr(err)
	}
	return st.FromState, nil
}

// Checkout confirms own-run custody and returns the current fence (see type doc).
func (c *CoordStore) Checkout(ctx context.Context, workItemID, principal, runID string) (int64, error) {
	td, err := coord.ReadTaskDetail(ctx, c.db, workItemID)
	if err != nil {
		return 0, mapCoordErr(err)
	}
	// The dispatched run must still be the recorded holder. A different run_id (or
	// a different holder principal, or an unclaimed row) means custody was lost —
	// reject as a stale fence rather than silently succeeding.
	if td.RunID != runID || (td.Holder != "" && td.Holder != principal) {
		return 0, ErrStaleFence
	}
	return td.FenceToken, nil
}

// projectDetail maps the shared coord read onto the task-io wire shape. Comments
// is always non-nil so the JSON renders `[]`, never `null`.
func projectDetail(td coord.TaskDetail) TaskDetail {
	out := TaskDetail{
		WorkItemID:         td.WorkItemID,
		Title:              td.Title,
		Description:        td.Description,
		State:              td.State,
		BlockedReason:      td.BlockedReason,
		AcceptanceCriteria: td.AcceptanceCriteria,
		Goals:              td.Goals,
		FenceToken:         td.FenceToken,
		Holder:             td.Holder,
		RunID:              td.RunID,
		Comments:           make([]Comment, 0, len(td.Comments)),
	}
	for _, cm := range td.Comments {
		out.Comments = append(out.Comments, Comment{Author: cm.Author, Body: cm.Body, CreatedAt: cm.CreatedAt})
	}
	return out
}

// mapCoordErr translates coord sentinels to the handler's own sentinels so the
// HTTP layer maps status codes without importing coord.
func mapCoordErr(err error) error {
	switch {
	case errors.Is(err, coord.ErrWorkItemNotFound):
		return ErrNotFound
	case errors.Is(err, coord.ErrInvalidState), errors.Is(err, coord.ErrStateConflict):
		// Both "not a board lane" and "no-op / fromState conflict" are, to the
		// agent, "that status transition is not permitted" → 422.
		return ErrInvalidTransition
	default:
		return err
	}
}
