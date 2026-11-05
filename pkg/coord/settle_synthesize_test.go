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
	body := renderSynthesizedSummary("Run failed (retry budget exhausted)", "", "failed", "", nil, nil)
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
