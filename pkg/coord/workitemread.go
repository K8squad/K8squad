// workitemread.go — M1.5 (ISI-4131): the BOARD READ MODELS. The write side of
// the loop (agent posts comments / reports changes / moves lanes through
// task-io) existed, but there was no server-side read for the board to draw
// from: the console Issues tab (M1.6, epic ISI-4126 "all visible in the console
// Issues tab, ugly is fine") needs (a) the per-Project card list — the §13
// board IS a projection of coord.work_item.state — and (b) the full ticket
// thread: agent-authored comments, the status-change history (audit_log), and
// the change refs (change_ref). This file is that read, on the custody side of
// the house so the HTTP surface (internal/apiserver) stays a thin auth+mapping
// shell — the same split as workitemwrite.go.
//
// Tenancy mirrors the write paths exactly (§12.1): teamID scopes both reads;
// an item outside the caller's Team is ErrWorkItemNotFound (404, existence-
// hiding), never a cross-tenant 403. Pass teamID "" only for a trusted,
// already-tenancy-checked caller (fleet-admin path, ISI-3937).
package coord

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// BoardItem is one card of the per-Project board list — the §13 projection of
// coord.work_item with the live claim holder (which agent, if any, has the
// card) and thread sizes for badges. Ordered by UpdatedAt DESC (the §13 List
// sort key, backed by idx_work_item_updated).
type BoardItem struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	State         string `json:"state"`
	BlockedReason string `json:"blockedReason,omitempty"`
	// Priority / Labels are the create-time attributes the card renders (ISI-4409,
	// O-3): a priority pill and label chips. Priority is "" when unset (the console
	// renders "none", never a fabricated value); Labels is never nil (empty ⇒ []).
	// WorkMode is detail-only, so it is NOT on the card (see TaskDetail).
	Priority string   `json:"priority,omitempty"`
	Labels   []string `json:"labels"`
	// ParentID is the adjacency-list up-edge (§6.1): the work item this one is
	// a direct sub-ticket of, "" for a root. Carried on the card so the
	// console can defend the sub-ticket list against mixed-version deploys
	// (an old apiserver ignoring ?parentId= becomes client-detectable,
	// ISI-4536 hardening) instead of trusting the filter silently.
	ParentID string `json:"parentId,omitempty"`
	Holder   string `json:"holder,omitempty"` // claim holder principal; "" ⇒ unclaimed
	// Assignee is the AGENT name the dispatched run works this ticket as
	// (coord.claim.assignee_agent, ISI-4237) — the board's "who is working
	// this". Unlike Holder it survives the terminal checkout release, so a
	// settled ticket still shows which agent worked it. "" ⇒ honestly
	// unassigned (the console renders "unassigned", never a fabricated name).
	Assignee     string    `json:"assignee,omitempty"`
	RunID        string    `json:"runId,omitempty"`
	UpdatedAt    time.Time `json:"updatedAt"`
	CommentCount int       `json:"commentCount"`
	ChangeCount  int       `json:"changeCount"`
}

// StatusChange is one move from the §6.5 audit history — human lane moves
// (workitemstate.go, state_transition), agent moves (AgentTransitionState,
// state_transition) AND the engine's own progress events (ISI-4237:
// claim_acquired todo → in_progress, reconcile_advanced step-to-step, the
// terminal settle's lane move, run_terminal) all land here with their
// principal, so the thread shows one narrative: claimed → advanced → settled.
type StatusChange struct {
	FromState  string    `json:"fromState"`
	ToState    string    `json:"toState"`
	Principal  string    `json:"principal"`
	OccurredAt time.Time `json:"occurredAt"`
	// EventType is the §6.5 audit event this row came from — the console can
	// label human/agent lane moves ("state_transition") distinctly from
	// engine progress ("claim_acquired" / "reconcile_advanced" /
	// "run_terminal"). Additive JSON: older readers ignore it.
	EventType string `json:"eventType,omitempty"`
}

// statusHistoryLimit bounds the audit tail the thread read returns — the
// console renders the recent timeline, not the item's whole lifetime. The full
// history stays in the audit-trail query API (ISI-2881).
const statusHistoryLimit = 50

// WorkItemThread is the full ticket detail for the board's detail view: the
// shared richer read (TaskDetail — title/description/state/AC/goals/comments/
// change refs/claim) plus the recent status-change history.
type WorkItemThread struct {
	TaskDetail
	StatusHistory []StatusChange `json:"statusHistory"`
}

