// agentauthor.go — the capability-gated, custody-scoped AGENT authoring lane on
// coord.work_item (ADR-0024, ISI-4734 / origin ISI-4711). Where workitemwrite.go
// is the HUMAN board create/edit and workitemdispatch.go the HUMAN assign, this
// file is the coord half of the narrow relaxation that lets a *capability-holding*
// agent (a PM decomposing an epic it holds in custody) author sub-tickets and hand
// them to implementers — while the human-only HTTP board wall stays exactly as it
// is (internal/apiserver/workitemwrite.go:83-85,158-159 are untouched).
//
// The capability gate itself (role→`work_item.author`, O-1) lives at the MCP edge
// (internal/memory), server-authenticated from the session, never a request field.
// By the time any method here runs, the edge has already verified the capability;
// coord enforces the four custody invariants the capability does NOT cover:
//
//	I1  SUB-TICKETS ONLY. Every create REQUIRES a parent (ADR-0024 §4.1). Root
//	    items stay human-only — the console is the only door for net-new top-level
//	    work. ErrAgentRootForbidden.
//	I2  PARENT IN CUSTODY. The agent must hold the parent (its run holds the claim,
//	    or it is the requested_agent), so it can only decompose work it was handed,
//	    never reach into an epic it does not own. ErrAgentNotInCustody. Cross-tenant
//	    parents are 404 existence-hiding (§12.1), never a cross-tenant 403.
//	I3  BOUNDED DEPTH. A created child's parent-chain depth is capped at N=4
//	    (epic→story→task→subtask, O-2) so an agent can't build unbounded nesting.
//	    ErrAgentDepthCapExceeded.
//	I4  BOUNDED FAN-OUT. A single agent Run may author at most M=50 children (O-2),
//	    counted from the run-stamped audit rows, so a runaway loop can't mint
//	    thousands of tickets. ErrAgentRunBudgetExceeded.
//
// HONEST AUDIT (ADR-0024 §3). Every agent-authored write books a §6.5 audit row
// stamped with the agent's principal AND its run_id (the agent-run linkage), and
// initiated_by_user_id NULL — never spoofed as a human. Human creates leave run_id
// NULL, so the per-run budget count (I4) sees only this run's agent authorship.
//
// Tenancy, enum validation, labels, optimistic-concurrency and the reparent-cycle
// guard are all INHERITED — create reuses the same insert shape as CreateWorkItem,
// update delegates to WorkItemWriteStore.UpdateWorkItem after the custody pre-check,
// and assign delegates to WorkItemDispatchStore.RequestDispatch (with Initiator
// "agent"). This file adds only the four gates above, never a second copy of the
// write semantics.
package coord

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/lib/pq"
)

// AgentAuthorDepthCap (I3) and AgentAuthorRunBudget (I4) are the ADR-0024 §4.3
// fan-out bounds, confirmed by Henrik at the recommended defaults (O-2). A created
// child's depth (root == 1) must be ≤ the cap; a single run may author ≤ the budget.
const (
	AgentAuthorDepthCap  = 4
	AgentAuthorRunBudget = 50
)

// Sentinels for the four agent-authoring gates. Callers match with errors.Is; the
// MCP edge maps each to an isError tool result (the deny message the model sees).
var (
	// ErrAgentRootForbidden (I1) — an agent create with no parent. Root items are
	// human-only; the relaxation is sub-tickets only.
	ErrAgentRootForbidden = errors.New("coord: agent authoring requires a parent (root items are human-only)")
	// ErrAgentNotInCustody (I2) — the agent holds no claim on the parent (create)
	// or on the target-or-any-ancestor (update), and is not its requested_agent.
	ErrAgentNotInCustody = errors.New("coord: agent does not hold the target in custody")
	// ErrAgentDepthCapExceeded (I3) — the new child would exceed the parent-chain
	// depth cap.
	ErrAgentDepthCapExceeded = errors.New("coord: agent authoring depth cap exceeded")
	// ErrAgentRunBudgetExceeded (I4) — this run has already authored the maximum
	// number of children; checkpoint and continue in a fresh run.
	ErrAgentRunBudgetExceeded = errors.New("coord: agent per-run create budget exceeded")
	// ErrAgentAssignUnavailable — the store was built without a dispatch backend
	// (no TeamAgentResolver in this deployment), so the PM→implementer assign verb
	// cannot run here. Create/update stay available; assign is refused honestly
	// rather than silently dropped. See ISI-4734 follow-up: wiring the Team-agent
	// resolver into the memory service.
	ErrAgentAssignUnavailable = errors.New("coord: agent assign is not available in this deployment (no dispatch backend)")
)

