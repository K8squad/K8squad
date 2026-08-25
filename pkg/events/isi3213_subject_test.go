package events

import "testing"

// DB-free unit tests for the NATS subject taxonomy (ISI-3213, child of ISI-2714).
// Subject/ParseSubject/sanitizeToken are pure routing logic with no Postgres
// dependency, so they run in the ci.yml unit lane.

func TestSubjectSanitizesMetacharacters(t *testing.T) {
	// Every NATS token separator/wildcard in a component must collapse to '_'
	// so a single component can never split the subject or widen into a wildcard.
	got := Subject("", "run", "proj.a", "sq d", "comp*>")
	want := "ksquad.run.proj_a.sq_d.comp__"
	if got != want {
		t.Fatalf("Subject sanitize = %q, want %q", got, want)
	}
}

func TestSubjectDefaultsPrefixAndNullSquad(t *testing.T) {
	// Empty prefix → DefaultPrefix; empty squad → the "_" NULL token.
	got := Subject("", "run", "proj", "", "completed")
	want := "ksquad.run.proj._.completed"
	if got != want {
		t.Fatalf("Subject defaults = %q, want %q", got, want)
	}
}

func TestSubjectExplicitPrefixPreserved(t *testing.T) {
	got := Subject("custom", "task", "p1", "team-x", "claimed")
	want := "custom.task.p1.team-x.claimed"
	if got != want {
		t.Fatalf("Subject explicit = %q, want %q", got, want)
	}
}

func TestParseSubjectRoundTrip(t *testing.T) {
	subj := Subject("ksquad", "run", "proj-1", "team-9", "handoff")
	parts, err := ParseSubject(subj)
	if err != nil {
		t.Fatalf("ParseSubject(%q) error: %v", subj, err)
	}
	if parts.Prefix != "ksquad" || parts.Entity != "run" || parts.ProjectID != "proj-1" ||
		parts.Squad != "team-9" || parts.EventType != "handoff" {
		t.Fatalf("ParseSubject round-trip mismatch: %+v", parts)
	}
}

func TestParseSubjectNullSquadDecodesToEmpty(t *testing.T) {
	parts, err := ParseSubject("ksquad.run.proj._.completed")
	if err != nil {
		t.Fatalf("ParseSubject error: %v", err)
	}
	if parts.Squad != "" {
		t.Errorf("null squad token decoded to %q, want empty string", parts.Squad)
	}
}

func TestParseSubjectRejectsMalformed(t *testing.T) {
	bad := []string{
		"",
		"only.four.tokens.here",
		"a.b.c.d.e.f",
		"ksquad",
	}
	for _, s := range bad {
		if _, err := ParseSubject(s); err == nil {
			t.Errorf("ParseSubject(%q) = nil error, want rejection", s)
		}
	}
}

func TestSanitizeTokenEmptyBecomesNullToken(t *testing.T) {
	if got := sanitizeToken(""); got != nullSquadToken {
		t.Errorf("sanitizeToken(\"\") = %q, want %q", got, nullSquadToken)
	}
	if got := sanitizeToken("plain"); got != "plain" {
		t.Errorf("sanitizeToken(plain) = %q, want plain", got)
	}
	if got := sanitizeToken("a\tb"); got != "a_b" {
		t.Errorf("sanitizeToken tab = %q, want a_b", got)
	}
}