// WorkItemReadStore is the board read surface: the per-Project card list and
// the per-ticket thread. Like WorkItemWriteStore it holds no mutable state
// beyond *sql.DB, so its methods are safe for concurrent use.
type WorkItemReadStore struct {
	db *sql.DB
}

// NewWorkItemReadStore binds the board reads to db.
func NewWorkItemReadStore(db *sql.DB) (*WorkItemReadStore, error) {
	if db == nil {
		return nil, errors.New("coord.NewWorkItemReadStore: nil db")
	}
	return &WorkItemReadStore{db: db}, nil
}

// ListWorkItems returns the board card list for one Project, newest-updated
// first. teamID scopes tenancy (§12.1) EXACTLY like the write paths: a scoped
// Team sees only its own cards (a team-less or foreign-team card is invisible,
// never a 403); an empty teamID is the trusted fleet-admin path (ISI-3937) and
// returns every card in the Project. parentID, when non-empty, narrows the list
// to that item's DIRECT children only (the 8.17 lazy-load the console's
// sub-ticket card asks for, ISI-4536); empty keeps the full-Project card list
// the board/kanban views draw from.
func (s *WorkItemReadStore) ListWorkItems(ctx context.Context, teamID, projectID, parentID string) ([]BoardItem, error) {
	if projectID == "" {
		return nil, fmt.Errorf("coord.ListWorkItems: projectID required")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT wi.id::text, wi.title, wi.state, wi.blocked_reason, wi.priority, wi.labels,
		       wi.parent_id::text,
		       c.holder_principal, c.assignee_agent, c.run_id::text, wi.updated_at,
		       (SELECT count(*) FROM coord.comment k WHERE k.work_item_id = wi.id),
		       (SELECT count(*) FROM coord.change_ref r WHERE r.work_item_id = wi.id)
		  FROM coord.work_item wi
		  LEFT JOIN coord.claim c ON c.work_item_id = wi.id
		 WHERE wi.project_id = $1::uuid
		   AND ($2::uuid IS NULL OR wi.team_id = $2::uuid)
		   AND ($3::uuid IS NULL OR wi.parent_id = $3::uuid)
		 ORDER BY wi.updated_at DESC, wi.id
		 LIMIT 500`, projectID, nullUUID(teamID), nullUUID(parentID))
	if err != nil {
		return nil, fmt.Errorf("coord.ListWorkItems: list %s: %w", projectID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []BoardItem
	for rows.Next() {
		var it BoardItem
		var blocked, priority, parent, holder, assignee, run sql.NullString
		// labels via pq.Array — pgx stdlib returns text[] as a *string* on Go < 1.27
		// (see coord.ReadTaskDetail); a direct &it.Labels scan 500s. ISI-4409.
		if err := rows.Scan(&it.ID, &it.Title, &it.State, &blocked, &priority, pq.Array(&it.Labels),
			&parent,
			&holder, &assignee, &run,
			&it.UpdatedAt, &it.CommentCount, &it.ChangeCount); err != nil {
			return nil, fmt.Errorf("coord.ListWorkItems: scan: %w", err)
		}
		it.BlockedReason = blocked.String
		it.Priority = priority.String
		it.ParentID = parent.String
		if it.Labels == nil {
			it.Labels = []string{}
		}
		it.Holder = holder.String
		it.Assignee = assignee.String
		it.RunID = run.String
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("coord.ListWorkItems: iterate: %w", err)
	}
	return out, nil
}

// ReadWorkItemThread returns one ticket's full thread: the shared richer read
// plus the recent status-change history. teamID scopes tenancy exactly like
// UpdateWorkItem — a cross-Team probe reads as ErrWorkItemNotFound (404).
func (s *WorkItemReadStore) ReadWorkItemThread(ctx context.Context, workItemID, teamID string) (WorkItemThread, error) {
	if workItemID == "" {
		return WorkItemThread{}, fmt.Errorf("coord.ReadWorkItemThread: workItemID required")
	}

	// Tenancy pre-check on the item's own row (ReadTaskDetail has no team
	// parameter by design — the task-io caller is fenced by its run token).
	var itemTeam sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT team_id FROM coord.work_item WHERE id = $1::uuid`, workItemID).Scan(&itemTeam)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return WorkItemThread{}, ErrWorkItemNotFound
	case err != nil:
		return WorkItemThread{}, fmt.Errorf("coord.ReadWorkItemThread: read team: %w", err)
	}
	if teamID != "" && (!itemTeam.Valid || itemTeam.String != teamID) {
		return WorkItemThread{}, ErrWorkItemNotFound
	}

	td, err := ReadTaskDetail(ctx, s.db, workItemID)
	if err != nil {
		return WorkItemThread{}, err
	}

	history, err := readStatusHistory(ctx, s.db, workItemID)
	if err != nil {
		return WorkItemThread{}, err
	}
	return WorkItemThread{TaskDetail: td, StatusHistory: history}, nil
}

