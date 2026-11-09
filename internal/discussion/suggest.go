package discussion

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// suggest.go — ISI-7218 (ISI-7075 C4, docs/board-interaction-taxonomy.md §5): the
// suggest_next_steps interaction kind. A coordinator reporting blocked/stuck must
// propose concrete next actions (tickets to create, who to assign, what to unblock)
// instead of just stating status — "blocked with zero suggested next steps" is a
// contract violation of the OUTPUT contract this kind enforces.
//
// The card is an ordinary kind='suggest_next_steps' discussion message carrying the
// SAME DecisionRequestPayload wire shape as a decision_request (one option grammar,
// two kinds — the taxonomy's whole point). It rides the SAME discussion.decision_request
// lifecycle side table (0031): one state machine (open → answered | rejected | expired |
// superseded), one answer/reject CAS, one resume_agent_on_answer continuation, one
// Inbox arm, one DecisionCard renderer. The message kind is the only discriminator;
// no new table, no new custody semantics — the card is inert by construction exactly
// like its decision sibling (asking is conversation; ANSWERING stays human-only).
//
// Kind-specific contract (taxonomy §5), enforced HERE as the server-side gate in the
// prompt → helper → server defense-in-depth chain:
//   - modes: choose_one | choose_many | free_form. approve is NOT a suggestion mode
//     (a confirmation is not a set of next actions); free_form is the explicit no-path
//     fallback ("if the coordinator genuinely sees no path, the card must say so in
//     detailsMarkdown and offer free_form").
//   - choose_*: the shared option rules (>=2 floor, cap 9, unique ids, id+label) plus
//     at most ONE recommended step (§5: "the coordinator's pick, at most one").
//   - options describe actions to take, not questions to answer — that half of the
//     contract is authored in the coordinator prompt (pkg/roleprompt/coordinator_prompt.md);
//     the server validates shape only.

// KindSuggestNextSteps marks a suggest-next-steps message (rendered by the SAME
// DecisionCard as a decision_request — taxonomy §4: single renderer, no bespoke
// component).
const KindSuggestNextSteps = "suggest_next_steps"

// ValidateSuggestNextSteps enforces the kind-specific contract on the shared payload
// shape before anything is persisted (the server-side gate; the runtime helper
// BuildSuggestPayload refuses early, this refuses always).
func ValidateSuggestNextSteps(p DecisionRequestPayload) error {
	if p.Title == "" {
		return fmt.Errorf("%w: a suggest_next_steps card requires a title", ErrInvalidDecisionPayload)
	}
	switch p.Mode {
	case DecisionModeFreeForm:
		// The no-path fallback (taxonomy §5 rule 1): free_form carries no options by
		// construction — a free_form card WITH options is a choose card wearing a costume.
		if len(p.Options) > 0 {
			return fmt.Errorf("%w: a free_form suggest_next_steps card carries no options (got %d) — use choose_one/choose_many to propose steps", ErrInvalidDecisionPayload, len(p.Options))
		}
	case DecisionModeChooseOne, DecisionModeChooseMany:
		// The shared card rules (>=2 floor, cap 9, unique ids, id+label) ride the
		// decision_request Validate — ONE implementation of the option grammar.
		if err := p.Validate(); err != nil {
			return err
		}
		// Taxonomy §5: "recommended — the coordinator's pick, at most one."
		rec := 0
		for _, o := range p.Options {
			if o.Recommended {
				rec++
			}
		}
		if rec > 1 {
			return fmt.Errorf("%w: a suggest_next_steps card carries at most one recommended step (got %d)", ErrInvalidDecisionPayload, rec)
		}
	default:
		return fmt.Errorf("%w: mode %q is not a suggest_next_steps mode (choose_one|choose_many|free_form)", ErrInvalidDecisionPayload, p.Mode)
	}
	return nil
}

// PostSuggestNextSteps appends an inert kind='suggest_next_steps' message and its
// phase='open' lifecycle row in one transaction — the suggest twin of
// PostDecisionRequest. Same idempotent create (a retry re-posting the same key
// returns the EXISTING card, no second row), same tenancy scoping, same answer/
// reject CAS and resume continuation; only the kind stamp and the validation gate
// differ. A suggestion card binds no plan revision — no target, no supersede sweep.
func (s *Store) PostSuggestNextSteps(ctx context.Context, projectID string, teamID, threadID uuid.UUID, auth AuthorContext, body string, payload DecisionRequestPayload, workItemID, idempotencyKey string, parentID *uuid.UUID) (*Message, error) {
	if payload.Version == 0 {
		payload.Version = 1
	}
	if err := ValidateSuggestNextSteps(payload); err != nil {
		return nil, err
	}
	return s.postDecisionCard(ctx, projectID, teamID, threadID, auth, body, payload, nil, workItemID, idempotencyKey, parentID, KindSuggestNextSteps, func() error { return ValidateSuggestNextSteps(payload) })
}
