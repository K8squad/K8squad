package apiserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/coord"
)

// ============================================================================
// Dispatch-on-mention run-minting (ISI-5116, parent ISI-5108, ruling ADR-0024b / ISI-5117).
// ============================================================================
//
// The decision half (ISI-5108, internal/discussion/dispatch.go) parses @-mentions in a room post,
// applies the four guardrails (loop cap ≤2, de-dupe, rate 5/msg, opt-out), and emits a MentionDispatch
// per matched agent through the discussion.MentionDispatcher seam. This file is the apiserver-side
// IMPLEMENTATION of that seam: it turns each MentionDispatch into a real agent Run.
//
// Per the Architect's ruling (ADR-0024b §2.2, "A2"): the minting principal is the discussion dispatch
// engine — a trusted, server-side component, a PEER of Intake / ADR-0022 board dispatch / GitHub-Kanban
// intake, NOT the triggering agent and NOT the room. It therefore sits OUTSIDE ADR-0024's agent-
// authoring scope, engages neither authoring wall, and reuses coord.CreateWorkItem + coord.RequestDispatch
// VERBATIM (no new exported pkg/coord symbol ⇒ FR-B3 untripped, §4.3). The one governance condition is
// that the minted per-mention item is board-HIDDEN (source='discussion', §4) — see markSourceDiscussion
// and the board-list allowlist filters (WHERE source='board').
//
// Option B (a Run bound to {projectId,threadId,messageId} with no board work item) is mechanically dead:
// RunSpec.WorkItemRef is Required + uuid-validated and Runs mint only via Intake scanning
// coord.work_item WHERE state='todo'. So the thread context must ride a real (board-hidden) work item.

// dispatchPrincipal is the trusted server principal that authors thread-run drive items — the discussion
// dispatch engine (ADR-0024b §3), a peer of Intake, never the mentioned or triggering agent. It is the
// created_by stamped on the minted work item and the dispatch principal on RequestDispatch.
const dispatchPrincipal = "discussion-dispatch"

// mentionDispatchLedger is the idempotency + reply-hop persistence seam over discussion.mention_dispatch
// (migration 0027). Kept an interface so the dispatcher is unit-testable against a fake without Postgres.
type mentionDispatchLedger interface {
	// Claim records (messageId, agentName) as the first-writer-wins idempotency marker. claimed=true ⇒
	// this caller won the race and must proceed to mint; claimed=false ⇒ an equal or earlier delivery
	// already dispatched this (message, agent) pair, so the caller must no-op (never double-run).
	Claim(ctx context.Context, d discussion.MentionDispatch) (claimed bool, err error)
	// Bind links the claimed row to its minted drive item so the reply path can recover the hop from a
	// Run's identity (coord.claim.run_id → work_item_id → mention_dispatch.hop_depth).
	Bind(ctx context.Context, messageID uuid.UUID, agentName, workItemID string) error
	// Release removes a claim whose mint failed, so a later retry can redo the dispatch cleanly.
	Release(ctx context.Context, messageID uuid.UUID, agentName string) error
	// HopForDispatchedRun implements discussion.ReplyHopResolver (see NewReplyHopResolver).
	HopForDispatchedRun(ctx context.Context, runID string) (hop int, ok bool, err error)
}

// mentionDispatcher implements discussion.MentionDispatcher — the run-minting half.
type mentionDispatcher struct {
	create     WorkItemWriter
	dispatch   WorkItemDispatcher
	refs       ProjectRefResolver
	ledger     mentionDispatchLedger
	markSource func(ctx context.Context, workItemID string) error // flips coord.work_item.source → 'discussion'
}

// NewMentionDispatcher builds the apiserver-side implementation of discussion.MentionDispatcher, wired
// to the SAME coord seams the human board create/dispatch handlers ride (WorkItemWriter,
// WorkItemDispatcher) plus the project-ref resolver and the Postgres dispatch ledger. Wire it onto the
// discussion handler with (*discussion.Handler).SetMentionDispatcher.
func NewMentionDispatcher(create WorkItemWriter, dispatch WorkItemDispatcher, refs ProjectRefResolver, db *sql.DB) discussion.MentionDispatcher {
	return &mentionDispatcher{
		create:     create,
		dispatch:   dispatch,
		refs:       refs,
		ledger:     pgMentionLedger{db: db},
		markSource: markSourceDiscussion(db),
	}
}