// readStatusHistory tails the §6.5 audit rows for one item, newest first —
// the "status changes" the M1.5 AC wants visible on the ticket. ISI-4237:
// the filter is the ENGINE-PROGRESS set, not just human/agent lane moves —
// a dispatched ticket's timeline reads claimed (todo → in_progress) →
// advanced (claiming_sandbox → dispatching → …) → settled (the terminal lane
// move / run_terminal), because the defect being fixed was a full dispatch
// loop that left the board showing nothing but a silent in_progress.
func readStatusHistory(ctx context.Context, db *sql.DB, workItemID string) ([]StatusChange, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT from_state, to_state, principal, created_at, event_type
		  FROM coord.audit_log
		 WHERE work_item_id = $1::uuid
		   AND event_type IN ('state_transition', 'claim_acquired',
		                      'reconcile_advanced', 'run_terminal')
		 ORDER BY id DESC
		 LIMIT $2`, workItemID, statusHistoryLimit)
	if err != nil {
		return nil, fmt.Errorf("coord.ReadWorkItemThread: read status history: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []StatusChange{}
	for rows.Next() {
		var sc StatusChange
		var from sql.NullString
		if err := rows.Scan(&from, &sc.ToState, &sc.Principal, &sc.OccurredAt, &sc.EventType); err != nil {
			return nil, fmt.Errorf("coord.ReadWorkItemThread: scan status history: %w", err)
		}
		sc.FromState = from.String
		out = append(out, sc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("coord.ReadWorkItemThread: iterate status history: %w", err)
	}
	return out, nil
}

// FindWorkItemByLabel returns the work item in projectID that carries label,
// or ErrWorkItemNotFound when none does. It is the shared create-if-absent
// precondition for label-keyed idempotent writes (ISI-4757 GitHub-Kanban bridge
// join; ISI-4766 system review-automation dedup): a caller looks a
// candidate label up first and skips its create when a match already exists, so
// a redelivered webhook or a re-run reconcile is a no-op. teamID scopes tenancy
// EXACTLY like ListWorkItems — a scoped Team sees only its own items (a foreign
// or team-less item reads as ErrWorkItemNotFound, never a cross-tenant 403);
// an empty teamID is the trusted fleet path used by the operator-side SYSTEM
// callers. When more than one item carries the label (labels are not unique at
// the schema level) the oldest is returned deterministically. Read-only: it
// mutates nothing and creates no item, so it never crosses the ISI-4711 custody
// wall.
func (s *WorkItemReadStore) FindWorkItemByLabel(ctx context.Context, teamID, projectID, label string) (WorkItemRecord, error) {
	if projectID == "" {
		return WorkItemRecord{}, fmt.Errorf("coord.FindWorkItemByLabel: projectID required")
	}
	if label == "" {
		return WorkItemRecord{}, fmt.Errorf("coord.FindWorkItemByLabel: label required")
	}
	var rec WorkItemRecord
	var team sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT wi.id::text, wi.project_id::text, wi.team_id::text, wi.title, wi.state, wi.labels
		  FROM coord.work_item wi
		 WHERE wi.project_id = $1::uuid
		   AND ($2::uuid IS NULL OR wi.team_id = $2::uuid)
		   AND $3 = ANY(wi.labels)
		 ORDER BY wi.created_at, wi.id
		 LIMIT 1`, projectID, nullUUID(teamID), label).
		Scan(&rec.ID, &rec.ProjectID, &team, &rec.Title, &rec.State, pq.Array(&rec.Labels))
	if errors.Is(err, sql.ErrNoRows) {
		return WorkItemRecord{}, ErrWorkItemNotFound
	}
	if err != nil {
		return WorkItemRecord{}, fmt.Errorf("coord.FindWorkItemByLabel: lookup %s in %s: %w", label, projectID, err)
	}
	if team.Valid {
		rec.TeamID = &team.String
	}
	if rec.Labels == nil {
		rec.Labels = []string{}
	}
	return rec, nil
}

// nullUUID turns an empty teamID into a NULL bind parameter (the trusted
// fleet-admin path); a non-empty one is cast to uuid at the server.
func nullUUID(id string) any {
	if id == "" {
		return nil
	}
	return id
}
