package coord

import (
	"strings"
	"testing"
)

// The platform-synthesized summary (ISI-7094) must read human-first: the five
// template sections present, and NONE of the progress firehose tokens
// ([run/[tool/[status) leaking in — that is the AC "no [run/[tool/[status
// strings leak into summary/human comments".
func TestRenderSynthesizedSummary_NoProgressTokens(t *testing.T) {
	body := renderSynthesizedSummary(
		"Run succeeded", " by agent Winston", "succeeded",
		"931eece0-0000-0000-0000-000000000000",
		[]string{"- pull_request https://example/pr/7 — kind column"},
		[]string{"- log: s3://bucket/run.log"},
		nil,
	)

	for _, section := range []string{"What I did:", "What changed:", "Decisions and why:", "Next steps:", "Evidence:"} {
		if !strings.Contains(body, section) {
			t.Errorf("summary missing section %q:\n%s", section, body)
		}
	}
	for _, leak := range []string{"[run", "[tool", "[status", "[llm"} {
		if strings.Contains(body, leak) {
			t.Errorf("summary leaked progress token %q:\n%s", leak, body)
		}
	}
	if !strings.Contains(body, "https://example/pr/7") {
		t.Errorf("summary dropped the change ref:\n%s", body)
	}
}

// With no reported changes the section degrades to honest-empty wording, still
// token-clean.
func TestRenderSynthesizedSummary_Empty(t *testing.T) {
	body := renderSynthesizedSummary("Run failed (retry budget exhausted)", "", "failed", "", nil, nil, nil)
	if !strings.Contains(body, "No commits or pull requests were reported") {
		t.Errorf("empty-change summary missing honest-empty wording:\n%s", body)
	}
	if !strings.Contains(body, "run_id=(unknown)") {
		t.Errorf("missing-run summary should say run_id=(unknown):\n%s", body)
	}
	for _, leak := range []string{"[run", "[tool", "[status"} {
		if strings.Contains(body, leak) {
			t.Errorf("summary leaked %q:\n%s", leak, body)
		}
	}
}

// ISI-7772 (humane failure surface): a FAILED run never gets the hollow
// "the agent did not post one" template — the notice names who failed, the
// real cause class from durable evidence, and the source-aware next action.
func TestRenderSynthesizedSummary_FailureNotice(t *testing.T) {
	failure := newFailureNotice(
		runFailureEvidence{a2aOutcome: SettleOutcomeFailed, ran: true, lastFrom: "running"},
		"devon", "discussion")
	body := renderSynthesizedSummary("", "", "failed",
		"c2c6f212-0000-0000-0000-000000000000", nil, nil, failure)

	if !strings.Contains(body, "Run failure notice (auto-generated") {
		t.Errorf("failure notice missing its header:\n%s", body)
	}
	if strings.Contains(body, "did not post one") {
		t.Errorf("the hollow template must not fire on failed runs:\n%s", body)
	}
	for _, want := range []string{
		"devon's run failed",
		"the model route gave up",
		"This discussion reply is single-shot",
		"cause=model_route",
		"phases: running → failed",
		"a2a settle=failed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("failure notice missing %q:\n%s", want, body)
		}
	}
	for _, leak := range []string{"[run", "[tool", "[status"} {
		if strings.Contains(body, leak) {
			t.Errorf("failure notice leaked progress token %q:\n%s", leak, body)
		}
	}
}

// failureCause classifies the durable evidence (ISI-7772). The classes mirror
// what FailureCauseForWorkItem can attribute, plus the journal distinction.
func TestFailureCauseClassification(t *testing.T) {
	cases := []struct {
		name      string
		ev        runFailureEvidence
		wantClass string
		wantShort string
	}{
		{"settled failed lap", runFailureEvidence{a2aOutcome: SettleOutcomeFailed}, "model_route", "model route retry limit"},
		{"settled follow_error lap", runFailureEvidence{a2aOutcome: SettleOutcomeFollowError}, "follow_error", "follow stream error"},
		{"gone sandbox reason", runFailureEvidence{sandboxGone: "sandbox pod not found"}, "sandbox", "sandbox death (sandbox pod not found)"},
		{"ran then died unmarked", runFailureEvidence{ran: true}, "sandbox", "sandbox death mid-run"},
		{"never reached running", runFailureEvidence{}, "before_start", "died before start"},
	}
	for _, tc := range cases {
		class, short, sentence := failureCause(tc.ev)
		if class != tc.wantClass || short != tc.wantShort || sentence == "" {
			t.Errorf("%s: failureCause = (%q, %q, %q), want (%q, %q, non-empty)", tc.name, class, short, sentence, tc.wantClass, tc.wantShort)
		}
	}
}
