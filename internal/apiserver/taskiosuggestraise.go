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

// taskiosuggestraise.go — ISI-7218 (ISI-7075 C4): the host adapter that binds
// the shared suggest-next-steps emission (internal/decisionraise — the single
// validation point, discussion.BuildSuggestPayload) onto the task-io seam. It
// composes the SAME two seams the decision raise rides (the dispatch-ledger
// thread resolver, ISI-5282, and the fenced discussion store create, ISI-5536)
// — zero new custody semantics — and translates the discussion package's
// sentinels onto the primitives-only taskio contract so pkg/taskio never
// imports the discussion schema.

// NewTaskIOSuggestRaise wires the task-io suggest-next-steps seam over the
// coord dispatch ledger (run → originating room thread) and the discussion
// store (the fenced, idempotent create). Either nil ⇒ error: the verb stays
// unwired (501), never half-mounted.
func NewTaskIOSuggestRaise(db *sql.DB, store *discussion.Store) (taskio.DecisionSuggester, error) {
	if db == nil || store == nil {
		return nil, errors.New("apiserver.NewTaskIOSuggestRaise: db and discussion store are required")
	}
	raiser, err := decisionraise.New(mentiondispatch.NewProposalThreadResolver(db), store)
	if err != nil {
		return nil, err
	}
	raiser = raiser.WithSuggestions(store)
	return taskioSuggester{raiser: raiser}, nil
}

// taskioSuggester adapts *decisionraise.Raiser onto taskio.DecisionSuggester.
type taskioSuggester struct {
	raiser *decisionraise.Raiser
}

// SuggestNextSteps converts the primitives-only task-io ask into the typed
// discussion raise and maps the errors back onto the task-io sentinels.
func (a taskioSuggester) SuggestNextSteps(ctx context.Context, runID, principal, workItemID string, ask taskio.SuggestAsk) (taskio.DecisionRaiseOutcome, error) {
	steps := make([]discussion.SuggestedStep, len(ask.Steps))
	for i, s := range ask.Steps {
		steps[i] = discussion.SuggestedStep{Label: s.Label, Description: s.Description, Recommended: s.Recommended}
	}
	msg, err := a.raiser.SuggestNextSteps(ctx, runID, principal, workItemID, discussion.SuggestNextStepsRaise{
		Title:     ask.Title,
		Details:   ask.Details,
		Steps:     steps,
		AllowMany: ask.AllowMany,
		NoPath:    ask.NoPath,
	})
	if err != nil {
		switch {
		case errors.Is(err, decisionraise.ErrNoRaisingThread):
			return taskio.DecisionRaiseOutcome{}, taskio.ErrNoRaisingThread
		case errors.Is(err, discussion.ErrInvalidDecisionPayload):
			return taskio.DecisionRaiseOutcome{}, fmt.Errorf("%w: %v", taskio.ErrInvalidSuggestAsk, err)
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
