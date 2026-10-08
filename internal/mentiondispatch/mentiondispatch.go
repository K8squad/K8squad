// Package mentiondispatch is the run-minting half of dispatch-on-mention (ISI-5116, parent ISI-5108,
// ruling ADR-0024b / ISI-5117), hoisted OUT of internal/apiserver so BOTH write edges of the discussion
// room drive the identical implementation and cannot drift (ISI-5125):
//
//   - the apiserver REST Handler (POST /api/projects/{id}/discussion/threads/{tid}/messages), and
//   - the cmd/memory `discussion_post` MCP tool, which runs in a SEPARATE process and calls
//     discussion.Store.PostMessage directly.
//
// Before ISI-5125 the dispatcher lived in internal/apiserver; the memory service could not reuse it
// (internal/apiserver imports internal/memory, so a memory→apiserver import would be a cycle). This
// package depends only on internal/discussion (the seam types) + pkg/coord (the board authoring stores),
// so both callers import it without a cycle. internal/apiserver keeps thin New* delegators (adapting its
// own WorkItemWriter/ProjectRefResolver seams onto this package's) so cmd/apiserver wiring is untouched.
//
// The decision half (ISI-5108, internal/discussion/dispatch.go) parses @-mentions in a room post, applies
// the four guardrails (loop cap ≤2, de-dupe, rate 5/msg, opt-out), and emits a discussion.MentionDispatch
// per matched agent. This package is the IMPLEMENTATION of the discussion.MentionDispatcher seam: it turns
// each MentionDispatch into a real agent Run.
//
// Per the Architect's ruling (ADR-0024b §2.2, "A2"): the minting principal is the discussion dispatch
// engine — a trusted, server-side component, a PEER of Intake / ADR-0022 board dispatch / GitHub-Kanban
// intake, NOT the triggering agent and NOT the room. It therefore sits OUTSIDE ADR-0024's agent-authoring
// scope, engages neither authoring wall, and reuses coord.CreateWorkItem + coord.RequestDispatch VERBATIM
// (no new exported pkg/coord symbol ⇒ FR-B3 untripped, §4.3). The one governance condition is that the
// minted per-mention item is board-HIDDEN (source='discussion', §4) — see markSourceDiscussion and the
// board-list allowlist filters (WHERE source='board').
//
// Option B (a Run bound to {projectId,threadId,messageId} with no board work item) is mechanically dead:
// RunSpec.WorkItemRef is Required + uuid-validated and Runs mint only via Intake scanning
// coord.work_item WHERE state='todo'. So the thread context must ride a real (board-hidden) work item.
package mentiondispatch

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

// dispatchPrincipal is the trusted server principal that authors thread-run drive items — the discussion
// dispatch engine (ADR-0024b §3), a peer of Intake, never the mentioned or triggering agent. It is the
// created_by stamped on the minted work item and the dispatch principal on RequestDispatch.
const dispatchPrincipal = "discussion-dispatch"

// WorkItemCreator is the board-create seam the dispatcher rides — the trusted server CreateWorkItem
// (pkg/coord/workitemwrite.go), NOT the ADR-0024 agent-authoring verb (which forbids ROOT items). Both
// the apiserver WorkItemWriter and *coord.WorkItemWriteStore satisfy it structurally (they carry
// CreateWorkItem plus more), so each caller passes the store it already builds without an adapter.
type WorkItemCreator interface {
	CreateWorkItem(ctx context.Context, in coord.CreateWorkItemInput) (coord.WorkItemRecord, error)
}

// WorkItemDispatcher is the board-dispatch seam — the trusted server RequestDispatch
// (pkg/coord/workitemdispatch.go), backlog→todo stamping requested_agent. The apiserver WorkItemDispatcher
// and *coord.WorkItemDispatchStore both satisfy it.
type WorkItemDispatcher interface {
	RequestDispatch(ctx context.Context, in coord.RequestDispatchInput) (coord.WorkItemDispatchResult, error)
}

// ResolvedProject is a room's platform-project reference resolved to the coord identities CreateWorkItem
// keys on: the Project CR UID and its owning Team UID.
type ResolvedProject struct {
	UID     string
	TeamUID string
}

// ProjectResolver resolves the room's "namespace/name" slug (ISI-3982) to the coord Project UID + owning
// Team UID. A UID-bearing ref should pass through unchanged. It is the one seam that differs by caller:
// apiserver adapts its ProjectRefResolver over the console informer cache; cmd/memory uses
// NewClientProjectResolver over its own Team-CR cache reader.
type ProjectResolver interface {
	ResolveProject(ctx context.Context, projectRef string) (ResolvedProject, error)
}

