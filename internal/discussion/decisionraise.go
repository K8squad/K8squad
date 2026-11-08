package discussion

import (
	"fmt"
	"strings"
)

// decisionraise.go — ISI-7224 (ISI-7074 WS-6.2, plan ISI-7204 rev 1): the typed
// runtime-facing decision ask. Before this, kind='decision_request' had zero
// non-test emitters outside internal/discussion — the inbox DecisionCard surface
// (ISI-5536 BE-7) was starved: nothing an agent runs ever RAISED a card.
//
// DecisionRaise is the ONE shape a runtime fills in (title, details, option
// labels, mode, recommended pick) and BuildPayload is the SINGLE validation +
// normalization point every emission path funnels through (plan default O2:
// shared helper, single validation point — never one validator per runtime).
// It returns a DecisionRequestPayload so the store's own Validate stays the
// authoritative server-side gate (WS-6.1 enforces the same floor there): the
// helper refuses early, the store refuses always.
//
// Option auto-lettering: labels are lettered a/b/c/... in the order given
// (id "a", rendered label "a) Ship it") — the a/b/c/d board contract the parent
// issue (ISI-7074) asks for. A caller passes plain labels and cannot get the
// lettering wrong or collide ids.
const (
	// DecisionRaiseOptionsMin is the >=2 floor of the board-decision contract
	// (plan O1/O2): an ask the board must decide is ALWAYS a choice among
	// multiple labeled options — a single option is a confirmation (use
	// approve), an open ask is free_form.
	DecisionRaiseOptionsMin = 2
	// DecisionRaiseOptionsSoftMax is the top of the helper's recommended band
	// (WS-6.2 Options[2..5]): five lettered options is the readable ceiling for
	// a human decision; more stay allowed up to the hard cap, and the error
	// text nudges callers back into the band.
	DecisionRaiseOptionsSoftMax = 5
	// DecisionRaiseOptionsMax is the hard cap (plan O3): nine lettered options
	// (a..i), mirroring the inbox card's one-key selection contract — a tenth
	// option would be unselectable by construction.
	DecisionRaiseOptionsMax = 9
)

// DecisionRaise is the typed ask a runtime raises when it needs a board
// decision: plain option labels in board order, auto-lettered a/b/c/... by
// BuildPayload. Mode defaults to choose_one when empty; Recommended is the
// 1-based option number (0 = no recommendation).
type DecisionRaise struct {
	Title       string
	Details     string
	Options     []string
	Mode        string
	Recommended int
}

// BuildPayload validates the ask and normalizes it into the wire payload — the
// single emission-side validation point (plan O2). choose_one/choose_many MUST
// carry DecisionRaiseOptionsMin..DecisionRaiseOptionsMax non-empty option
// labels; approve and free_form are the explicit opt-outs and carry no options
// (any passed are ignored, mirroring the server-side contract).
func (r DecisionRaise) BuildPayload() (DecisionRequestPayload, error) {
	title := strings.TrimSpace(r.Title)
	if title == "" {
		return DecisionRequestPayload{}, fmt.Errorf("%w: a decision ask requires a title", ErrInvalidDecisionPayload)
	}
	mode := r.Mode
	if mode == "" {
		mode = DecisionModeChooseOne
	}
	switch mode {
	case DecisionModeApprove, DecisionModeFreeForm:
		// Explicit opt-outs of the options floor (plan O1): approve IS the
		// binary (accept/reject), free_form IS the open answer — options are
		// not the decision for either mode.
		r.Options = nil
		r.Recommended = 0
	case DecisionModeChooseOne, DecisionModeChooseMany:
		if len(r.Options) < DecisionRaiseOptionsMin {
			return DecisionRequestPayload{}, fmt.Errorf("%w: a decision ask needs at least %d options, got %d — a single option is a confirmation (use mode approve), an open ask is free_form", ErrInvalidDecisionPayload, DecisionRaiseOptionsMin, len(r.Options))
		}
		if len(r.Options) > DecisionRaiseOptionsMax {
			return DecisionRequestPayload{}, fmt.Errorf("%w: a decision ask permits at most %d lettered options (a..%c), got %d — the board contract recommends %d", ErrInvalidDecisionPayload, DecisionRaiseOptionsMax, rune('a'+DecisionRaiseOptionsMax-1), len(r.Options), DecisionRaiseOptionsSoftMax)
		}
		if r.Recommended < 0 || r.Recommended > len(r.Options) {
			return DecisionRequestPayload{}, fmt.Errorf("%w: recommended is the 1-based option number (0 = none), got %d against %d options", ErrInvalidDecisionPayload, r.Recommended, len(r.Options))
		}
	default:
		return DecisionRequestPayload{}, fmt.Errorf("%w: unknown decision mode %q (choose_one|choose_many|approve|free_form)", ErrInvalidDecisionPayload, mode)
	}
	payload := DecisionRequestPayload{
		Version:         1,
		Mode:            mode,
		Title:           title,
		DetailsMarkdown: strings.TrimSpace(r.Details),
	}
	var defaultSelected []string
	for i, raw := range r.Options {
		label := strings.TrimSpace(raw)
		if label == "" {
			return DecisionRequestPayload{}, fmt.Errorf("%w: option %d is empty — every option needs a label", ErrInvalidDecisionPayload, i+1)
		}
		id := string(rune('a' + i))
		payload.Options = append(payload.Options, DecisionOption{
			ID:          id,
			Label:       id + ") " + label,
			Description: label,
			Recommended: r.Recommended == i+1,
		})
		if r.Recommended == i+1 {
			defaultSelected = append(defaultSelected, id)
		}
	}
	payload.DefaultSelectedOptionIDs = defaultSelected
	return payload, nil
}