// AgentAuthorStore is the coord binding of the ADR-0024 authoring lane. It holds
// the *sql.DB (for the create txn and the custody reads) plus the two existing
// human stores it delegates to for update/assign, so the write semantics live in
// exactly one place. No mutable state beyond these handles — safe for concurrent
// use (each create opens its own transaction).
type AgentAuthorStore struct {
	db       *sql.DB
	writes   *WorkItemWriteStore
	dispatch *WorkItemDispatchStore
}

// NewAgentAuthorStore binds the authoring lane to db and the human write store it
// reuses. dispatch backs the PM→implementer assign verb and MAY be nil: a
// deployment without a TeamAgentResolver (the memory service today, which has no
// Team-CR reader) still serves create + update, and AgentAssign returns
// ErrAgentAssignUnavailable rather than silently dropping the verb. db and writes
// are required — a nil either way is a half-wired lane, so a construction error.
func NewAgentAuthorStore(db *sql.DB, writes *WorkItemWriteStore, dispatch *WorkItemDispatchStore) (*AgentAuthorStore, error) {
	if db == nil {
		return nil, errors.New("coord.NewAgentAuthorStore: nil db")
	}
	if writes == nil {
		return nil, errors.New("coord.NewAgentAuthorStore: nil write store")
	}
	return &AgentAuthorStore{db: db, writes: writes, dispatch: dispatch}, nil
}

// AgentIdentity is the server-authenticated agent scope every authoring op runs
// under (WINV2). All four fields ride the MCP session headers (X-Principal-Id /
// X-Agent-Id / X-Run-Id / X-Team-Id), never a tool argument — an agent cannot
// widen tenancy or forge authorship through the body.
type AgentIdentity struct {
	Principal string // X-Principal-Id — the §6.5 author, stamped on the audit row
	AgentID   string // X-Agent-Id — the agent name (Team.Spec.Agents[].Name); custody & assign identity
	RunID     string // X-Run-Id — the run whose custody authorizes the write; stamped for the budget count
	TeamID    string // X-Team-Id — the caller's Team scope; a target outside it is 404 (existence-hiding)
}

// AgentCreateChildInput is one agent-authored sub-ticket. ParentID is REQUIRED
// (I1). The remaining fields mirror CreateWorkItemInput's create-time attributes;
// state is absent (the child lands in backlog, §8.6). AssigneeAgentID is handled
// by the MCP edge as a follow-on assign, not here.
type AgentCreateChildInput struct {
	ParentID string
	Title    string
	Body     string
	Priority string
	WorkMode string
	Labels   []string
}

