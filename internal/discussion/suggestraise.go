package discussion

import (
	"fmt"
	"strings"
)

// suggestraise.go — ISI-7218 (ISI-7075 C4): the typed runtime-facing
// suggest-next-steps ask, the twin of DecisionRaise (decisionraise.go). A
// coordinator that is blocked/stuck fills THIS shape in — what is blocked plus
// 2–5 concrete ACTIONS (create a child ticket, reassign, ask X to unblock Y) —
// and BuildSuggestPayload is the single validation + normalization point every
// emission path funnels through (the O2 discipline: the helper refuses early,
// the store's ValidateSuggestNextSteps refuses always).
//
// Option auto-lettering mirrors DecisionRaise: ids a/b/c/... in the order given,
// rendered label "a) Create ISI child for X" — ids are the join key the board's
// answer returns (selectedOptionIds), so a caller cannot collide them or get the
// lettering wrong.

// SuggestedStep is ONE proposed next action: an imperative action label plus the
// one-line deciding context (what it does, who owns it, what it unblocks —
// taxonomy §5) and the coordinator's pick flag (at most one across the card).
type SuggestedStep struct {
	Label       string
	Description string
	Recommended bool
}

// SuggestNextStepsRaise is the typed ask a coordinator raises when reporting
// blocked/stuck. NoPath is the explicit no-path fallback (taxonomy §5 rule 1):
// the card becomes free_form and Details MUST carry why no path is visible — the
// board fills the path. AllowMany selects choose_many (the board may pursue
// several next steps) over the choose_one default.
type SuggestNextStepsRaise struct {
	Title     string
	Details   string
	Steps     []SuggestedStep
	AllowMany bool
	NoPath    bool
}

// BuildSuggestPayload validates the ask and normalizes it into the shared wire
// payload (DecisionRequestPayload — one option grammar, two kinds).
func (r SuggestNextStepsRaise) BuildSuggestPayload() (DecisionRequestPayload, error) {
	title := strings.TrimSpace(r.Title)
	if title == "" {
		return DecisionRequestPayload{}, fmt.Errorf("%w: a suggest-next-steps ask requires a title", ErrInvalidDecisionPayload)
	}
	mode := DecisionModeChooseOne
	if r.AllowMany {
		mode = DecisionModeChooseMany
	}
	switch {
	case r.NoPath:
		// Taxonomy §5 rule 1: the no-path card says so in detailsMarkdown and offers
		// free_form — the board fills the path. Either steps or noPath, never both.
		if len(r.Steps) > 0 {
			return DecisionRequestPayload{}, fmt.Errorf("%w: noPath is the no-options fallback — pass either steps or noPath, not both", ErrInvalidDecisionPayload)
		}
		if strings.TrimSpace(r.Details) == "" {
			return DecisionRequestPayload{}, fmt.Errorf("%w: a noPath ask must explain in details why no next step is visible (taxonomy §5 rule 1)", ErrInvalidDecisionPayload)
		}
		mode = DecisionModeFreeForm
	case len(r.Steps) < DecisionRaiseOptionsMin:
		return DecisionRequestPayload{}, fmt.Errorf("%w: blocked with zero suggested next steps is a contract violation — propose at least %d concrete actions (got %d), or use the noPath fallback with an explanation", ErrInvalidDecisionPayload, DecisionRaiseOptionsMin, len(r.Steps))
	case len(r.Steps) > DecisionRaiseOptionsMax:
		return DecisionRequestPayload{}, fmt.Errorf("%w: a suggest-next-steps ask permits at most %d lettered options (a..%c), got %d — the board contract recommends %d", ErrInvalidDecisionPayload, DecisionRaiseOptionsMax, rune('a'+DecisionRaiseOptionsMax-1), len(r.Steps), DecisionRaiseOptionsSoftMax)
	}
	payload := DecisionRequestPayload{
		Version:         1,
		Mode:            mode,
		Title:           title,
		DetailsMarkdown: strings.TrimSpace(r.Details),
	}
	var defaultSelected []string
	for i, raw := range r.Steps {
		label := strings.TrimSpace(raw.Label)
		if label == "" {
			return DecisionRequestPayload{}, fmt.Errorf("%w: step %d has an empty label — every proposed action needs a label", ErrInvalidDecisionPayload, i+1)
		}
		id := string(rune('a' + i))
		payload.Options = append(payload.Options, DecisionOption{
			ID:          id,
			Label:       id + ") " + label,
			Description: strings.TrimSpace(raw.Description),
			Recommended: raw.Recommended,
		})
		if raw.Recommended {
			defaultSelected = append(defaultSelected, id)
		}
	}
	payload.DefaultSelectedOptionIDs = defaultSelected
	// The store gate re-validates (defense in depth) — refuse early here so the
	// kind-specific rules the builder owns surface at the emission edge.
	if err := ValidateSuggestNextSteps(payload); err != nil {
		return DecisionRequestPayload{}, err
	}
	return payload, nil
}
