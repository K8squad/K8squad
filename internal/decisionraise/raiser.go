// Package decisionraise is the shared runtime emission path for board decision
// requests (ISI-7224, ISI-7074 WS-6.2, plan ISI-7204 rev 1): ONE typed helper
// that resolves the raising run's discussion thread, validates + auto-letters
// the ask at a single point, and posts the inert kind='decision_request' card
// (ADR-0026 §4) the inbox DecisionCard arm already renders (ISI-5536 BE-7).
//
// The emission surfaces ride this package: the run-scoped task-io verb
// POST /api/task-io/request-decision (every sandboxed runtime — the operator
// injects KSQUAD_COORD_URL/KSQUAD_COORD_TOKEN into rundrive-dispatched shims
// and the warm pool mounts the same token as a Secret), so any runtime flavor
// can raise one with the credential it already holds.
//
// Custody semantics: NONE added. The card is inert by construction
// (discussion.PostDecisionRequest); the human-gated answer shell's existing
// resume_agent_on_answer continuation (RequestDispatch of the bound work item)
// is the only thing that moves the ticket — this package never dispatches.
package decisionraise

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
)

// ThreadResolver answers "which room thread was this run dispatched from?" —
// the SAME dispatch-ledger lookup propose mode rides (ISI-5282):
// *mentiondispatch.ProposalThreadResolver satisfies it structurally, so there
// is one ledger, not two.
type ThreadResolver interface {
	ThreadForDispatchedRun(ctx context.Context, runID string) (projectID string, teamID, threadID uuid.UUID, ok bool, err error)
}

// DecisionPoster is the fenced create the raise rides — *discussion.Store
// satisfies it via PostDecisionRequest, the SAME method the REST thread-scoped
// create uses, so tenancy scoping and the idempotent create live in the store,
// not here.
type DecisionPoster interface {
	PostDecisionRequest(ctx context.Context, projectID string, teamID, threadID uuid.UUID, auth discussion.AuthorContext, body string, payload discussion.DecisionRequestPayload, target *discussion.DecisionTarget, workItemID, idempotencyKey string, parentID *uuid.UUID) (*discussion.Message, error)
}

// ErrNoRaisingThread is the honest refusal for a run with no originating
// discussion thread (e.g. a board-intake run, not a room @-mention run): there
// is no thread to raise the card in, so the helper refuses rather than guessing
// one — the ticket-to-thread mapping gap is owned by ISI-5283 WS-2.
var ErrNoRaisingThread = errors.New("decisionraise: this run was not dispatched from a discussion thread — no thread to raise a decision in")

// Raiser turns a runtime's typed ask into a posted decision card. Built from a
// thread resolver + the fenced poster; nil either refuses construction (the
// emission edge then reports the surface unwired, never a silent no-op).
type Raiser struct {
	resolver ThreadResolver
	poster   DecisionPoster
	// suggest is the OPTIONAL suggest-next-steps emission seam (ISI-7218,
	// ISI-7075 C4): nil => SuggestNextSteps refuses with ErrSuggestionsUnwired
	// (the surface reports unwired, never a silent no-op).
	suggest SuggestionPoster
}

// New bundles the resolver + poster. It returns an error when either seam is
// absent so a caller wires the verb only when the raise can actually land.
func New(resolver ThreadResolver, poster DecisionPoster) (*Raiser, error) {
	if resolver == nil || poster == nil {
		return nil, errors.New("decisionraise.New: resolver and poster are required")
	}
	return &Raiser{resolver: resolver, poster: poster}, nil
}

// RequestDecision raises one board-decision card from a run. Attribution is
// server-stamped from the run identity (principal + runID — the task-io token's
// binding), never client text; workItemID binds the card to the ticket it
// decides so the existing answer-shell continuation re-dispatches THAT item and
// the inbox arm joins it (ADR-0026 §3.3/§5). The idempotency key is derived
// deterministically from (runID, ask), so an at-least-once retry returns the
// SAME card — never a duplicate question.
func (r *Raiser) RequestDecision(ctx context.Context, runID, principal, workItemID string, ask discussion.DecisionRaise) (*discussion.Message, error) {
	payload, err := ask.BuildPayload()
	if err != nil {
		return nil, err
	}
	projectID, teamID, threadID, ok, err := r.resolver.ThreadForDispatchedRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("decisionraise: resolve raising thread for run %s: %w", runID, err)
	}
	if !ok {
		return nil, ErrNoRaisingThread
	}
	agent, run := principal, runID
	auth := discussion.AuthorContext{
		Principal: principal,
		TeamID:    teamID,
		AgentID:   &agent,
		RunID:     &run,
	}
	body := payload.Title
	if payload.DetailsMarkdown != "" {
		body += "\n\n" + payload.DetailsMarkdown
	}
	msg, err := r.poster.PostDecisionRequest(ctx, projectID, teamID, threadID, auth, body, payload, nil, workItemID, raiseIdempotencyKey(runID, payload), nil)
	if err != nil {
		return nil, err
	}
	return msg, nil
}

