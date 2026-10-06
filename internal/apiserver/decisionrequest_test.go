package apiserver

import (
	"errors"
	"testing"

	"github.com/K8squad/K8squad/internal/discussion"
)

// ISI-5536 (ADR-0026 §4.1) — buildDecisionAnswer is the handler-side gate that validates a human's
// selection against the stored card's mode/options before the CAS. These prove each mode's contract
// and that every rejection maps to ErrInvalidDecisionPayload (→400).

func strptr(s string) *string { return &s }

func optPayload(mode string, opts ...string) discussion.DecisionRequestPayload {
	p := discussion.DecisionRequestPayload{Version: 1, Mode: mode, Title: "t"}
	for _, id := range opts {
		p.Options = append(p.Options, discussion.DecisionOption{ID: id, Label: id})
	}
	return p
}

func TestBuildDecisionAnswer_ChooseOne(t *testing.T) {
	p := optPayload(discussion.DecisionModeChooseOne, "reqwest", "hyper")

	// exactly one valid option — OK
	ans, err := buildDecisionAnswer(p, []string{"reqwest"}, nil)
	if err != nil {
		t.Fatalf("valid choose_one rejected: %v", err)
	}
	if len(ans.SelectedOptionIDs) != 1 || ans.SelectedOptionIDs[0] != "reqwest" {
		t.Fatalf("selection not carried: %+v", ans.SelectedOptionIDs)
	}

	// zero options and no free-text allowed — 400
	if _, err := buildDecisionAnswer(p, nil, nil); !errors.Is(err, discussion.ErrInvalidDecisionPayload) {
		t.Fatalf("empty choose_one should be invalid, got %v", err)
	}
	// two selected — 400
	if _, err := buildDecisionAnswer(p, []string{"reqwest", "hyper"}, nil); !errors.Is(err, discussion.ErrInvalidDecisionPayload) {
		t.Fatalf("multi-select choose_one should be invalid, got %v", err)
	}
	// unknown option id — 400
	if _, err := buildDecisionAnswer(p, []string{"ghost"}, nil); !errors.Is(err, discussion.ErrInvalidDecisionPayload) {
		t.Fatalf("unknown option should be invalid, got %v", err)
	}
}

func TestBuildDecisionAnswer_ChooseOneFreeTextEscape(t *testing.T) {
	p := optPayload(discussion.DecisionModeChooseOne, "reqwest")
	p.AllowFreeText = true
	// the "something else" path: no option, free-text supplied — OK
	if _, err := buildDecisionAnswer(p, nil, strptr("roll our own")); err != nil {
		t.Fatalf("free-text escape rejected: %v", err)
	}
	// free-text when the card forbids it — 400
	q := optPayload(discussion.DecisionModeChooseOne, "reqwest")
	if _, err := buildDecisionAnswer(q, []string{"reqwest"}, strptr("sneaky")); !errors.Is(err, discussion.ErrInvalidDecisionPayload) {
		t.Fatalf("free-text on a no-free-text card should be invalid, got %v", err)
	}
}

func TestBuildDecisionAnswer_ChooseMany(t *testing.T) {
	p := optPayload(discussion.DecisionModeChooseMany, "a", "b", "c")
	p.MinSelected = 2
	p.MaxSelected = 3

	if _, err := buildDecisionAnswer(p, []string{"a", "b"}, nil); err != nil {
		t.Fatalf("valid choose_many rejected: %v", err)
	}
	// below min — 400
	if _, err := buildDecisionAnswer(p, []string{"a"}, nil); !errors.Is(err, discussion.ErrInvalidDecisionPayload) {
		t.Fatalf("below-min choose_many should be invalid, got %v", err)
	}
	// above max — 400
	q := optPayload(discussion.DecisionModeChooseMany, "a", "b", "c")
	q.MaxSelected = 1
	if _, err := buildDecisionAnswer(q, []string{"a", "b"}, nil); !errors.Is(err, discussion.ErrInvalidDecisionPayload) {
		t.Fatalf("above-max choose_many should be invalid, got %v", err)
	}
	// duplicate selection — 400
	if _, err := buildDecisionAnswer(p, []string{"a", "a"}, nil); !errors.Is(err, discussion.ErrInvalidDecisionPayload) {
		t.Fatalf("duplicate selection should be invalid, got %v", err)
	}
}

func TestBuildDecisionAnswer_FreeForm(t *testing.T) {
	p := discussion.DecisionRequestPayload{Version: 1, Mode: discussion.DecisionModeFreeForm, Title: "t"}
	if _, err := buildDecisionAnswer(p, nil, strptr("here is my answer")); err != nil {
		t.Fatalf("valid free_form rejected: %v", err)
	}
	if _, err := buildDecisionAnswer(p, nil, nil); !errors.Is(err, discussion.ErrInvalidDecisionPayload) {
		t.Fatalf("empty free_form should be invalid, got %v", err)
	}
}

func TestBuildDecisionAnswer_Approve(t *testing.T) {
	p := discussion.DecisionRequestPayload{Version: 1, Mode: discussion.DecisionModeApprove, Title: "ship it?"}
	ans, err := buildDecisionAnswer(p, []string{"ignored"}, nil)
	if err != nil {
		t.Fatalf("approve rejected: %v", err)
	}
	if len(ans.SelectedOptionIDs) != 0 {
		t.Fatalf("approve should carry no option selection, got %+v", ans.SelectedOptionIDs)
	}
}

func TestBuildDecisionAnswer_UnknownMode(t *testing.T) {
	p := discussion.DecisionRequestPayload{Version: 1, Mode: "telepathy", Title: "t"}
	if _, err := buildDecisionAnswer(p, nil, nil); !errors.Is(err, discussion.ErrInvalidDecisionPayload) {
		t.Fatalf("unknown mode should be invalid, got %v", err)
	}
}