// NewReplyHopResolver builds the discussion.ReplyHopResolver over the same dispatch ledger, so the
// discussion handler can stamp an agent reply's loop-guard hop from the Run's identity. Wire it with
// (*discussion.Handler).SetReplyHopResolver.
func NewReplyHopResolver(db *sql.DB) discussion.ReplyHopResolver {
	return pgMentionLedger{db: db}
}

// DispatchMention turns one resolved @-mention into a real agent Run. It is idempotent on
// (MessageID, AgentName): a duplicate at-least-once delivery is a no-op.
func (m *mentionDispatcher) DispatchMention(ctx context.Context, d discussion.MentionDispatch) error {
	if d.AgentName == "" {
		return fmt.Errorf("mention dispatch: empty agent name")
	}

	// (1) Idempotency claim — first writer wins on (messageId, agentName). A redelivery no-ops here, so
	// one @-mention never mints two Runs for the same agent.
	claimed, err := m.ledger.Claim(ctx, d)
	if err != nil {
		return fmt.Errorf("mention dispatch: claim (%s,%s): %w", d.MessageID, d.AgentName, err)
	}
	if !claimed {
		return nil // already dispatched for this (message, agent)
	}
	// Any failure before the Run intent is durably recorded releases the claim so a retry can redo. A
	// success clears this. (A late RequestDispatch failure leaves a board-hidden orphan backlog item —
	// harmless: no Run was minted, and the item never surfaces to a human — while allowing a retry.)
	minted := false
	defer func() {
		if !minted {
			_ = m.ledger.Release(ctx, d.MessageID, d.AgentName)
		}
	}()

	// (2) Resolve the room's "namespace/name" slug to the coord Project UID + owning Team UID, exactly
	// like the board create/dispatch handlers (proposalFanout.execute). A UID-bearing id passes through.
	projectID := d.ProjectID
	teamUID := d.TeamID.String()
	if m.refs != nil {
		resolved, rerr := m.refs.ResolveProjectRef(ctx, d.ProjectID)
		if rerr != nil {
			return fmt.Errorf("mention dispatch: resolve project %q: %w", d.ProjectID, rerr)
		}
		projectID = resolved.UID
		if resolved.TeamUID != "" {
			teamUID = resolved.TeamUID
		}
	}

	// (3) Mint the drive item carrying the thread context + "read thread X, reply in the room"
	// instruction. Reuses coord.CreateWorkItem verbatim; it lands in 'backlog' with source='board' (DB
	// default), flipped to 'discussion' next.
	rec, err := m.create.CreateWorkItem(ctx, coord.CreateWorkItemInput{
		ProjectID: projectID,
		TeamID:    teamUID,
		Title:     mentionRunTitle(d),
		Body:      mentionRunBody(d),
		Principal: dispatchPrincipal,
	})
	if err != nil {
		return fmt.Errorf("mention dispatch: create work item: %w", err)
	}

	// (4) Flip the board-hide marker BEFORE RequestDispatch advances the lane, so the item is never
	// board-visible in a lane a human sees (ADR-0024b §4.2). The board-list queries filter source='board'.
	if m.markSource != nil {
		if err := m.markSource(ctx, rec.ID); err != nil {
			return fmt.Errorf("mention dispatch: mark source discussion (%s): %w", rec.ID, err)
		}
	}

	// (5) Bind the item to the ledger row so the reply path can recover the hop from a Run id.
	if err := m.ledger.Bind(ctx, d.MessageID, d.AgentName, rec.ID); err != nil {
		return fmt.Errorf("mention dispatch: bind work item (%s): %w", rec.ID, err)
	}

	// (6) Request dispatch: backlog→todo, stamping requested_agent = the mentioned agent, so the operator
	// Intake sweep mints the Run for exactly that agent. Initiator provenance mirrors the trigger.
	initiator := "human"
	if d.TriggeredByAgentID != nil {
		initiator = "agent"
	}
	if _, err := m.dispatch.RequestDispatch(ctx, coord.RequestDispatchInput{
		WorkItemID: rec.ID,
		AgentID:    d.AgentName,
		TeamID:     teamUID,
		Principal:  dispatchPrincipal,
		Initiator:  initiator,
	}); err != nil {
		return fmt.Errorf("mention dispatch: request dispatch (%s→%s): %w", rec.ID, d.AgentName, err)
	}
	minted = true
	return nil
}

