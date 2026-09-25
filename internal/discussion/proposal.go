// ISI-4928 — Action-proposal messages and their decision lifecycle (plan ISI-4919 §4.4, story 4).
//
// A proposal is an ordinary append-only discussion message with kind='proposal' and a structured
// payload naming the action a human may authorize. It is INERT by construction: posting it writes
// no coord row and moves no custody. The lifecycle `proposed → confirmed | dismissed | executed`
// lives in the discussion.proposal side table (0025) because the message row itself is append-only
// (0004 permits only the invalidated_at soft-retract) and must stay custody-free.
//
// The room never executes anything: ConfirmProposal/DismissProposal are CAS transitions on the
// decision record, and the fan-out into the EXISTING authoring APIs happens in the apiserver
// confirm shell (internal/apiserver/proposalconfirm.go) — zero new custody semantics.
package discussion

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ============================================================================
// Kinds, actions, phases
// ============================================================================

const (
	// KindProposal marks an action-proposal message (rendered as the dashed accent card).
	KindProposal = "proposal"

	// KindStructured is the generic structured kind; confirm results post back under it.
	KindStructured = "structured"

	// Proposal actions — the exact fan-out set from plan §6.
	ProposalActionCreateTicket = "create_ticket" // → POST /api/projects/{pid}/work-items
	ProposalActionAssignAgent  = "assign_agent"  // → POST /api/work-items/{id}/dispatch
	ProposalActionPartyRun     = "party_run"     // → team-targeted work-item mint (no requested_agent)

	// Proposal phases (the decision state machine). Deliberately NOT called "state" — no custody
	// token from the fence's forbidden list enters the discussion schema (see 0025).
	ProposalPhaseProposed  = "proposed"
	ProposalPhaseConfirmed = "confirmed"
	ProposalPhaseDismissed = "dismissed"
	ProposalPhaseExecuted  = "executed"
)

// ProposalPayload is the structured payload of a kind='proposal' message (plan §6 data contract).
type ProposalPayload struct {
	Action          string `json:"action"`                    // create_ticket | assign_agent | party_run
	Title           string `json:"title,omitempty"`           // create_ticket / party_run
	Body            string `json:"body,omitempty"`            // create_ticket / party_run (ticket body)
	AssigneeAgentID string `json:"assigneeAgentId,omitempty"` // assign_agent: Team.Spec.Agents[].Name
	TicketID        string `json:"ticketId,omitempty"`        // assign_agent: existing work-item id
}

// Validate enforces the per-action minimum contract before anything is persisted.
func (p ProposalPayload) Validate() error {
	switch p.Action {
	case ProposalActionCreateTicket, ProposalActionPartyRun:
		if p.Title == "" {
			return fmt.Errorf("%w: action %q requires a title", ErrInvalidProposalPayload, p.Action)
		}
	case ProposalActionAssignAgent:
		if p.TicketID == "" {
			return fmt.Errorf("%w: action assign_agent requires ticketId", ErrInvalidProposalPayload)
		}
		if p.AssigneeAgentID == "" {
			return fmt.Errorf("%w: action assign_agent requires assigneeAgentId", ErrInvalidProposalPayload)
		}
	default:
		return fmt.Errorf("%w: unknown proposal action %q", ErrInvalidProposalPayload, p.Action)
	}
	return nil
}

// Proposal is a proposal message joined with its decision-lifecycle row. TeamID is the owning
// thread's team (the room's tenancy) so the apiserver confirm shell can scope its coord writes
// without a second lookup.
type Proposal struct {
	Message   Message
	TeamID    uuid.UUID
	Payload   ProposalPayload
	Phase     string     `json:"phase"`
	DecidedBy string     `json:"decidedBy,omitempty"`
	DecidedAt *time.Time `json:"decidedAt,omitempty"`
}

// ============================================================================
// Errors
// ============================================================================

var (
	// ErrProposalNotFound — the message is not a proposal, is retracted, or is outside the caller's
	// Team scope (404-not-403, AC5).
	ErrProposalNotFound = errors.New("discussion: proposal not found")
	// ErrProposalNotProposed — the lifecycle already left `proposed` (confirmed/dismissed/executed);
	// confirm and dismiss are one-shot decisions.
	ErrProposalNotProposed = errors.New("discussion: proposal is no longer in the proposed phase")
	// ErrInvalidProposalPayload — payload failed Validate (400).
	ErrInvalidProposalPayload = errors.New("discussion: invalid proposal payload")
)

// ============================================================================
// Store — proposal lifecycle
// ============================================================================

