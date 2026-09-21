// workitemauthor.go — ADR-0024 (ISI-4735, origin ISI-4711): the AGENT-authored,
// capability-verified, custody-scoped create/edit entry into coord.work_item.
//
// It is the coord half of the narrow authoring lane the ADR opens for agents. The
// human-only HTTP board wall (internal/apiserver/workitemwrite.go) does NOT move;
// these methods are a SEPARATE trusted entry — analogous to the fleet-admin
// TeamID=="" path — reachable ONLY from the MCP tool edge (internal/memory) AFTER
// that edge has verified the caller's `work_item.author` capability. The HTTP
// handlers never call them, so there is no way to reach the agent lane without a
// capability the board wall keeps refusing.
//
// SEAM SPLIT (who checks what, and why):
//   - Capability (does this agent's role hold `work_item.author`?) is checked at
//     the MCP edge, because the role→capability binding lives outside coord (it is
//     not in the coord schema). coord cannot see roles, so it does not pretend to.
//   - Custody (does this agent actually hold the parent?) + fan-out bounds (depth,
//     per-run budget) are checked HERE, inside the write transaction, because they
//     are properties of coord's own claim/work_item rows and must be enforced under
//     the same row locks as the insert so a racing claim release / concurrent
//     create can never slip past them.
//
// INVARIANTS PRESERVED (ADR-0024 §7):
//   - Deny-by-default: an agent with no capability never reaches here; an agent
//     that reaches here but holds no claim on the parent is refused (existence-
//     hiding — it cannot tell whether the parent exists).
//   - Sub-tickets only: ParentID is REQUIRED. No agent-authored root items — the
//     console stays the only door for net-new top-level work.
//   - Custody owns lane motion: a created child lands in 'backlog' like any create;
//     these methods never write `state`.
//   - Honest agent audit provenance: the §6.5 audit row is stamped with the AGENT
//     principal and the authoring RunID, and InitiatedByUserID stays empty — an
//     agent author is never spoofed as a human principal (§3).
package coord

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// Fan-out bounds (ADR-0024 §4.3, O-2 accepted defaults). Both are enforced
// server-side inside the write txn and audited when tripped (no silent
// truncation) — an agent that hits a bound gets a clear refusal, not a partial
// write.
const (
	// AgentAuthorMaxDepth caps a created child's parent-chain depth: epic(1) →
	// story(2) → task(3) → subtask(4). A child whose parent already sits at this
	// depth is refused, so an agent cannot build unbounded nesting.
	AgentAuthorMaxDepth = 4
	// AgentAuthorRunBudget caps how many children a single authoring Run may
	// create. Over budget the Run must checkpoint and continue in a fresh run (or
	// escalate) — a runaway loop cannot mint thousands of tickets.
	AgentAuthorRunBudget = 50
)

// Agent-lane refusal sentinels. They are DISTINCT from the human-lane sentinels so
// the MCP edge (and the capability test suite) can map each to a precise tool
// error, while the existence-hiding ones deliberately do not reveal whether a
// parent/item exists outside the agent's custody.
var (
	// ErrAgentAuthorRootDenied — an agent create with no ParentID. Agents author
	// sub-tickets only; root items stay human-only (400).
	ErrAgentAuthorRootDenied = errors.New("coord: agent authoring requires a parent; agents create sub-tickets only, never root items")
	// ErrAgentAuthorNotInCustody — the agent holds no active claim on the target
	// (parent for create, item-or-ancestor for update). Returned identically
	// whether the target is missing or simply not held, so an agent cannot probe
	// for the existence of work outside its custody (existence-hiding, 404/403).
	ErrAgentAuthorNotInCustody = errors.New("coord: target is not in the agent's custody")
	// ErrAgentAuthorDepthExceeded — the created child would exceed AgentAuthorMaxDepth (400).
	ErrAgentAuthorDepthExceeded = errors.New("coord: agent authoring depth cap exceeded")
	// ErrAgentAuthorRunBudgetExceeded — this Run has already authored AgentAuthorRunBudget items (400).
	ErrAgentAuthorRunBudgetExceeded = errors.New("coord: agent authoring per-run create budget exceeded")
)

