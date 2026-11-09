package coord

// workitemagentcomment.go — ISI-7220 (epic ISI-7075, workstream B3): the AGENT
// work-item comment write op with cross-agent reference parity.
//
// Parity map (what "cross-agent reference parity, board + discussion" means in
// code — the plan's grounded map confirmed, this file closes the gap):
//
//   - DISCUSSION surface (internal/discussion + the discussion_post MCP tool):
//     @-mention dispatch (ISI-5125) AND structured ticket references
//     (ISI-5166) — both primitives live, parity-tested per edge.
//   - BOARD surface, human console comments (AppendHumanComment): ticket
//     references ✓ (ISI-5214). No agent mentions — a human references an agent
//     by assigning (§8.6), the board's explicit dispatch verb.
//   - BOARD surface, agent comments: NOTHING existed. The run-token taskio
//     path (coord.AppendComment) is a plain INSERT for the run's own final
//     comment — no references, no mentions, and no MCP tool exposes it. This
//     file adds the agent-authored comment with BOTH reference primitives:
//     structured ticket links (the SAME resolveCommentRefs pipeline the human
//     path rides) and structured agent mentions (roster-validated by the MCP
//     edge, persisted under payload.mentions).
//
// FR-B3 is PRESERVED, not bent: a board comment is still an append-only,
// provenanced note (§6.1) — a mention is a durable REFERENCE (metadata the
// thread renders), never a dispatch and never an agent-to-agent channel. The
// board's sanctioned "make agent X act" verb stays the custody op
// (work_item_assign / AgentRequestDispatch, ADR-0024 §5). Rooms dispatch on
// @-mention because rooms ARE the chat surface; the board never was — parity
// of the reference PRIMITIVE, not of the transport semantic.
//
// Custody discipline mirrors AgentUpdateWorkItem (ADR-0024 §4): an agent may
// comment on a work item it holds in custody or any descendant of one it
// holds (agentHoldsCustodyScope) — referencing OTHER tickets flows through the
// references primitive (comment on YOUR item, link any in-project ticket),
// exactly the discussion model (post in YOUR thread, link other tickets).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// AgentCommentInput is one agent-authored work-item comment (ISI-7220 B3).
// Identity fields are server-supplied by the MCP edge from the authenticated
// session (never tool arguments); References/Mentions are the cross-agent
// reference primitives the comment carries.
type AgentCommentInput struct {
	WorkItemID string
	Principal  string
	AgentName  string
	RunID      string
	TeamID     string
	Body       string
	// References are structured ticket LINKS (the ISI-5214 wire shape). Resolved
	// against the commented ticket's own project in the same txn; unknown /
	// out-of-project / cross-Team candidates are dropped, never persisted.
	References []CommentRef
	// Mentions are agent names recorded as durable references on the comment
	// (payload.mentions). The MCP edge validates them against the Team roster
	// before the call; a mention NEVER dispatches (FR-B3).
	Mentions []string
}

// agentMentionCap bounds the mention set on one comment — the same bounding
// discipline as every other fan-out-shaped input (a comment is a note, not a
// broadcast surface).
const agentMentionCap = 10