// PostProposal appends an inert kind='proposal' message and its phase='proposed' lifecycle row in
// one transaction. Agents and humans alike may propose — proposing is conversation, not execution.
func (s *Store) PostProposal(ctx context.Context, projectID, teamID, threadID uuid.UUID, auth AuthorContext, body string, payload ProposalPayload, parentID *uuid.UUID) (*Message, error) {
	if body == "" {
		return nil, ErrEmptyBody
	}
	if err := payload.Validate(); err != nil {
		return nil, err
	}
	if err := s.assertThreadInScope(ctx, projectID, teamID, threadID); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	payloadJSON := json.RawMessage(raw)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	m := Message{
		ThreadID:        threadID,
		ParentID:        parentID,
		AuthorPrincipal: auth.Principal,
		AuthorAgentID:   auth.AgentID,
		AuthorRunID:     auth.RunID,
		Body:            body,
		Audience:        "party", // proposals are room-visible: the review audience is the party
		Kind:            KindProposal,
		Payload:         &payloadJSON,
	}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO discussion.message
		    (thread_id, parent_id, author_principal, author_agent_id, author_run_id, body, audience, kind, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, created_at`,
		threadID, parentID, auth.Principal, auth.agentID(), auth.runID(), body, m.Audience, m.Kind, raw,
	).Scan(&m.ID, &m.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("post proposal message: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO discussion.proposal (message_id, phase) VALUES ($1, 'proposed')`, m.ID); err != nil {
		return nil, fmt.Errorf("post proposal lifecycle: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &m, nil
}

// GetProposal returns the proposal message + lifecycle row, tenancy-scoped through thread.
func (s *Store) GetProposal(ctx context.Context, projectID, teamID, messageID uuid.UUID) (*Proposal, error) {
	var p Proposal
	var payload []byte
	var parentID uuid.NullUUID
	var agentID, runID sql.NullString
	var decidedBy sql.NullString
	var decidedAt sql.NullTime
	const q = `
		SELECT m.id, m.thread_id, m.parent_id, m.author_principal, m.author_agent_id, m.author_run_id,
		       m.body, m.audience, m.kind, m.payload, m.created_at, m.invalidated_at,
		       t.team_id, p.phase, p.decided_by, p.decided_at
		FROM discussion.proposal p
		JOIN discussion.message m ON m.id = p.message_id
		JOIN discussion.thread t  ON t.id = m.thread_id
		WHERE p.message_id = $1 AND t.project_id = $2 AND t.team_id = $3
		  AND m.invalidated_at IS NULL`
	err := s.db.QueryRowContext(ctx, q, messageID, projectID, teamID).Scan(
		&p.Message.ID, &p.Message.ThreadID, &parentID, &p.Message.AuthorPrincipal,
		&agentID, &runID, &p.Message.Body, &p.Message.Audience, &p.Message.Kind,
		&payload, &p.Message.CreatedAt, &p.Message.InvalidatedAt,
		&p.TeamID, &p.Phase, &decidedBy, &decidedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProposalNotFound
	}
	if err != nil {
		return nil, err
	}
	if parentID.Valid {
		pid := parentID.UUID
		p.Message.ParentID = &pid
	}
	if agentID.Valid {
		p.Message.AuthorAgentID = &agentID.String
	}
	if runID.Valid {
		p.Message.AuthorRunID = &runID.String
	}
	if err := json.Unmarshal(payload, &p.Payload); err != nil {
		return nil, fmt.Errorf("discussion: proposal %s has a malformed payload: %w", messageID, err)
	}
	if decidedBy.Valid {
		p.DecidedBy = decidedBy.String
	}
	if decidedAt.Valid {
		t := decidedAt.Time
		p.DecidedAt = &t
	}
	return &p, nil
}

// ConfirmProposal CAS-advances proposed→confirmed, stamping the deciding human. It is the
// single-winner lock for the fan-out: the loser of a concurrent confirm gets ErrProposalNotProposed.
// Returns the full proposal (payload included) so the shell can fan out without a re-read.
func (s *Store) ConfirmProposal(ctx context.Context, projectID, teamID, messageID uuid.UUID, auth AuthorContext) (*Proposal, error) {
	if err := s.transitionProposal(ctx, projectID, teamID, messageID, auth.Principal,
		ProposalPhaseProposed, ProposalPhaseConfirmed); err != nil {
		return nil, err
	}
	return s.GetProposal(ctx, projectID, teamID, messageID)
}

// DismissProposal CAS-advances proposed→dismissed. No fan-out, no coord write — dismissing is a
// decision recorded on the card, nothing more.
func (s *Store) DismissProposal(ctx context.Context, projectID, teamID, messageID uuid.UUID, auth AuthorContext) error {
	return s.transitionProposal(ctx, projectID, teamID, messageID, auth.Principal,
		ProposalPhaseProposed, ProposalPhaseDismissed)
}

// CompleteProposal advances confirmed→executed with the fan-out result and appends the post-back
// message (kind='structured', parented to the proposal card) the room renders with a run chip.
//
// The transition, result write, and post-back insert are ONE transaction: a partial failure rolls
// the card back to `confirmed` rather than leaving a half-executed row (executed-but-no-result, or
// result-but-no-post-back). The confirm shell's resume path (ISI-4945) then recovers it. The CAS is
// still one-shot — a card already in `executed` is refused with ErrProposalNotProposed (the shell
// treats that as an idempotent success before ever calling back in here).
func (s *Store) CompleteProposal(ctx context.Context, projectID, teamID, messageID uuid.UUID, auth AuthorContext, result json.RawMessage, resultBody string) (*Message, error) {
	if resultBody == "" {
		resultBody = "Proposal executed."
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	tag, err := tx.ExecContext(ctx, `
		UPDATE discussion.proposal p
		   SET phase = 'executed', decided_by = $4, decided_at = now(), result = $5, updated_at = now()
		FROM discussion.message m JOIN discussion.thread t ON t.id = m.thread_id
		WHERE p.message_id = m.id AND t.project_id = $1 AND t.team_id = $2
		  AND p.message_id = $3 AND m.invalidated_at IS NULL AND p.phase = 'confirmed'`,
		projectID, teamID, messageID, auth.Principal, []byte(result))
	if err != nil {
		return nil, err
	}
	if n, _ := tag.RowsAffected(); n == 0 {
		// Distinguish "invisible" (404) from "visible but not in confirmed" (409) with a scope probe.
		if _, gerr := s.GetProposal(ctx, projectID, teamID, messageID); gerr != nil {
			return nil, gerr
		}
		return nil, ErrProposalNotProposed
	}

	postBack, err := s.postResultMessage(ctx, tx, projectID, teamID, messageID, auth, result, resultBody)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return postBack, nil
}

// FailProposal rolls confirmed back to proposed after a failed fan-out so the human can retry or
// dismiss. The lifecycle never lies about an execution that did not happen.
func (s *Store) FailProposal(ctx context.Context, projectID, teamID, messageID uuid.UUID) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE discussion.proposal p SET phase = 'proposed', decided_by = NULL, decided_at = NULL, updated_at = now()
		FROM discussion.message m JOIN discussion.thread t ON t.id = m.thread_id
		WHERE p.message_id = m.id AND t.project_id = $1 AND t.team_id = $2
		  AND p.message_id = $3 AND p.phase = 'confirmed'`, projectID, teamID, messageID)
	return err
}

// transitionProposal is the tenancy-scoped CAS: phase must be exactly `from` or the transition is
// refused, and a retracted card can never be decided.
func (s *Store) transitionProposal(ctx context.Context, projectID, teamID, messageID uuid.UUID, principal, from, to string) error {
	tag, err := s.db.ExecContext(ctx, `
		UPDATE discussion.proposal p
		   SET phase = $6, decided_by = $5, decided_at = now(), updated_at = now()
		FROM discussion.message m JOIN discussion.thread t ON t.id = m.thread_id
		WHERE p.message_id = m.id AND t.project_id = $1 AND t.team_id = $2
		  AND p.message_id = $3 AND m.invalidated_at IS NULL AND p.phase = $4`,
		projectID, teamID, messageID, from, principal, to)
	if err != nil {
		return err
	}
	if n, _ := tag.RowsAffected(); n == 0 {
		// Distinguish "invisible" (404) from "visible but not in phase" (409) with a scope probe.
		if _, gerr := s.GetProposal(ctx, projectID, teamID, messageID); gerr != nil {
			return gerr
		}
		return ErrProposalNotProposed
	}
	return nil
}

// rowQuerier is the one-row query surface shared by *sql.DB and *sql.Tx, so the post-back insert
// can run inside the CompleteProposal transaction (atomic with the phase transition).
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// postResultMessage appends the confirm post-back: a kind='structured' reply pinned to the
// proposal card whose payload carries the fan-out outcome (the run-chip data, plan §4.7).
func (s *Store) postResultMessage(ctx context.Context, db rowQuerier, projectID, teamID, proposalID uuid.UUID, auth AuthorContext, result json.RawMessage, body string) (*Message, error) {
	var threadID uuid.UUID
	err := db.QueryRowContext(ctx, `
		SELECT m.thread_id FROM discussion.message m
		JOIN discussion.thread t ON t.id = m.thread_id
		WHERE m.id = $1 AND t.project_id = $2 AND t.team_id = $3 AND m.invalidated_at IS NULL`,
		proposalID, projectID, teamID).Scan(&threadID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProposalNotFound
	}
	if err != nil {
		return nil, err
	}
	m := Message{
		ThreadID:        threadID,
		ParentID:        &proposalID,
		AuthorPrincipal: auth.Principal,
		AuthorAgentID:   auth.AgentID,
		AuthorRunID:     auth.RunID,
		Body:            body,
		Audience:        "party",
		Kind:            KindStructured,
		Payload:         &result,
	}
	err = db.QueryRowContext(ctx, `
		INSERT INTO discussion.message
		    (thread_id, parent_id, author_principal, author_agent_id, author_run_id, body, audience, kind, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, created_at`,
		threadID, m.ParentID, auth.Principal, auth.agentID(), auth.runID(), body, m.Audience, m.Kind,
		[]byte(result),
	).Scan(&m.ID, &m.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("post proposal result: %w", err)
	}
	return &m, nil
}