// AgentCreateWorkItemInput is one agent-authored sub-ticket create. Identity
// (Principal / AgentName / RunID) is the caller's SERVER-STAMPED session scope,
// never a tool argument — the MCP edge fills it from X-Principal-Id / X-Agent-Id /
// X-Run-Id (WINV2). ParentID is REQUIRED (sub-ticket only). The child inherits the
// parent's Team, so tenancy is automatic and un-spoofable (§6.1).
type AgentCreateWorkItemInput struct {
	ParentID string // REQUIRED — the in-custody parent this child decomposes
	Title    string // REQUIRED
	Body     string
	Priority string // optional; validated against the coord enum
	WorkMode string // optional; validated against the coord enum
	Labels   []string
	// Principal is the §6.5 audit author (the agent's server-stamped principal,
	// e.g. "agent:john"); AgentName is the identity matched against the parent's
	// coord.claim (attribution / holder); RunID is the authoring Run (per-run
	// budget + honest audit provenance). All three are required.
	Principal string
	AgentName string
	RunID     string
}

// AgentUpdateWorkItemInput is one agent-authored field edit. Custody scope is the
// in-custody item AND its descendants (ADR-0024 O-3). State is never touched.
type AgentUpdateWorkItemInput struct {
	Title             *string
	Body              *string
	ParentID          *string // reparent; "" ⇒ detach to root
	ExpectedUpdatedAt string  // optimistic-concurrency precondition, preserved
	Principal         string
	AgentName         string
	RunID             string
}