// raiseIdempotencyKey derives the deterministic create key: same run + same ask
// (mode/title/options) ⇒ same key ⇒ the store's idempotent create returns the
// existing card on a retry.
func raiseIdempotencyKey(runID string, payload discussion.DecisionRequestPayload) string {
	labels := make([]string, len(payload.Options))
	for i, o := range payload.Options {
		labels[i] = o.ID + ":" + o.Description
	}
	sum := sha256.Sum256([]byte(payload.Mode + "\x00" + payload.Title + "\x00" + strings.Join(labels, "\x00")))
	return fmt.Sprintf("decision-raise:%s:%s", runID, hex.EncodeToString(sum[:8]))
}

// ============================================================================
// Suggest-next-steps emission (ISI-7218, ISI-7075 C4 — taxonomy §5)
// ============================================================================

// SuggestionPoster is the fenced create the suggest raise rides — *discussion.Store
// satisfies it via PostSuggestNextSteps, the same store the decision poster uses,
// so tenancy scoping and the idempotent create live in the store, not here.
type SuggestionPoster interface {
	PostSuggestNextSteps(ctx context.Context, projectID string, teamID, threadID uuid.UUID, auth discussion.AuthorContext, body string, payload discussion.DecisionRequestPayload, workItemID, idempotencyKey string, parentID *uuid.UUID) (*discussion.Message, error)
}

// ErrSuggestionsUnwired is the honest refusal when the suggest seam was not armed.
var ErrSuggestionsUnwired = errors.New("decisionraise: suggest-next-steps emission is not wired on this deployment")

// WithSuggestions arms the suggest-next-steps emission. Optional by design: a
// host wiring only the decision verb keeps New's two-seam contract; an unarmed
// Raiser refuses SuggestNextSteps loudly.
func (r *Raiser) WithSuggestions(poster SuggestionPoster) *Raiser {
	r.suggest = poster
	return r
}

// SuggestNextSteps raises one kind='suggest_next_steps' card from a blocked run
// (ISI-7218): the same resolve→validate→idempotent-post chain as
// RequestDecision, stamped with the suggest kind and its kind-specific gate
// (BuildSuggestPayload). The board answers through the EXISTING decision
// answer/reject shells; the resume_agent_on_answer continuation re-dispatches
// this run's work item — nothing here adds custody semantics.
func (r *Raiser) SuggestNextSteps(ctx context.Context, runID, principal, workItemID string, ask discussion.SuggestNextStepsRaise) (*discussion.Message, error) {
	if r.suggest == nil {
		return nil, ErrSuggestionsUnwired
	}
	payload, err := ask.BuildSuggestPayload()
	if err != nil {
		return nil, err
	}
	projectID, teamID, threadID, ok, err := r.resolver.ThreadForDispatchedRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("decisionraise: resolve raising thread for run %s: %w", runID, err)
	}
	if !ok {
		return nil, ErrNoRaisingThread
	}
	agent, run := principal, runID
	auth := discussion.AuthorContext{
		Principal: principal,
		TeamID:    teamID,
		AgentID:   &agent,
		RunID:     &run,
	}
	body := payload.Title
	if payload.DetailsMarkdown != "" {
		body += "\n\n" + payload.DetailsMarkdown
	}
	msg, err := r.suggest.PostSuggestNextSteps(ctx, projectID, teamID, threadID, auth, body, payload, workItemID, suggestIdempotencyKey(runID, payload), nil)
	if err != nil {
		return nil, err
	}
	return msg, nil
}

// suggestIdempotencyKey derives the deterministic create key for a suggest raise
// (distinct prefix from decision raises so the two kinds never collide): same run
// + same ask ⇒ same key ⇒ the store's idempotent create returns the existing card.
func suggestIdempotencyKey(runID string, payload discussion.DecisionRequestPayload) string {
	labels := make([]string, len(payload.Options))
	for i, o := range payload.Options {
		labels[i] = o.ID + ":" + o.Description
	}
	sum := sha256.Sum256([]byte(payload.Mode + "\x00" + payload.Title + "\x00" + strings.Join(labels, "\x00")))
	return fmt.Sprintf("suggest-raise:%s:%s", runID, hex.EncodeToString(sum[:8]))
}