// AgentAppendComment appends one agent-authored comment — with structured
// ticket references and agent mentions — to a work item the agent holds in
// custody (or a descendant of one it holds), and records its §6.5 audit row in
// the same transaction.
//
// Semantics (mirroring the sibling agent write ops so the whole agent surface
// answers one shape):
//   - (comment, nil): persisted; Author is the server-stamped principal;
//     References/Mentions carry only the survivors (resolved refs, normalized
//     mentions).
//   - (zero, ErrInvalidWorkItem): missing identity/body (400-class).
//   - (zero, ErrWorkItemNotFound): no such item, or one outside the caller's
//     Team scope — existence-hiding (404), never a cross-tenant 403.
//   - (zero, ErrAgentAuthorNotInCustody): the agent holds neither the item nor
//     any ancestor (ADR-0024 custody scope).
//   - (zero, err): infrastructure failure; nothing was written.
//
// NO lane nudge: comment-triggered re-dispatch (ISI-4495) is the HUMAN console
// semantic; an agent comment is a reference/handoff note and must never
// re-trigger work as a side effect.
func (s *WorkItemWriteStore) AgentAppendComment(ctx context.Context, in AgentCommentInput) (TaskComment, error) {
	if in.WorkItemID == "" || in.Principal == "" || in.AgentName == "" || in.RunID == "" {
		return TaskComment{}, fmt.Errorf("%w: workItemID, principal, agentName and runID are required", ErrInvalidWorkItem)
	}
	if in.Body == "" {
		return TaskComment{}, fmt.Errorf("%w: comment body may not be empty", ErrInvalidWorkItem)
	}
	mentions := normalizeAgentMentions(in.Mentions)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TaskComment{}, fmt.Errorf("coord.AgentAppendComment: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Tenancy fence (§12.1) + the commented ticket's own project for ref
	// resolution — the same lock+read shape AppendHumanComment rides. FOR
	// UPDATE OF wi serialises against a racing delete/reparent so the FK the
	// INSERT depends on cannot vanish mid-txn.
	var teamOut sql.NullString
	var projectID string
	err = tx.QueryRowContext(ctx, `
		SELECT wi.team_id::text, wi.project_id::text
		  FROM coord.work_item wi
		 WHERE wi.id = $1::uuid
		   FOR UPDATE OF wi`,
		in.WorkItemID).Scan(&teamOut, &projectID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return TaskComment{}, ErrWorkItemNotFound
	case err != nil:
		return TaskComment{}, fmt.Errorf("coord.AgentAppendComment: read item: %w", err)
	}
	if in.TeamID != "" && (!teamOut.Valid || teamOut.String != in.TeamID) {
		return TaskComment{}, ErrWorkItemNotFound
	}

	// Custody scope (ADR-0024 §4, mirrors AgentUpdateWorkItem): the item, or
	// any ancestor of it, must be held by this agent.
	held, err := agentHoldsCustodyScope(ctx, tx, in.WorkItemID, in.Principal, in.AgentName)
	if err != nil {
		return TaskComment{}, err
	}
	if !held {
		return TaskComment{}, ErrAgentAuthorNotInCustody
	}

	// Ticket links: the SAME resolve → survive pipeline the human comment path
	// rides (ISI-5214) — dedupe, drop unknown/out-of-project/cross-Team,
	// canonicalize titles from coord.work_item. A link is never a dispatch and
	// never a write fence — an empty/unresolvable set degrades to link-free.
	resolvedRefs, err := resolveCommentRefs(ctx, tx, projectID, in.TeamID, in.References)
	if err != nil {
		return TaskComment{}, fmt.Errorf("coord.AgentAppendComment: resolve refs: %w", err)
	}

	// payload stays NULL for a plain comment; otherwise one jsonb object with
	// only the non-empty primitives — the same $N::jsonb discipline as the
	// human path (lib/pq []byte→bytea ambiguity avoidance).
	var payloadArg any
	if len(resolvedRefs) > 0 || len(mentions) > 0 {
		obj := map[string]any{}
		if len(resolvedRefs) > 0 {
			obj["references"] = resolvedRefs
		}
		if len(mentions) > 0 {
			obj["mentions"] = mentions
		}
		encoded, err := json.Marshal(obj)
		if err != nil {
			return TaskComment{}, fmt.Errorf("coord.AgentAppendComment: marshal payload: %w", err)
		}
		payloadArg = string(encoded)
	}

	var created time.Time
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO coord.comment (work_item_id, author_principal, body, payload)
		VALUES ($1::uuid, $2, $3, $4::jsonb)
		RETURNING created_at`, in.WorkItemID, in.Principal, in.Body, payloadArg).Scan(&created); err != nil {
		return TaskComment{}, fmt.Errorf("coord.AgentAppendComment: insert: %w", err)
	}

	// §6.5 provenance in the same txn, fence NULL (a comment is neither a
	// custody nor a lease op) — the same 'work_item_commented' shape the human
	// path writes; the principal IS the agent's server-stamped identity, so
	// the audit row attributes the comment honestly (initiator=agent).
	if err := s.writeAudit(ctx, tx, in.WorkItemID, "work_item_commented", in.Principal, "", map[string]any{}); err != nil {
		return TaskComment{}, err
	}

	if err := tx.Commit(); err != nil {
		return TaskComment{}, fmt.Errorf("coord.AgentAppendComment: commit: %w", err)
	}
	return TaskComment{Author: in.Principal, Body: in.Body, CreatedAt: created, References: resolvedRefs, Mentions: mentions}, nil
}

// normalizeAgentMentions trims, strips a leading @ (the body syntax is @name),
// drops empties, dedupes (first-seen, order preserved) and caps the mention
// set. Pure — unit-tested in workitemagentcomment_unit_test.go.
func normalizeAgentMentions(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, m := range in {
		m = strings.TrimPrefix(strings.TrimSpace(m), "@")
		if m == "" || seen[m] {
			continue
		}
		if len(out) == agentMentionCap {
			break
		}
		seen[m] = true
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
