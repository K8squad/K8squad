package discussion

// ISI-5536 (ISI-5531 E2) — decision_request unit tests (no DB): the payload contract, the
// RejectAllowed polarity, and the post-back answer-line formatter. The DB-backed lifecycle
// (idempotent create, answer/reject/expire CAS single-winner, tenant-scoped idempotency, the
// supersede sweep, cross-team tenancy) is covered against real Postgres in decision_integration_test.go
// (build tag discussion_integration), NOT here — these unit tests never touch the store.

import (
	"errors"
	"fmt"
	"testing"
)

func TestDecisionRequestPayloadValidate(t *testing.T) {
	opt := DecisionOption{ID: "reqwest", Label: "reqwest"}
	opt2 := DecisionOption{ID: "hyper", Label: "hyper"}

	// 10 distinct options — one over the maxDecisionOptions cap.
	var tenOpts []DecisionOption
	for i := 0; i < 10; i++ {
		tenOpts = append(tenOpts, DecisionOption{ID: fmt.Sprintf("o%d", i), Label: fmt.Sprintf("Option %d", i)})
	}

	cases := []struct {
		name    string
		payload DecisionRequestPayload
		wantErr error // nil ⇒ must pass
	}{
		// approve / free_form are opt-outs of the >=2 rule — no options required.
		{"approve ok", DecisionRequestPayload{Mode: DecisionModeApprove, Title: "ship it?"}, nil},
		{"free_form ok", DecisionRequestPayload{Mode: DecisionModeFreeForm, Title: "describe the bug"}, nil},
		// choose modes require >=2 options.
		{"choose_one ok 2 options", DecisionRequestPayload{Mode: DecisionModeChooseOne, Title: "client?", Options: []DecisionOption{opt, opt2}}, nil},
		{"choose_many ok 2 options", DecisionRequestPayload{Mode: DecisionModeChooseMany, Title: "pick some", Options: []DecisionOption{opt, opt2}}, nil},
		// 1-option payloads must be rejected.
		{"choose_one 1 option rejected", DecisionRequestPayload{Mode: DecisionModeChooseOne, Title: "client?", Options: []DecisionOption{opt}}, ErrInvalidDecisionPayload},
		{"choose_many 1 option rejected", DecisionRequestPayload{Mode: DecisionModeChooseMany, Title: "pick", Options: []DecisionOption{opt}}, ErrInvalidDecisionPayload},
		// Hard cap: 10 options must be rejected (FE number-key contract, keys 1–9 only).
		{"choose_one over cap rejected", DecisionRequestPayload{Mode: DecisionModeChooseOne, Title: "x", Options: tenOpts}, ErrInvalidDecisionPayload},
		{"choose_many over cap rejected", DecisionRequestPayload{Mode: DecisionModeChooseMany, Title: "x", Options: tenOpts}, ErrInvalidDecisionPayload},
		{"missing title", DecisionRequestPayload{Mode: DecisionModeApprove}, ErrInvalidDecisionPayload},
		{"unknown mode", DecisionRequestPayload{Mode: "self_destruct", Title: "x"}, ErrInvalidDecisionPayload},
		{"choose_one no options", DecisionRequestPayload{Mode: DecisionModeChooseOne, Title: "client?"}, ErrInvalidDecisionPayload},
		{"choose_many no options", DecisionRequestPayload{Mode: DecisionModeChooseMany, Title: "pick"}, ErrInvalidDecisionPayload},
		{"option needs id+label", DecisionRequestPayload{Mode: DecisionModeChooseOne, Title: "x", Options: []DecisionOption{{Label: "no id"}, opt2}}, ErrInvalidDecisionPayload},
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

func TestDecisionRejectAllowedPolarity(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name string
		p    DecisionRequestPayload
		want bool
	}{
		// The wire/omitempty default (nil) must PERMIT rejection — the FE shows Reject by default, so a
		// nil-default-deny silently 400'd it (M1).
		{"choose_one default (nil) allows", DecisionRequestPayload{Mode: DecisionModeChooseOne}, true},
		{"choose_many default (nil) allows", DecisionRequestPayload{Mode: DecisionModeChooseMany}, true},
		{"free_form default (nil) allows", DecisionRequestPayload{Mode: DecisionModeFreeForm}, true},
		{"explicit true allows", DecisionRequestPayload{Mode: DecisionModeChooseOne, AllowReject: &yes}, true},
		{"explicit false denies", DecisionRequestPayload{Mode: DecisionModeChooseOne, AllowReject: &no}, false},
		// approve is accept/reject by construction — always rejectable, even if someone sets false.
		{"approve always allows (nil)", DecisionRequestPayload{Mode: DecisionModeApprove}, true},
		{"approve always allows (explicit false)", DecisionRequestPayload{Mode: DecisionModeApprove, AllowReject: &no}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.RejectAllowed(); got != tc.want {
				t.Fatalf("RejectAllowed() = %v, want %v", got, tc.want)
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