// mentionRunTitle is the board-hidden drive item's title — descriptive for the audit trail, never seen
// on a human board.
func mentionRunTitle(d discussion.MentionDispatch) string {
	return fmt.Sprintf("Discussion reply: @%s in thread %s", d.AgentName, d.ThreadID)
}

// mentionRunBody is the run's instruction + thread context. The Run receives only WorkItemRef; the
// driver fetches this body by id at dispatch time (rundrive/dispatch.go), so the agent's context is the
// instruction to read the thread and reply in the room, plus the loop-guard hop it must run at.
func mentionRunBody(d discussion.MentionDispatch) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You were @-mentioned in the discussion room for project %q.\n\n", d.ProjectID)
	fmt.Fprintf(&b, "1. Read the thread (thread id %s) — the triggering message is %s.\n", d.ThreadID, d.MessageID)
	fmt.Fprintf(&b, "   Use the discussion_search tool (or GET the thread) to load recent messages for context.\n")
	fmt.Fprintf(&b, "2. Reply IN THE ROOM by POSTing to the thread's messages endpoint\n")
	fmt.Fprintf(&b, "   (POST /api/projects/%s/discussion/threads/%s/messages). Do NOT open a board ticket.\n", d.ProjectID, d.ThreadID)
	fmt.Fprintf(&b, "\nThis is a conversation, not custody: your only deliverable is the reply message.\n")
	// The hop is enforced server-side on the reply (the loop guard reads the Run's identity, not this
	// number), but recording it makes the run's provenance legible in the item body.
	fmt.Fprintf(&b, "\n[dispatch] hopDepth=%d principal=%s\n", d.HopDepth, d.TriggeredByPrincipal)
	return b.String()
}

// ----------------------------------------------------------------------------
// Postgres ledger (discussion.mention_dispatch, migration 0027)
// ----------------------------------------------------------------------------

type pgMentionLedger struct{ db *sql.DB }

func (l pgMentionLedger) Claim(ctx context.Context, d discussion.MentionDispatch) (bool, error) {
	res, err := l.db.ExecContext(ctx, `
		INSERT INTO discussion.mention_dispatch (message_id, agent_name, project_id, thread_id, hop_depth)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (message_id, agent_name) DO NOTHING`,
		d.MessageID, d.AgentName, d.ProjectID, d.ThreadID, d.HopDepth)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func (l pgMentionLedger) Bind(ctx context.Context, messageID uuid.UUID, agentName, workItemID string) error {
	_, err := l.db.ExecContext(ctx, `
		UPDATE discussion.mention_dispatch SET work_item_id = $3::uuid
		 WHERE message_id = $1 AND agent_name = $2`, messageID, agentName, workItemID)
	return err
}

func (l pgMentionLedger) Release(ctx context.Context, messageID uuid.UUID, agentName string) error {
	_, err := l.db.ExecContext(ctx, `
		DELETE FROM discussion.mention_dispatch WHERE message_id = $1 AND agent_name = $2`,
		messageID, agentName)
	return err
}

func (l pgMentionLedger) HopForDispatchedRun(ctx context.Context, runID string) (int, bool, error) {
	if strings.TrimSpace(runID) == "" {
		return 0, false, nil
	}
	var hop int
	err := l.db.QueryRowContext(ctx, `
		SELECT md.hop_depth
		  FROM discussion.mention_dispatch md
		  JOIN coord.claim c ON c.work_item_id = md.work_item_id
		 WHERE c.run_id = $1::uuid`, runID).Scan(&hop)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil // not a thread-run — a normal post stays hop 0
	}
	if err != nil {
		return 0, false, err
	}
	return hop, true, nil
}

// markSourceDiscussion returns the board-hide marker setter: it flips a just-created work item's source
// to 'discussion' so it never surfaces on a human board (ADR-0024b §4). Kept at the apiserver edge (a
// direct UPDATE, not a coord.CreateWorkItem parameter) so no new exported pkg/coord symbol is introduced
// and the create seam stays reused verbatim (FR-B3 untripped).
func markSourceDiscussion(db *sql.DB) func(context.Context, string) error {
	return func(ctx context.Context, workItemID string) error {
		_, err := db.ExecContext(ctx,
			`UPDATE coord.work_item SET source = 'discussion' WHERE id = $1::uuid`, workItemID)
		return err
	}
}