// ledger is the idempotency + reply-hop persistence seam over discussion.mention_dispatch (migration
// 0027). Kept an interface so the dispatcher is unit-testable against a fake without Postgres.
type ledger interface {
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

// dispatcher implements discussion.MentionDispatcher — the run-minting half.
type dispatcher struct {
	create     WorkItemCreator
	dispatch   WorkItemDispatcher
	refs       ProjectResolver
	ledger     ledger
	markSource func(ctx context.Context, workItemID string) error // flips coord.work_item.source → 'discussion'
}

// New builds the run-minting implementation of discussion.MentionDispatcher, wired to the trusted board
// create/dispatch seams plus a project resolver and the Postgres dispatch ledger over db. Wire the result
// onto a discussion handler with (*discussion.Handler).SetMentionDispatcher, or onto the cmd/memory tool
// surface via memory.NewDiscussionDispatch.
func New(create WorkItemCreator, dispatch WorkItemDispatcher, refs ProjectResolver, db *sql.DB) discussion.MentionDispatcher {
	return &dispatcher{
		create:     create,
		dispatch:   dispatch,
		refs:       refs,
		ledger:     pgLedger{db: db},
		markSource: markSourceDiscussion(db),
	}
}

// NewReplyHopResolver builds the discussion.ReplyHopResolver over the same dispatch ledger, so a write
// edge can stamp an agent reply's loop-guard hop from the Run's identity. Wire it with
// (*discussion.Handler).SetReplyHopResolver, or hand it to memory.NewDiscussionDispatch.
func NewReplyHopResolver(db *sql.DB) discussion.ReplyHopResolver {
	return pgLedger{db: db}
}

// ProposalThreadResolver answers "which room thread was this run dispatched from?"
// over the SAME dispatch ledger (ISI-5282 propose mode). It satisfies
// memory.ProposalThreadResolver structurally (primitives-only signature), so
// cmd/memory can hand it to the authoring tool surface without an import cycle.
type ProposalThreadResolver struct{ db *sql.DB }

// NewProposalThreadResolver builds the propose-mode thread lookup over the dispatch
// ledger: a coordinator Run minted by a room @-mention is bound (Bind) to its drive
// item, so runID → coord.claim.run_id → mention_dispatch.work_item_id recovers the
// originating thread + its project/team. A propose-mode coordinator posts its
// create_ticket/assign_agent proposals back into exactly that conversation.
func NewProposalThreadResolver(db *sql.DB) *ProposalThreadResolver {
	return &ProposalThreadResolver{db: db}
}

// ThreadForDispatchedRun resolves the room thread (project slug, team uid, thread id)
// a Run was dispatched from. ok=false for any run that is NOT a room thread-run (no
// mention_dispatch row for its claim) — there is no originating thread, so the caller
// refuses propose mode honestly rather than guessing. A blank runID is a non-run post
// (ok=false, no error).
func (r *ProposalThreadResolver) ThreadForDispatchedRun(ctx context.Context, runID string) (projectID string, teamID, threadID uuid.UUID, ok bool, err error) {
	if strings.TrimSpace(runID) == "" {
		return "", uuid.Nil, uuid.Nil, false, nil
	}
	err = r.db.QueryRowContext(ctx, `
		SELECT t.project_id, t.team_id, md.thread_id
		  FROM discussion.mention_dispatch md
		  JOIN coord.claim c       ON c.work_item_id = md.work_item_id
		  JOIN discussion.thread t ON t.id = md.thread_id
		 WHERE c.run_id = $1::uuid`, runID).Scan(&projectID, &teamID, &threadID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", uuid.Nil, uuid.Nil, false, nil // not a thread-run — no proposal thread
	}
	if err != nil {
		return "", uuid.Nil, uuid.Nil, false, err
	}
	return projectID, teamID, threadID, true, nil
}

// DispatchMention turns one resolved @-mention into a real agent Run. It is idempotent on
// (MessageID, AgentName): a duplicate at-least-once delivery is a no-op.
func (m *dispatcher) DispatchMention(ctx context.Context, d discussion.MentionDispatch) error {
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
		resolved, rerr := m.refs.ResolveProject(ctx, d.ProjectID)
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
	if d.Orchestrate {
		return fmt.Sprintf("Discussion coordination: @%s structure work in thread %s", d.AgentName, d.ThreadID)
	}
	return fmt.Sprintf("Discussion reply: @%s in thread %s", d.AgentName, d.ThreadID)
}

// orchestrationRunBody is the multi-mention coordinator directive (ISI-5283 / ISI-5267 WS-2): a post
// that resolved 2+ @-mentions dispatches the Team's Coordinator ONCE, asking it to STRUCTURE the work
// involving the mentioned agents rather than each agent replying independently. The coordinator is the
// LIVE ISI-5220 orchestrator, so the directive points it at its existing authoring verbs
// (work_item_create to decompose, work_item_assign to hand each sub-item to the right agent). The run
// still reads the thread for full context; the structured directive just frames the task.
func orchestrationRunBody(d discussion.MentionDispatch) string {
	var b strings.Builder
	mentioned := make([]string, 0, len(d.OrchestratedAgents))
	for _, a := range d.OrchestratedAgents {
		mentioned = append(mentioned, "@"+a)
	}
	involved := strings.Join(mentioned, ", ")
	fmt.Fprintf(&b, "You are the Team Coordinator for project %q. A discussion-room post @-mentioned\n", d.ProjectID)
	fmt.Fprintf(&b, "multiple agents (%s), so instead of each replying independently you are asked to\n", involved)
	fmt.Fprintf(&b, "STRUCTURE the work involving %s for this ticket.\n\n", involved)
	fmt.Fprintf(&b, "The request is: «%s»\n\n", strings.TrimSpace(d.RequestBody))
	fmt.Fprintf(&b, "1. Read the thread (thread id %s) — the triggering message is %s —\n", d.ThreadID, d.MessageID)
	fmt.Fprintf(&b, "   using the discussion_search tool (or GET the thread) for full context.\n")
	fmt.Fprintf(&b, "2. Decompose the request into work items with work_item_create and assign each to the\n")
	fmt.Fprintf(&b, "   right agent among %s with work_item_assign (your LIVE orchestration verbs).\n", involved)
	fmt.Fprintf(&b, "3. Post a short summary of the plan IN THE ROOM by POSTing to the thread's messages\n")
	fmt.Fprintf(&b, "   endpoint (POST /api/projects/%s/discussion/threads/%s/messages).\n", d.ProjectID, d.ThreadID)
	fmt.Fprintf(&b, "\n[dispatch] orchestrate hopDepth=%d principal=%s\n", d.HopDepth, d.TriggeredByPrincipal)
	return b.String()
}

// partyRunBody is a party-mode VOICE dispatch body (ISI-5586 WS-C, ADR-0027 §3.2). It front-loads the
// cross-talk context the facilitator assembled — persona self-framing, the rolling <400-word summary,
// "What Others Said This Round", and the disagree/pass guidelines (discussion.PartyContext.RenderVoiceContext)
// — then appends the same read-thread / reply-in-room / hop footer every thread-run carries. The point of
// WS-C: the voice is PUSHED the right context instead of having to pull the whole transcript, and it is
// framed as a named persona with a mandate to actually disagree rather than a lone replier.
func partyRunBody(d discussion.MentionDispatch) string {
	var b strings.Builder
	b.WriteString(d.Party.RenderVoiceContext())
	fmt.Fprintf(&b, "\n## Your deliverable\n")
	fmt.Fprintf(&b, "1. Read the thread (thread id %s) — the triggering message is %s — with the\n", d.ThreadID, d.MessageID)
	fmt.Fprintf(&b, "   discussion_search tool (or GET the thread) if you need more than the context above.\n")
	fmt.Fprintf(&b, "2. Post ONE reply IN THE ROOM in your own voice by POSTing to the thread's messages endpoint\n")
	fmt.Fprintf(&b, "   (POST /api/projects/%s/discussion/threads/%s/messages). Do NOT open a board ticket.\n", d.ProjectID, d.ThreadID)
	fmt.Fprintf(&b, "\nThis is a conversation, not custody: your only deliverable is the reply message.\n")
	fmt.Fprintf(&b, "\n[dispatch] party round=%d hopDepth=%d principal=%s\n", d.Party.Round, d.HopDepth, d.TriggeredByPrincipal)
	return b.String()
}

// mentionRunBody is the run's instruction + thread context. The Run receives only WorkItemRef; the
// driver fetches this body by id at dispatch time (rundrive/dispatch.go), so the agent's context is the
// instruction to read the thread and reply in the room, plus the loop-guard hop it must run at.
func mentionRunBody(d discussion.MentionDispatch) string {
	if d.Orchestrate {
		return orchestrationRunBody(d)
	}
	if d.Party != nil {
		return partyRunBody(d)
	}
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

type pgLedger struct{ db *sql.DB }

func (l pgLedger) Claim(ctx context.Context, d discussion.MentionDispatch) (bool, error) {
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

func (l pgLedger) Bind(ctx context.Context, messageID uuid.UUID, agentName, workItemID string) error {
	_, err := l.db.ExecContext(ctx, `
		UPDATE discussion.mention_dispatch SET work_item_id = $3::uuid
		 WHERE message_id = $1 AND agent_name = $2`, messageID, agentName, workItemID)
	return err
}

func (l pgLedger) Release(ctx context.Context, messageID uuid.UUID, agentName string) error {
	_, err := l.db.ExecContext(ctx, `
		DELETE FROM discussion.mention_dispatch WHERE message_id = $1 AND agent_name = $2`,
		messageID, agentName)
	return err
}

func (l pgLedger) HopForDispatchedRun(ctx context.Context, runID string) (int, bool, error) {
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
// to 'discussion' so it never surfaces on a human board (ADR-0024b §4). Kept at the edge (a direct
// UPDATE, not a coord.CreateWorkItem parameter) so no new exported pkg/coord symbol is introduced and the
// create seam stays reused verbatim (FR-B3 untripped).
func markSourceDiscussion(db *sql.DB) func(context.Context, string) error {
	return func(ctx context.Context, workItemID string) error {
		_, err := db.ExecContext(ctx,
			`UPDATE coord.work_item SET source = 'discussion' WHERE id = $1::uuid`, workItemID)
		return err
	}
}
