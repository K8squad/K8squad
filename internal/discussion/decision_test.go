package discussion

// ISI-5536 (ISI-5531 E2) — decision_request unit tests (no DB): the payload contract and the
// post-back answer-line formatter. The DB-backed lifecycle (idempotent create, answer/reject/expire
// CAS, supersede sweep, tenancy) runs in the integration lane (see proposal_integration_test.go).

import (
	"errors"
	"testing"
)

func TestDecisionRequestPayloadValidate(t *testing.T) {
	opt := DecisionOption{ID: "reqwest", Label: "reqwest"}
	cases := []struct {
		name    string
		payload DecisionRequestPayload
		wantErr error // nil ⇒ must pass
	}{
		{"approve ok", DecisionRequestPayload{Mode: DecisionModeApprove, Title: "ship it?"}, nil},
		{"free_form ok", DecisionRequestPayload{Mode: DecisionModeFreeForm, Title: "describe the bug"}, nil},
		{"choose_one ok", DecisionRequestPayload{Mode: DecisionModeChooseOne, Title: "client?", Options: []DecisionOption{opt}}, nil},
		{"choose_many ok", DecisionRequestPayload{Mode: DecisionModeChooseMany, Title: "pick some", Options: []DecisionOption{opt}}, nil},
		{"missing title", DecisionRequestPayload{Mode: DecisionModeApprove}, ErrInvalidDecisionPayload},
		{"unknown mode", DecisionRequestPayload{Mode: "self_destruct", Title: "x"}, ErrInvalidDecisionPayload},
		{"choose_one needs options", DecisionRequestPayload{Mode: DecisionModeChooseOne, Title: "client?"}, ErrInvalidDecisionPayload},
		{"choose_many needs options", DecisionRequestPayload{Mode: DecisionModeChooseMany, Title: "pick"}, ErrInvalidDecisionPayload},
		{"option needs id+label", DecisionRequestPayload{Mode: DecisionModeChooseOne, Title: "x", Options: []DecisionOption{{Label: "no id"}}}, ErrInvalidDecisionPayload},
		{"duplicate option id", DecisionRequestPayload{Mode: DecisionModeChooseOne, Title: "x", Options: []DecisionOption{opt, opt}}, ErrInvalidDecisionPayload},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.payload.Validate()
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("want %v, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestDecisionAnswerLine(t *testing.T) {
	free := "use fasthttp"
	cases := []struct {
		name string
		ans  DecisionAnswer
		want string
	}{
		{"single option", DecisionAnswer{SelectedOptionIDs: []string{"reqwest"}}, "reqwest"},
		{"many options", DecisionAnswer{SelectedOptionIDs: []string{"a", "b"}}, "a, b"},
		{"free text", DecisionAnswer{FreeText: &free}, "use fasthttp"},
		{"empty", DecisionAnswer{}, "(no selection)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decisionAnswerLine(tc.ans); got != tc.want {
				t.Fatalf("want %q, got %q", tc.want, got)
			}
		})
	}
}