// AgentCreateWorkItem inserts one agent-authored sub-ticket under a parent the
// calling agent holds in custody, atomically with a §6.5 'work_item_created' audit
// row stamped with the AGENT principal + RunID (InitiatedByUserID empty — honest
// agent provenance, never a spoofed human). Fan-out bounds (depth, per-run budget)
// are enforced under the same txn.
//
// Semantics:
//   - (record, nil): the child exists in 'backlog', team inherited from the parent.
//   - (zero, ErrAgentAuthorRootDenied): no ParentID (400).
//   - (zero, ErrInvalidWorkItem): empty title/principal/agent/run, or a bad enum /
//     oversized label set (400).
//   - (zero, ErrAgentAuthorNotInCustody): the agent holds no claim on the parent —
//     or the parent does not exist (existence-hiding, 404/403).
//   - (zero, ErrAgentAuthorDepthExceeded / ErrAgentAuthorRunBudgetExceeded): a
//     fan-out bound tripped (400), audited.
//   - (zero, err): infrastructure failure; nothing was written.
func (s *WorkItemWriteStore) AgentCreateWorkItem(ctx context.Context, in AgentCreateWorkItemInput) (WorkItemRecord, error) {
	if in.ParentID == "" {
		return WorkItemRecord{}, ErrAgentAuthorRootDenied
	}
	if in.Title == "" || in.Principal == "" || in.AgentName == "" || in.RunID == "" {
		return WorkItemRecord{}, fmt.Errorf("%w: title, principal, agentName and runID are required", ErrInvalidWorkItem)
	}
	priority, workMode, labels, err := normalizeCreateFields(in.Priority, in.WorkMode, in.Labels)
	if err != nil {
		return WorkItemRecord{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkItemRecord{}, fmt.Errorf("coord.AgentCreateWorkItem: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// (1) Custody: lock + read the parent and require the agent to hold it. FOR
	// UPDATE serialises a racing claim release / reparent against us so the custody
	// decision and the insert act on one consistent parent row.
	var parentProject string
	var parentTeam sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT project_id::text, team_id FROM coord.work_item WHERE id = $1::uuid FOR UPDATE`,
		in.ParentID).Scan(&parentProject, &parentTeam)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return WorkItemRecord{}, ErrAgentAuthorNotInCustody // existence-hiding: no probe for unheld work
	case err != nil:
		return WorkItemRecord{}, fmt.Errorf("coord.AgentCreateWorkItem: read parent: %w", err)
	}
	held, err := agentHoldsClaim(ctx, tx, in.ParentID, in.Principal, in.AgentName)
	if err != nil {
		return WorkItemRecord{}, err
	}
	if !held {
		return WorkItemRecord{}, ErrAgentAuthorNotInCustody
	}

	// (2) Depth cap: the child's depth is parent-depth + 1.
	parentDepth, err := chainDepth(ctx, tx, in.ParentID)
	if err != nil {
		return WorkItemRecord{}, err
	}
	if parentDepth+1 > AgentAuthorMaxDepth {
		return WorkItemRecord{}, fmt.Errorf("%w: child depth %d exceeds cap %d", ErrAgentAuthorDepthExceeded, parentDepth+1, AgentAuthorMaxDepth)
	}

	// (3) Per-run create budget: count this Run's prior authored creates. The
	// count is over the immutable audit log (run_id + event_type), so it is exact
	// and cannot be under-counted by a concurrent create in the same run — that
	// create's audit row commits with its own txn before this one reads.
	var authored int
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*) FROM coord.audit_log
		 WHERE run_id = $1::uuid AND event_type = 'work_item_created'`,
		in.RunID).Scan(&authored); err != nil {
		return WorkItemRecord{}, fmt.Errorf("coord.AgentCreateWorkItem: run budget count: %w", err)
	}
	if authored >= AgentAuthorRunBudget {
		return WorkItemRecord{}, fmt.Errorf("%w: run %s has authored %d/%d items", ErrAgentAuthorRunBudgetExceeded, in.RunID, authored, AgentAuthorRunBudget)
	}

	// (4) Insert the child in the default entry lane, inheriting the parent's team
	// (§6.1). project_id is the parent's — no cross-project sub-issues.
	var rec WorkItemRecord
	var teamOut, parentOut sql.NullString
	var body, priorityOut, workModeOut sql.NullString
	err = tx.QueryRowContext(ctx, `
		INSERT INTO coord.work_item (project_id, team_id, parent_id, title, body, state, created_by, priority, work_mode, labels)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, NULLIF($5,''), 'backlog', $6, NULLIF($7,''), NULLIF($8,''), $9)
		RETURNING id::text, project_id::text, team_id::text, parent_id::text, title, body, state, created_by, created_at, updated_at, priority, work_mode, labels`,
		parentProject, parentTeam, in.ParentID, in.Title, in.Body, in.Principal, priority, workMode, labels,
	).Scan(&rec.ID, &rec.ProjectID, &teamOut, &parentOut, &rec.Title, &body, &rec.State, &rec.CreatedBy, &rec.CreatedAt, &rec.UpdatedAt, &priorityOut, &workModeOut, pq.Array(&rec.Labels))
	if err != nil {
		return WorkItemRecord{}, fmt.Errorf("coord.AgentCreateWorkItem: insert: %w", err)
	}
	rec.TeamID = nullToPtr(teamOut)
	rec.ParentID = nullToPtr(parentOut)
	rec.Body = body.String
	rec.Priority = nullToPtr(priorityOut)
	rec.WorkMode = nullToPtr(workModeOut)
	if rec.Labels == nil {
		rec.Labels = []string{}
	}

	// (5) Honest agent audit: AGENT principal + authoring RunID, InitiatedByUserID
	// empty. This is the row the per-run budget counts and the board history reads.
	if err := s.writeAgentAudit(ctx, tx, rec.ID, "work_item_created", in.Principal, in.RunID, map[string]any{
		"title":       rec.Title,
		"parent_id":   parentOut.String,
		"priority":    priority,
		"work_mode":   workMode,
		"labels":      labels,
		"agent":       in.AgentName,
		"agent_write": true,
	}); err != nil {
		return WorkItemRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkItemRecord{}, fmt.Errorf("coord.AgentCreateWorkItem: commit: %w", err)
	}
	return rec, nil
}

// AgentUpdateWorkItem edits one item's fields (title/body/parent) when the agent
// holds custody of the item OR of one of its ancestors (in-custody item +
// descendants, ADR-0024 O-3), atomically with a §6.5 'work_item_edited' audit row
// stamped with the agent principal + RunID. State is never touched.
//
// Semantics mirror UpdateWorkItem, with the human Team scope replaced by the agent
// custody scope:
//   - (record, nil): the item now carries the requested fields.
//   - (zero, ErrInvalidWorkItem): missing identity, no editable field, blank title,
//     or a self/cyclic reparent (400).
//   - (zero, ErrAgentAuthorNotInCustody): neither the item nor any ancestor is held
//     by the agent — or the item (or a reparent destination) does not exist
//     (existence-hiding).
//   - (zero, ErrAgentAuthorRootDenied): a reparent to parent_id:"" (detach-to-root) —
//     agents never promote a sub-ticket to a root item (F1, ISI-4746).
//   - (zero, ErrAgentAuthorDepthExceeded): a reparent whose destination would push
//     the moved item past the depth cap (F1, ISI-4746).
//   - (zero, ErrStateConflict): ExpectedUpdatedAt did not match (409).
//   - (zero, err): infrastructure failure; nothing was written.
func (s *WorkItemWriteStore) AgentUpdateWorkItem(ctx context.Context, workItemID string, in AgentUpdateWorkItemInput) (WorkItemRecord, error) {
	if workItemID == "" || in.Principal == "" || in.AgentName == "" || in.RunID == "" {
		return WorkItemRecord{}, fmt.Errorf("%w: workItemID, principal, agentName and runID are required", ErrInvalidWorkItem)
	}
	if in.Title == nil && in.Body == nil && in.ParentID == nil {
		return WorkItemRecord{}, fmt.Errorf("%w: no editable field supplied", ErrInvalidWorkItem)
	}
	if in.Title != nil && *in.Title == "" {
		return WorkItemRecord{}, fmt.Errorf("%w: title may not be blank", ErrInvalidWorkItem)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkItemRecord{}, fmt.Errorf("coord.AgentUpdateWorkItem: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// (1) Lock + read the target.
	var rec WorkItemRecord
	var teamOut, parentOut, bodyOut sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT id::text, project_id::text, team_id::text, parent_id::text, title, body, state, created_by, created_at, updated_at
		  FROM coord.work_item WHERE id = $1::uuid FOR UPDATE`,
		workItemID).Scan(&rec.ID, &rec.ProjectID, &teamOut, &parentOut, &rec.Title, &bodyOut, &rec.State, &rec.CreatedBy, &rec.CreatedAt, &rec.UpdatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return WorkItemRecord{}, ErrAgentAuthorNotInCustody
	case err != nil:
		return WorkItemRecord{}, fmt.Errorf("coord.AgentUpdateWorkItem: read current: %w", err)
	}

	// (2) Custody scope: the item, or any ancestor of it, must be held by the agent.
	held, err := agentHoldsCustodyScope(ctx, tx, workItemID, in.Principal, in.AgentName)
	if err != nil {
		return WorkItemRecord{}, err
	}
	if !held {
		return WorkItemRecord{}, ErrAgentAuthorNotInCustody
	}

	// (3) Optimistic-concurrency guard.
	if in.ExpectedUpdatedAt != "" {
		want, perr := time.Parse(time.RFC3339Nano, in.ExpectedUpdatedAt)
		if perr != nil {
			return WorkItemRecord{}, fmt.Errorf("%w: expectedUpdatedAt not RFC3339", ErrInvalidWorkItem)
		}
		if !rec.UpdatedAt.Equal(want) {
			return WorkItemRecord{}, fmt.Errorf("%w: item changed since %s", ErrStateConflict, in.ExpectedUpdatedAt)
		}
	}

	// (4) Reparent validation. A reparent is a fresh authoring decision about WHERE
	// work lives, so the create-side invariants re-run on the destination (F1,
	// ISI-4746): no detach-to-root (I1), the new parent must be in the agent's
	// custody scope (dest custody), and the moved item's new depth must stay within
	// the cap (I3). Source custody is already settled by step (2) above.
	if in.ParentID != nil && *in.ParentID == "" {
		// I1: parent_id:"" would NULL the parent and promote an in-custody sub-ticket
		// to an agent-controlled ROOT item — the exact rule AgentCreateWorkItem guards.
		return WorkItemRecord{}, ErrAgentAuthorRootDenied
	}
	if in.ParentID != nil && *in.ParentID != "" {
		if *in.ParentID == workItemID {
			return WorkItemRecord{}, fmt.Errorf("%w: an item cannot be its own parent", ErrInvalidWorkItem)
		}
		var newParentProject string
		err = tx.QueryRowContext(ctx, `
			SELECT project_id::text FROM coord.work_item WHERE id = $1::uuid FOR UPDATE`,
			*in.ParentID).Scan(&newParentProject)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return WorkItemRecord{}, ErrAgentAuthorNotInCustody
		case err != nil:
			return WorkItemRecord{}, fmt.Errorf("coord.AgentUpdateWorkItem: read new parent: %w", err)
		}
		if newParentProject != rec.ProjectID {
			return WorkItemRecord{}, ErrAgentAuthorNotInCustody // no cross-project reparent
		}
		newHeld, err := agentHoldsCustodyScope(ctx, tx, *in.ParentID, in.Principal, in.AgentName)
		if err != nil {
			return WorkItemRecord{}, err
		}
		if !newHeld {
			return WorkItemRecord{}, ErrAgentAuthorNotInCustody
		}
		cyclic, cerr := ancestorIncludes(ctx, tx, *in.ParentID, workItemID)
		if cerr != nil {
			return WorkItemRecord{}, cerr
		}
		if cyclic {
			return WorkItemRecord{}, fmt.Errorf("%w: reparent would form a cycle", ErrInvalidWorkItem)
		}
		// I3: the moved item's new depth (new-parent depth + 1) must stay within the
		// cap, mirroring create's per-node bound at the create/move point (F1, ISI-4746).
		newParentDepth, derr := chainDepth(ctx, tx, *in.ParentID)
		if derr != nil {
			return WorkItemRecord{}, derr
		}
		if newParentDepth+1 > AgentAuthorMaxDepth {
			return WorkItemRecord{}, fmt.Errorf("%w: reparented item depth %d exceeds cap %d", ErrAgentAuthorDepthExceeded, newParentDepth+1, AgentAuthorMaxDepth)
		}
	}

	// (5) Conditional UPDATE with a re-asserted updated_at CAS (racing write ⇒ 409).
	newTitle := rec.Title
	if in.Title != nil {
		newTitle = *in.Title
	}
	newBody := bodyOut
	if in.Body != nil {
		newBody = sql.NullString{String: *in.Body, Valid: *in.Body != ""}
	}
	newParent := parentOut
	if in.ParentID != nil {
		newParent = sql.NullString{String: *in.ParentID, Valid: *in.ParentID != ""}
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE coord.work_item
		   SET title = $2, body = $3, parent_id = $4::uuid, updated_at = now()
		 WHERE id = $1::uuid AND updated_at = $5`,
		workItemID, newTitle, newBody, newParent, rec.UpdatedAt)
	if err != nil {
		return WorkItemRecord{}, fmt.Errorf("coord.AgentUpdateWorkItem: update: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return WorkItemRecord{}, fmt.Errorf("coord.AgentUpdateWorkItem: rows: %w", err)
	} else if n == 0 {
		return WorkItemRecord{}, fmt.Errorf("%w: concurrent edit", ErrStateConflict)
	}

	// (6) Read back the committed row so the caller re-syncs to server truth.
	var priorityOut, workModeOut sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT title, body, parent_id::text, updated_at, priority, work_mode, labels
		  FROM coord.work_item WHERE id = $1::uuid`,
		workItemID).Scan(&newTitle, &newBody, &newParent, &rec.UpdatedAt, &priorityOut, &workModeOut, pq.Array(&rec.Labels)); err != nil {
		return WorkItemRecord{}, fmt.Errorf("coord.AgentUpdateWorkItem: read back: %w", err)
	}

	if err := s.writeAgentAudit(ctx, tx, workItemID, "work_item_edited", in.Principal, in.RunID, agentEditedFields(in)); err != nil {
		return WorkItemRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkItemRecord{}, fmt.Errorf("coord.AgentUpdateWorkItem: commit: %w", err)
	}

	rec.Title = newTitle
	rec.Body = newBody.String
	rec.ParentID = nullToPtr(newParent)
	rec.TeamID = nullToPtr(teamOut)
	rec.Priority = nullToPtr(priorityOut)
	rec.WorkMode = nullToPtr(workModeOut)
	if rec.Labels == nil {
		rec.Labels = []string{}
	}
	return rec, nil
}

// agentHoldsClaim reports whether the agent holds the item's coord.claim: either as
// the current lease holder (holder_principal == principal AND the lease is live) or
// as the attributed assignee (assignee_agent == agentName). Either is "in custody"
// per ADR-0024 §4.1 ("an active coord.claim on that parent, or is the parent's
// assigned agent"). assignee_agent is durable attribution (it survives lease
// release), so a plain read is sufficient for that path; the holder path additionally
// requires a live lease, read here as lease_expires_at > now(). A concurrent lease
// release racing this read is benign — the attribution decision is best-effort by
// design and the write still commits under the parent's FOR UPDATE lock.
func agentHoldsClaim(ctx context.Context, tx *sql.Tx, workItemID, principal, agentName string) (bool, error) {
	var holder, assignee sql.NullString
	var leaseLive sql.NullBool
	err := tx.QueryRowContext(ctx, `
		SELECT holder_principal, assignee_agent, lease_expires_at > now() AS lease_live
		  FROM coord.claim WHERE work_item_id = $1::uuid`,
		workItemID).Scan(&holder, &assignee, &leaseLive)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil // no claim row ⇒ not held
	case err != nil:
		return false, fmt.Errorf("coord.workItemAuthor: read claim: %w", err)
	}
	if assignee.Valid && assignee.String != "" && assignee.String == agentName {
		return true, nil
	}
	if holder.Valid && holder.String != "" && holder.String == principal && leaseLive.Valid && leaseLive.Bool {
		return true, nil
	}
	return false, nil
}

// agentHoldsCustodyScope reports whether the item is within the agent's custody
// scope: the item itself is held, or any ancestor on its parent chain is held
// (in-custody item + descendants, ADR-0024 O-3). Walks parent_id upward, bounded by
// the same belt-and-braces cap ancestorIncludes uses against a pre-existing cycle.
func agentHoldsCustodyScope(ctx context.Context, tx *sql.Tx, workItemID, principal, agentName string) (bool, error) {
	const depthCap = 256
	cur := workItemID
	for i := 0; i < depthCap; i++ {
		held, err := agentHoldsClaim(ctx, tx, cur, principal, agentName)
		if err != nil {
			return false, err
		}
		if held {
			return true, nil
		}
		var parent sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT parent_id::text FROM coord.work_item WHERE id = $1::uuid`, cur).Scan(&parent)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("coord.workItemAuthor: custody-scope walk: %w", err)
		}
		if !parent.Valid || parent.String == "" {
			return false, nil // reached a root without finding a held ancestor
		}
		cur = parent.String
	}
	return false, fmt.Errorf("coord.workItemAuthor: custody-scope walk exceeded depth %d (possible pre-existing cycle)", depthCap)
}

// chainDepth counts the depth of an item: the number of nodes from it up to and
// including its root (a root item has depth 1). Reuses the same bounded ancestry
// walk as the reparent-cycle guard (ADR-0024 §1.2: fan-out bounding is net-new, but
// it rides the traversal that already exists).
func chainDepth(ctx context.Context, tx *sql.Tx, workItemID string) (int, error) {
	const depthCap = 256
	cur := workItemID
	for depth := 1; depth <= depthCap; depth++ {
		var parent sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT parent_id::text FROM coord.work_item WHERE id = $1::uuid`, cur).Scan(&parent)
		if errors.Is(err, sql.ErrNoRows) {
			return depth, nil // vanished mid-walk; treat as a root at this depth
		}
		if err != nil {
			return 0, fmt.Errorf("coord.workItemAuthor: depth walk: %w", err)
		}
		if !parent.Valid || parent.String == "" {
			return depth, nil
		}
		cur = parent.String
	}
	return 0, fmt.Errorf("coord.workItemAuthor: depth walk exceeded %d (possible pre-existing cycle)", depthCap)
}

// writeAgentAudit inserts the §6.5 provenance row for an agent-authored write in
// the same txn: principal is the AGENT, run_id is the authoring Run, and
// initiated_by_user_id is NULL — honest agent provenance, never a spoofed human
// (ADR-0024 §3). Distinct from writeAudit only in that it stamps run_id, which the
// per-run create budget counts.
func (s *WorkItemWriteStore) writeAgentAudit(ctx context.Context, tx *sql.Tx, workItemID, eventType, principal, runID string, detail map[string]any) error {
	payload, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("coord.workItemAuthor: audit payload: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coord.audit_log (work_item_id, run_id, event_type, principal, initiated_by_user_id, payload)
		VALUES ($1::uuid, $2::uuid, $3, $4, NULL, $5::jsonb)`,
		workItemID, runID, eventType, principal, string(payload)); err != nil {
		return fmt.Errorf("coord.workItemAuthor: audit: %w", err)
	}
	return nil
}

// agentEditedFields is the audit detail for an agent edit — the changed field names
// plus the agent identity (values live in the row; history need not duplicate them).
func agentEditedFields(in AgentUpdateWorkItemInput) map[string]any {
	fields := []string{}
	if in.Title != nil {
		fields = append(fields, "title")
	}
	if in.Body != nil {
		fields = append(fields, "body")
	}
	if in.ParentID != nil {
		fields = append(fields, "parent_id")
	}
	return map[string]any{"fields": fields, "agent": in.AgentName, "agent_write": true}
}