// AgentCreateChild creates one sub-ticket under a parent the agent holds in
// custody, atomically with a run-stamped §6.5 audit row, after the I1..I4 gates.
//
// Semantics:
//   - (record, nil): the child exists in backlog under the parent, team inherited.
//   - (zero, ErrAgentRootForbidden): no parent supplied (I1).
//   - (zero, ErrInvalidWorkItem): empty title, or a bad enum / oversized label set.
//   - (zero, ErrWorkItemNotFound): parent absent or outside the caller's Team (404).
//   - (zero, ErrAgentNotInCustody): the agent holds no claim on the parent (I2).
//   - (zero, ErrAgentDepthCapExceeded): the child would exceed the depth cap (I3).
//   - (zero, ErrAgentRunBudgetExceeded): this run is over its create budget (I4).
//   - (zero, err): infrastructure failure; nothing was written.
func (s *AgentAuthorStore) AgentCreateChild(ctx context.Context, id AgentIdentity, in AgentCreateChildInput) (WorkItemRecord, error) {
	if id.Principal == "" || id.AgentID == "" || id.RunID == "" {
		return WorkItemRecord{}, fmt.Errorf("%w: principal, agentId and runId are required for agent authoring", ErrInvalidWorkItem)
	}
	if in.ParentID == "" {
		return WorkItemRecord{}, ErrAgentRootForbidden // I1: sub-tickets only.
	}
	if in.Title == "" {
		return WorkItemRecord{}, fmt.Errorf("%w: title required", ErrInvalidWorkItem)
	}
	priority, workMode, labels, err := normalizeCreateFields(in.Priority, in.WorkMode, in.Labels)
	if err != nil {
		return WorkItemRecord{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkItemRecord{}, fmt.Errorf("coord.AgentCreateChild: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// (1) Lock + read the parent: its project/team (tenancy + inheritance) and its
	// requested_agent (one of the custody signals). FOR UPDATE serialises a racing
	// claim/reassign against the custody check below.
	var parentProject string
	var parentTeam, parentRequestedAgent sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT project_id::text, team_id::text, requested_agent
		  FROM coord.work_item WHERE id = $1::uuid FOR UPDATE`,
		in.ParentID).Scan(&parentProject, &parentTeam, &parentRequestedAgent)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return WorkItemRecord{}, ErrWorkItemNotFound
	case err != nil:
		return WorkItemRecord{}, fmt.Errorf("coord.AgentCreateChild: read parent: %w", err)
	}

	// (2) Tenancy (§12.1): a parent outside the caller's Team is invisible — 404,
	// never a cross-tenant 403.
	if id.TeamID != "" && (!parentTeam.Valid || parentTeam.String != id.TeamID) {
		return WorkItemRecord{}, ErrWorkItemNotFound
	}

	// (3) I2 custody: the agent must hold the parent. Its run holds the claim, or it
	// is the parent's requested_agent (the pre-run intent Intake honors).
	held, err := s.holdsCustody(ctx, tx, in.ParentID, id, parentRequestedAgent)
	if err != nil {
		return WorkItemRecord{}, err
	}
	if !held {
		return WorkItemRecord{}, ErrAgentNotInCustody
	}

	// (4) I3 depth: child depth = parent depth + 1, capped at N. Reuses the same
	// bounded parent-chain walk as the reparent-cycle guard.
	parentDepth, err := ancestorDepth(ctx, tx, in.ParentID)
	if err != nil {
		return WorkItemRecord{}, err
	}
	if parentDepth+1 > AgentAuthorDepthCap {
		return WorkItemRecord{}, fmt.Errorf("%w: child depth %d exceeds cap %d", ErrAgentDepthCapExceeded, parentDepth+1, AgentAuthorDepthCap)
	}

	// (5) I4 budget: count THIS run's prior agent-authored creates (run-stamped audit
	// rows; human creates leave run_id NULL and never count).
	var authored int
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*) FROM coord.audit_log
		 WHERE run_id = $1::uuid AND event_type = 'work_item_created'`,
		id.RunID).Scan(&authored); err != nil {
		return WorkItemRecord{}, fmt.Errorf("coord.AgentCreateChild: budget count: %w", err)
	}
	if authored >= AgentAuthorRunBudget {
		return WorkItemRecord{}, fmt.Errorf("%w: run has authored %d of %d children", ErrAgentRunBudgetExceeded, authored, AgentAuthorRunBudget)
	}

	// (6) Insert the child, inheriting the parent's team (§6.1), in backlog. Same
	// insert shape/columns as CreateWorkItem so the child is indistinguishable from
	// a human-authored one on the read side — only the audit row carries the agent.
	var rec WorkItemRecord
	var teamOut, parentOut, body, priorityOut, workModeOut sql.NullString
	err = tx.QueryRowContext(ctx, `
		INSERT INTO coord.work_item (project_id, team_id, parent_id, title, body, state, created_by, priority, work_mode, labels)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, NULLIF($5,''), 'backlog', $6, NULLIF($7,''), NULLIF($8,''), $9)
		RETURNING id::text, project_id::text, team_id::text, parent_id::text, title, body, state, created_by, created_at, updated_at, priority, work_mode, labels`,
		parentProject, parentTeam, in.ParentID, in.Title, in.Body, id.Principal, priority, workMode, pq.Array(labels),
	).Scan(&rec.ID, &rec.ProjectID, &teamOut, &parentOut, &rec.Title, &body, &rec.State, &rec.CreatedBy, &rec.CreatedAt, &rec.UpdatedAt, &priorityOut, &workModeOut, pq.Array(&rec.Labels))
	if err != nil {
		return WorkItemRecord{}, fmt.Errorf("coord.AgentCreateChild: insert: %w", err)
	}
	rec.TeamID = nullToPtr(teamOut)
	rec.ParentID = nullToPtr(parentOut)
	rec.Body = body.String
	rec.Priority = nullToPtr(priorityOut)
	rec.WorkMode = nullToPtr(workModeOut)
	if rec.Labels == nil {
		rec.Labels = []string{}
	}

	// (7) Honest agent provenance (ADR-0024 §3): run_id set (agent-run linkage),
	// principal = the agent, initiated_by NULL. payload records author="agent".
	if err := s.writeAgentAudit(ctx, tx, rec.ID, "work_item_created", id, map[string]any{
		"author":    "agent",
		"agent_id":  id.AgentID,
		"title":     rec.Title,
		"parent_id": in.ParentID,
		"priority":  priority,
		"work_mode": workMode,
		"labels":    labels,
	}); err != nil {
		return WorkItemRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkItemRecord{}, fmt.Errorf("coord.AgentCreateChild: commit: %w", err)
	}
	return rec, nil
}

// AgentUpdate edits fields of an item the agent holds in custody OR any descendant
// of such an item (O-3: in-custody item + descendants). The custody frontier is
// checked here; the field write, enum/label validation, optimistic-concurrency and
// reparent-cycle guard are then INHERITED from WorkItemWriteStore.UpdateWorkItem,
// scoped to the agent's Team (a cross-tenant target is 404). Principal is the agent
// (honest §6.5 author). No state — lane motion stays custody's job (§4.2).
func (s *AgentAuthorStore) AgentUpdate(ctx context.Context, id AgentIdentity, workItemID string, in UpdateWorkItemInput) (WorkItemRecord, error) {
	if id.Principal == "" || id.AgentID == "" || id.RunID == "" {
		return WorkItemRecord{}, fmt.Errorf("%w: principal, agentId and runId are required for agent authoring", ErrInvalidWorkItem)
	}
	if workItemID == "" {
		return WorkItemRecord{}, fmt.Errorf("%w: work item id required", ErrInvalidWorkItem)
	}

	// Custody frontier: walk target→root; the agent may edit the item iff it holds
	// custody of the item or any ancestor (an epic's holder governs its whole
	// subtree). A cross-tenant / absent item is 404 existence-hiding.
	authorized, err := s.holdsCustodyFrontier(ctx, s.db, workItemID, id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return WorkItemRecord{}, ErrWorkItemNotFound
	case err != nil:
		return WorkItemRecord{}, err
	}
	if !authorized {
		return WorkItemRecord{}, ErrAgentNotInCustody
	}

	// Delegate the write to the single-sourced human path, pinned to the agent's
	// Team and stamped with the agent principal. TeamID is set (never "") so the
	// store still fences the item to the agent's tenancy inside its own txn.
	in.TeamID = id.TeamID
	in.Principal = id.Principal
	in.InitiatedByUserID = ""
	return s.writes.UpdateWorkItem(ctx, workItemID, in)
}

// AgentAssign hands an in-custody item (or a descendant) to an implementer agent by
// driving the existing board dispatch (ADR-0022 RequestDispatch) with Initiator
// "agent" — the PM→implementer handoff. The target agent must belong to the item's
// Team (the existing TeamAgentResolver guard); the item must be an unclaimed
// backlog/todo (RequestDispatch's own precondition). Custody is enforced here first
// so an agent cannot dispatch work it does not hold.
func (s *AgentAuthorStore) AgentAssign(ctx context.Context, id AgentIdentity, workItemID, assigneeAgentID string) (WorkItemDispatchResult, error) {
	if id.Principal == "" || id.AgentID == "" || id.RunID == "" {
		return WorkItemDispatchResult{}, fmt.Errorf("%w: principal, agentId and runId are required for agent authoring", ErrInvalidWorkItem)
	}
	if workItemID == "" || assigneeAgentID == "" {
		return WorkItemDispatchResult{}, fmt.Errorf("%w: workItemId and assigneeAgentId are required", ErrInvalidWorkItem)
	}
	if s.dispatch == nil {
		return WorkItemDispatchResult{}, ErrAgentAssignUnavailable
	}
	authorized, err := s.holdsCustodyFrontier(ctx, s.db, workItemID, id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return WorkItemDispatchResult{}, ErrWorkItemNotFound
	case err != nil:
		return WorkItemDispatchResult{}, err
	}
	if !authorized {
		return WorkItemDispatchResult{}, ErrAgentNotInCustody
	}
	return s.dispatch.RequestDispatch(ctx, RequestDispatchInput{
		WorkItemID: workItemID,
		AgentID:    assigneeAgentID,
		TeamID:     id.TeamID,
		Principal:  id.Principal,
		Initiator:  "agent",
	})
}

// rowQuerier is the read seam shared by *sql.DB and *sql.Tx so the custody/depth
// helpers run either standalone (update/assign pre-check) or inside the create txn.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// holdsCustody reports whether the agent holds an active claim on workItemID, or is
// its requested_agent. The claim carries the run holding the item (§6.2) and the
// holder principal; requestedAgent is passed in when the caller already read it
// (create), else "" and re-read here (frontier walk).
func (s *AgentAuthorStore) holdsCustody(ctx context.Context, q rowQuerier, workItemID string, id AgentIdentity, requestedAgent sql.NullString) (bool, error) {
	if requestedAgent.Valid && requestedAgent.String == id.AgentID {
		return true, nil
	}
	var holder, runID sql.NullString
	err := q.QueryRowContext(ctx, `
		SELECT holder_principal, run_id::text FROM coord.claim WHERE work_item_id = $1::uuid`,
		workItemID).Scan(&holder, &runID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil // no claim row ⇒ unheld
	}
	if err != nil {
		return false, fmt.Errorf("coord.AgentAuthor: read claim: %w", err)
	}
	if runID.Valid && runID.String == id.RunID {
		return true, nil
	}
	if holder.Valid && holder.String == id.Principal {
		return true, nil
	}
	return false, nil
}

// holdsCustodyFrontier walks workItemID→root (bounded) and reports whether the agent
// holds custody of the item or any ancestor (O-3: in-custody item + descendants).
// The first read doubles as the existence check: an absent/cross-tenant target
// surfaces sql.ErrNoRows to the caller (mapped to 404 existence-hiding).
func (s *AgentAuthorStore) holdsCustodyFrontier(ctx context.Context, q rowQuerier, workItemID string, id AgentIdentity) (bool, error) {
	const depthCap = 256 // belt-and-braces against a pre-existing data cycle (matches ancestorIncludes).
	cur := workItemID
	for i := 0; i < depthCap; i++ {
		var team, parent, requestedAgent sql.NullString
		err := q.QueryRowContext(ctx, `
			SELECT team_id::text, parent_id::text, requested_agent
			  FROM coord.work_item WHERE id = $1::uuid`, cur).Scan(&team, &parent, &requestedAgent)
		if err != nil {
			if i == 0 {
				return false, err // sql.ErrNoRows on the target ⇒ existence-hiding 404 for the caller.
			}
			if errors.Is(err, sql.ErrNoRows) {
				return false, nil // a dangling parent ref ends the walk unauthorized.
			}
			return false, fmt.Errorf("coord.AgentAuthor: custody walk: %w", err)
		}
		// Tenancy (§12.1) is checked on the target row only: a cross-tenant item is
		// invisible (404) rather than a walk that leaks ancestry across Teams.
		if i == 0 && id.TeamID != "" && (!team.Valid || team.String != id.TeamID) {
			return false, sql.ErrNoRows
		}
		held, err := s.holdsCustody(ctx, q, cur, id, requestedAgent)
		if err != nil {
			return false, err
		}
		if held {
			return true, nil
		}
		if !parent.Valid || parent.String == "" {
			return false, nil // reached a root without finding custody.
		}
		cur = parent.String
	}
	return false, fmt.Errorf("coord.AgentAuthor: custody walk exceeded depth %d (possible pre-existing cycle)", depthCap)
}

// ancestorDepth counts nodes from start up to its root, inclusive (a root == 1) —
// the parent depth I3 caps. Bounded exactly like ancestorIncludes so a pre-existing
// data cycle errors loudly rather than looping.
func ancestorDepth(ctx context.Context, q rowQuerier, start string) (int, error) {
	const depthCap = 256
	cur := start
	for depth := 1; depth <= depthCap; depth++ {
		var parent sql.NullString
		err := q.QueryRowContext(ctx, `SELECT parent_id::text FROM coord.work_item WHERE id = $1::uuid`, cur).Scan(&parent)
		if errors.Is(err, sql.ErrNoRows) {
			return depth, nil // start's chain ends here (should not happen mid-txn; treat as reached-root).
		}
		if err != nil {
			return 0, fmt.Errorf("coord.AgentAuthor: depth walk: %w", err)
		}
		if !parent.Valid || parent.String == "" {
			return depth, nil // reached a root.
		}
		cur = parent.String
	}
	return 0, fmt.Errorf("coord.AgentAuthor: depth walk exceeded %d (possible pre-existing cycle)", depthCap)
}

// writeAgentAudit inserts the §6.5 provenance row for an agent-authored write in the
// same txn: run_id set (agent-run linkage the per-run budget counts), principal the
// agent, initiated_by NULL, fence NULL (authoring is not a custody op, ADR-037).
func (s *AgentAuthorStore) writeAgentAudit(ctx context.Context, tx *sql.Tx, workItemID, eventType string, id AgentIdentity, detail map[string]any) error {
	payload, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("coord.AgentAuthor: audit payload: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coord.audit_log (work_item_id, run_id, event_type, principal, payload)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5::jsonb)`,
		workItemID, id.RunID, eventType, id.Principal, string(payload)); err != nil {
		return fmt.Errorf("coord.AgentAuthor: audit: %w", err)
	}
	return nil
}
