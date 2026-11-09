package discussion

import (
	"errors"
	"testing"
)

// decisionraise_test.go — ISI-7224 (WS-6.2): the shared helper's single
// validation point. Every case here is the contract BOTH emission paths
// (task-io verb, host adapter) inherit unchanged — there is no second
// validator anywhere else in the tree.

func TestBuildPayloadAutoLettersOptions(t *testing.T) {
	p, err := (DecisionRaise{
		Title:       "Ship which rollout strategy?",
		Details:     " context for the board",
		Options:     []string{"Canary 10%", "Full rollout", "Hold for Friday"},
		Mode:        "choose_one",
		Recommended: 2,
	}).BuildPayload()
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	if p.Version != 1 || p.Mode != DecisionModeChooseOne {
		t.Fatalf("version/mode = %d/%q", p.Version, p.Mode)
	}
	if len(p.Options) != 3 {
		t.Fatalf("options = %d, want 3", len(p.Options))
	}
	wantIDs := []string{"a", "b", "c"}
	wantLabels := []string{"a) Canary 10%", "b) Full rollout", "c) Hold for Friday"}
	for i, o := range p.Options {
		if o.ID != wantIDs[i] || o.Label != wantLabels[i] {
			t.Fatalf("option %d = %+v, want id %q label %q", i, o, wantIDs[i], wantLabels[i])
		}
	}
	if !p.Options[1].Recommended || p.Options[0].Recommended || p.Options[2].Recommended {
		t.Fatalf("recommended flag must land on option 2 only: %+v", p.Options)
	}
	if len(p.DefaultSelectedOptionIDs) != 1 || p.DefaultSelectedOptionIDs[0] != "b" {
		t.Fatalf("defaultSelected = %v, want [b]", p.DefaultSelectedOptionIDs)
	}
	if p.DetailsMarkdown != "context for the board" {
		t.Fatalf("details = %q", p.DetailsMarkdown)
	}
	// The built payload must pass the store's own gate unchanged — the helper
	// and the server contract (WS-6.1) agree by construction.
	if err := p.Validate(); err != nil {
		t.Fatalf("built payload failed store Validate: %v", err)
	}
}

func TestBuildPayloadDefaultsModeToChooseOne(t *testing.T) {
	p, err := (DecisionRaise{Title: "T", Options: []string{"a", "b"}}).BuildPayload()
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	if p.Mode != DecisionModeChooseOne {
		t.Fatalf("mode = %q, want choose_one default", p.Mode)
	}
}

func TestBuildPayloadCapAndFloor(t *testing.T) {
	cases := []struct {
		name    string
		options []string
		wantErr bool
	}{
		{"zero options", nil, true},
		{"single option", []string{"only"}, true},
		{"two options", []string{"a", "b"}, false},
		{"five options", []string{"1", "2", "3", "4", "5"}, false},
		{"nine options", []string{"1", "2", "3", "4", "5", "6", "7", "8", "9"}, false},
		{"ten options", []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10"}, true},
	}
	for _, tc := range cases {
		_, err := (DecisionRaise{Title: "T", Options: tc.options}).BuildPayload()
		if tc.wantErr && !errors.Is(err, ErrInvalidDecisionPayload) {
			t.Fatalf("%s: err = %v, want ErrInvalidDecisionPayload", tc.name, err)
		}
		if !tc.wantErr && err != nil {
			t.Fatalf("%s: unexpected err %v", tc.name, err)
		}
	}
	// The 9-option ceiling letters through i.
	p, err := (DecisionRaise{Title: "T", Options: []string{"1", "2", "3", "4", "5", "6", "7", "8", "9"}}).BuildPayload()
	if err != nil {
		t.Fatalf("nine options: %v", err)
	}
	if last := p.Options[8]; last.ID != "i" || last.Label != "i) 9" {
		t.Fatalf("last option = %+v, want id i label \"i) 9\"", last)
	}
}

func TestBuildPayloadOptOutModesIgnoreOptions(t *testing.T) {
	for _, mode := range []string{DecisionModeApprove, DecisionModeFreeForm} {
		p, err := (DecisionRaise{Title: "T", Mode: mode, Options: []string{"ignored"}}).BuildPayload()
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if len(p.Options) != 0 {
			t.Fatalf("%s: options must be ignored, got %+v", mode, p.Options)
		}
	}
}

func TestBuildPayloadRejections(t *testing.T) {
	cases := []struct {
		name string
		ask  DecisionRaise
	}{
		{"empty title", DecisionRaise{Options: []string{"a", "b"}}},
		{"unknown mode", DecisionRaise{Title: "T", Mode: "coin_flip", Options: []string{"a", "b"}}},
		{"empty option label", DecisionRaise{Title: "T", Options: []string{"a", "  "}}},
		{"recommended out of range", DecisionRaise{Title: "T", Options: []string{"a", "b"}, Recommended: 3}},
		{"negative recommended", DecisionRaise{Title: "T", Options: []string{"a", "b"}, Recommended: -1}},
	}
	for _, tc := range cases {
		_, err := tc.ask.BuildPayload()
		if !errors.Is(err, ErrInvalidDecisionPayload) {
			t.Fatalf("%s: err = %v, want ErrInvalidDecisionPayload", tc.name, err)
		}
	}
}
