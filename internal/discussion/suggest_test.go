package discussion

// ISI-7218 (ISI-7075 C4) — suggest_next_steps unit tests (no DB): the
// kind-specific payload contract layered on the shared option grammar, and the
// typed ask builder. The DB-backed lifecycle rides the decisionraise integration
// lane (internal/decisionraise/suggestraise_integration_test.go, build tag
// decision_integration) — these unit tests never touch the store.

import (
	"errors"
	"testing"
)

func TestValidateSuggestNextSteps(t *testing.T) {
	opt := DecisionOption{ID: "a", Label: "a) Create child for quota bump"}
	opt2 := DecisionOption{ID: "b", Label: "b) Reassign training to CTO"}
	rec1 := DecisionOption{ID: "a", Label: "a", Recommended: true}
	rec2 := DecisionOption{ID: "b", Label: "b", Recommended: true}

	cases := []struct {
		name    string
		payload DecisionRequestPayload
		wantErr error // nil ⇒ must pass
	}{
		{"choose_one ok", DecisionRequestPayload{Mode: DecisionModeChooseOne, Title: "Blocked on X — next steps?", Options: []DecisionOption{opt, opt2}}, nil},
		{"choose_many ok", DecisionRequestPayload{Mode: DecisionModeChooseMany, Title: "Blocked on X", Options: []DecisionOption{opt, opt2}}, nil},
		{"free_form ok (no-path fallback)", DecisionRequestPayload{Mode: DecisionModeFreeForm, Title: "No visible path", DetailsMarkdown: "why"}, nil},
		{"free_form with options rejected", DecisionRequestPayload{Mode: DecisionModeFreeForm, Title: "x", Options: []DecisionOption{opt, opt2}}, ErrInvalidDecisionPayload},
		{"approve rejected (not a suggestion mode)", DecisionRequestPayload{Mode: DecisionModeApprove, Title: "x"}, ErrInvalidDecisionPayload},
		{"missing title", DecisionRequestPayload{Mode: DecisionModeChooseOne}, ErrInvalidDecisionPayload},
		{"unknown mode", DecisionRequestPayload{Mode: "self_destruct", Title: "x"}, ErrInvalidDecisionPayload},
		{"one option rejected (shared >=2 floor)", DecisionRequestPayload{Mode: DecisionModeChooseOne, Title: "x", Options: []DecisionOption{opt}}, ErrInvalidDecisionPayload},
		{"duplicate id rejected (shared rule)", DecisionRequestPayload{Mode: DecisionModeChooseOne, Title: "x", Options: []DecisionOption{opt, opt}}, ErrInvalidDecisionPayload},
		{"two recommended rejected on choose_one (§5 at-most-one)", DecisionRequestPayload{Mode: DecisionModeChooseOne, Title: "x", Options: []DecisionOption{rec1, rec2}}, ErrInvalidDecisionPayload},
		{"two recommended rejected on choose_many (§5 at-most-one)", DecisionRequestPayload{Mode: DecisionModeChooseMany, Title: "x", Options: []DecisionOption{rec1, rec2}}, ErrInvalidDecisionPayload},
		{"one recommended ok", DecisionRequestPayload{Mode: DecisionModeChooseOne, Title: "x", Options: []DecisionOption{rec1, opt2}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSuggestNextSteps(tc.payload)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("ValidateSuggestNextSteps: unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ValidateSuggestNextSteps error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestBuildSuggestPayload(t *testing.T) {
	p, err := (SuggestNextStepsRaise{
		Title:   "  Blocked: no GPU quota for training  ",
		Details: " squad parked since Monday ",
		Steps: []SuggestedStep{
			{Label: "Create child for the quota bump", Description: "implementer owns it; unblocks training", Recommended: true},
			{Label: "Reassign evaluation to the reviewer"},
		},
	}).BuildSuggestPayload()
	if err != nil {
		t.Fatalf("BuildSuggestPayload: %v", err)
	}
	if p.Version != 1 || p.Mode != DecisionModeChooseOne {
		t.Fatalf("version/mode = %d/%q", p.Version, p.Mode)
	}
	if p.Title != "Blocked: no GPU quota for training" || p.DetailsMarkdown != "squad parked since Monday" {
		t.Fatalf("title/details not trimmed: %q / %q", p.Title, p.DetailsMarkdown)
	}
	if len(p.Options) != 2 {
		t.Fatalf("options = %d, want 2", len(p.Options))
	}
	if p.Options[0].ID != "a" || p.Options[0].Label != "a) Create child for the quota bump" {
		t.Fatalf("first option not auto-lettered: %+v", p.Options[0])
	}
	if !p.Options[0].Recommended || p.Options[1].Recommended {
		t.Fatalf("recommended flags = %v/%v", p.Options[0].Recommended, p.Options[1].Recommended)
	}
	if len(p.DefaultSelectedOptionIDs) != 1 || p.DefaultSelectedOptionIDs[0] != "a" {
		t.Fatalf("defaultSelected = %v, want [a]", p.DefaultSelectedOptionIDs)
	}
	if p.Options[1].Description != "" {
		t.Fatalf("description should be optional, got %q", p.Options[1].Description)
	}
}

func TestBuildSuggestPayloadAllowMany(t *testing.T) {
	p, err := (SuggestNextStepsRaise{
		Title:     "Blocked on two fronts",
		Steps:     []SuggestedStep{{Label: "A"}, {Label: "B"}},
		AllowMany: true,
	}).BuildSuggestPayload()
	if err != nil {
		t.Fatalf("BuildSuggestPayload: %v", err)
	}
	if p.Mode != DecisionModeChooseMany {
		t.Fatalf("mode = %q, want choose_many", p.Mode)
	}
}

func TestBuildSuggestPayloadNoPath(t *testing.T) {
	p, err := (SuggestNextStepsRaise{
		Title:   "Stuck: no visibility at all",
		Details: "every probe returns empty — cannot even name a candidate fix",
		NoPath:  true,
	}).BuildSuggestPayload()
	if err != nil {
		t.Fatalf("BuildSuggestPayload: %v", err)
	}
	if p.Mode != DecisionModeFreeForm || len(p.Options) != 0 {
		t.Fatalf("noPath payload = mode %q with %d options, want free_form with none", p.Mode, len(p.Options))
	}
	if p.DetailsMarkdown == "" {
		t.Fatalf("noPath payload lost its explanation")
	}

	if _, err := (SuggestNextStepsRaise{Title: "x", NoPath: true}).BuildSuggestPayload(); !errors.Is(err, ErrInvalidDecisionPayload) {
		t.Fatalf("noPath without details error = %v, want ErrInvalidDecisionPayload", err)
	}
	if _, err := (SuggestNextStepsRaise{Title: "x", NoPath: true, Steps: []SuggestedStep{{Label: "A"}, {Label: "B"}}}).BuildSuggestPayload(); !errors.Is(err, ErrInvalidDecisionPayload) {
		t.Fatalf("noPath with steps error = %v, want ErrInvalidDecisionPayload", err)
	}
}

func TestBuildSuggestPayloadFloors(t *testing.T) {
	// Taxonomy §5 rule 1: blocked with zero suggested next steps is refused.
	if _, err := (SuggestNextStepsRaise{Title: "Blocked", Steps: []SuggestedStep{{Label: "only one"}}}).BuildSuggestPayload(); !errors.Is(err, ErrInvalidDecisionPayload) {
		t.Fatalf("1-step error = %v, want ErrInvalidDecisionPayload", err)
	}
	if _, err := (SuggestNextStepsRaise{Title: "Blocked"}).BuildSuggestPayload(); !errors.Is(err, ErrInvalidDecisionPayload) {
		t.Fatalf("0-step error = %v, want ErrInvalidDecisionPayload", err)
	}
	// Hard cap: 10 steps exceed the 9-option keyboard contract.
	var many []SuggestedStep
	for i := 0; i < 10; i++ {
		many = append(many, SuggestedStep{Label: string(rune('A' + i))})
	}
	if _, err := (SuggestNextStepsRaise{Title: "Blocked", Steps: many}).BuildSuggestPayload(); !errors.Is(err, ErrInvalidDecisionPayload) {
		t.Fatalf("10-step error = %v, want ErrInvalidDecisionPayload", err)
	}
	// Missing title.
	if _, err := (SuggestNextStepsRaise{Steps: []SuggestedStep{{Label: "A"}, {Label: "B"}}}).BuildSuggestPayload(); !errors.Is(err, ErrInvalidDecisionPayload) {
		t.Fatalf("missing title error = %v, want ErrInvalidDecisionPayload", err)
	}
	// At most one recommended — refused at the builder's own gate.
	if _, err := (SuggestNextStepsRaise{Title: "Blocked", Steps: []SuggestedStep{{Label: "A", Recommended: true}, {Label: "B", Recommended: true}}}).BuildSuggestPayload(); !errors.Is(err, ErrInvalidDecisionPayload) {
		t.Fatalf("two-recommended error = %v, want ErrInvalidDecisionPayload", err)
	}
}
