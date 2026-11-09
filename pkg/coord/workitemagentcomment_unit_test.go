package coord

import "testing"

// workitemagentcomment_unit_test.go — pure-logic unit checks for the ISI-7220
// B3 agent-comment mention normalization and payload reads. Same discipline as
// workitemcomment_test.go: the SQL-level behaviour is proven by the wired store
// suites; these pin the lane-policy-free pure halves.

func TestNormalizeAgentMentions(t *testing.T) {
	// @-prefix tolerated (the body syntax is @name), whitespace trimmed,
	// empties dropped, dedupe first-seen, order preserved.
	got := normalizeAgentMentions([]string{" @alice ", "bob", "@alice", "", "  ", "@carol", "carol"})
	want := []string{"alice", "bob", "carol"}
	if len(got) != len(want) {
		t.Fatalf("normalizeAgentMentions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("normalizeAgentMentions[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
	// The mention set is capped — a comment is a note, not a broadcast.
	many := make([]string, 25)
	for i := range many {
		many[i] = "agent-" + string(rune('a'+i))
	}
	if n := len(normalizeAgentMentions(many)); n != agentMentionCap {
		t.Fatalf("capped mention set = %d, want %d", n, agentMentionCap)
	}
	// Empty / all-blank input yields nil (a plain, mention-free comment).
	for _, in := range [][]string{nil, {}, {"", "@", "   "}} {
		if got := normalizeAgentMentions(in); got != nil {
			t.Fatalf("normalizeAgentMentions(%v) = %v, want nil", in, got)
		}
	}
}

func TestMentionsFromPayload(t *testing.T) {
	// The mentions key reads off the same jsonb payload as references.
	payload := []byte(`{"mentions":["alice","bob"],"references":[{"workItemId":"x"}]}`)
	got := mentionsFromPayload(payload)
	if len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
		t.Fatalf("mentionsFromPayload = %v, want [alice bob]", got)
	}
	// A references-only payload (every ISI-5214 row) stays mention-free.
	if got := mentionsFromPayload([]byte(`{"references":[{"workItemId":"x"}]}`)); got != nil {
		t.Fatalf("references-only payload mentions = %v, want nil", got)
	}
	// NULL / empty / malformed payloads degrade to nil, exactly like
	// referencesFromPayload — a link-free comment stays link-free.
	for _, p := range [][]byte{nil, {}, []byte("not json")} {
		if got := mentionsFromPayload(p); got != nil {
			t.Fatalf("mentionsFromPayload(%q) = %v, want nil", p, got)
		}
	}
}
