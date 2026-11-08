package apiserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/K8squad/K8squad/internal/decisionraise"
	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/internal/mentiondispatch"
	"github.com/K8squad/K8squad/pkg/taskio"
)

// taskiodecisionraise.go — ISI-7224 (ISI-7074 WS-6.2): the host adapter that
// binds the shared decision-raise helper (internal/decisionraise — the single
// validation point) onto the task-io seam. It composes the SAME two seams the
// propose-mode coordinator rides (the dispatch-ledger thread resolver,
// ISI-5282, and the fenced discussion create, ISI-5536) — zero new custody
// semantics — and translates the discussion package's sentinels onto the
// primitives-only taskio contract so pkg/taskio never imports the discussion
// schema.

// NewTaskIODecisionRaise wires the task-io decision seam over the coord
// dispatch ledger (run → originating room thread) and the discussion store
// (the fenced, idempotent create). Either nil ⇒ error: the verb stays unwired
// (501), never half-mounted.
func NewTaskIODecisionRaise(db *sql.DB, store *discussion.Store) (taskio.DecisionRaiser, error) {
	if db == nil || store == nil {
		return nil, errors.New("apiserver.NewTaskIODecisionRaise: db and discussion store are required")
	}
	raiser, err := decisionraise.New(mentiondispatch.NewProposalThreadResolver(db), store)
	if err != nil {
		return nil, err
	}
	return taskioDecisionRaiser{raiser: raiser}, nil
}

// taskioDecisionRaiser adapts *decisionraise.Raiser onto taskio.DecisionRaiser.
type taskioDecisionRaiser struct {
	raiser *decisionraise.Raiser
}

// RequestDecision converts the primitives-only task-io ask into the typed
// discussion raise and maps the errors back onto the task-io sentinels.
func (a taskioDecisionRaiser) RequestDecision(ctx context.Context, runID, principal, workItemID string, ask taskio.DecisionAsk) (taskio.DecisionRaiseOutcome, error) {
	msg, err := a.raiser.RequestDecision(ctx, runID, principal, workItemID, discussion.DecisionRaise{
		Title:       ask.Title,
		Details:     ask.Details,
		Options:     ask.Options,
		Mode:        ask.Mode,
		Recommended: ask.Recommended,
	})
	if err != nil {
		switch {
		case errors.Is(err, decisionraise.ErrNoRaisingThread):
			return taskio.DecisionRaiseOutcome{}, taskio.ErrNoRaisingThread
		case errors.Is(err, discussion.ErrInvalidDecisionPayload):
			return taskio.DecisionRaiseOutcome{}, fmt.Errorf("%w: %v", taskio.ErrInvalidDecisionAsk, err)
		default:
			return taskio.DecisionRaiseOutcome{}, err
		}
	}
	return taskio.DecisionRaiseOutcome{
		MessageID: msg.ID.String(),
		ThreadID:  msg.ThreadID.String(),
		Phase:     "open",
	}, nil
}
