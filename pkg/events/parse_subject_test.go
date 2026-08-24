package events

import "testing"

// ParseSubject is the read-side of the subject taxonomy the plugin subscribe SDK
// (Story 12.2) relies on. It must invert Subject for real values, decode the "_"
// NULL-squad token back to "", and reject anything that is not the five-token
// contract.
func TestParseSubject_RoundTripsSubject(t *testing.T) {
	cases := []struct {
		entity, project, squad, eventType string
	}{
		{"work_item", "proj-1", "squad-a", "claimed"},
		{"run", "proj-1", "", "completed"}, // NULL squad → "_" → ""
		{"scm", "p", "s", "check_run.failed"},
	}
	for _, c := range cases {
		subj := Subject("ksquad", c.entity, c.project, c.squad, c.eventType)
		got, err := ParseSubject(subj)
		if err != nil {
			t.Fatalf("ParseSubject(%q) error: %v", subj, err)
		}
		if got.Entity != c.entity || got.ProjectID != c.project || got.Squad != c.squad {
			t.Fatalf("ParseSubject(%q) = %+v, want entity=%q project=%q squad=%q",
				subj, got, c.entity, c.project, c.squad)
		}
		// event_type separators are sanitized on the way out; the token that comes
		// back is the on-wire (sanitized) form, so compare against sanitizeToken.
		if want := sanitizeToken(c.eventType); got.EventType != want {
			t.Fatalf("ParseSubject(%q).EventType = %q, want %q", subj, got.EventType, want)
		}
		if got.Prefix != "ksquad" {
			t.Fatalf("ParseSubject(%q).Prefix = %q, want ksquad", subj, got.Prefix)
		}
	}
}

func TestParseSubject_RejectsMalformed(t *testing.T) {
	for _, subj := range []string{"", "ksquad.run", "ksquad.run.p.s", "ksquad.run.p.s.done.extra"} {
		if _, err := ParseSubject(subj); err == nil {
			t.Fatalf("ParseSubject(%q) = nil error, want rejection of non-5-token subject", subj)
		}
	}
}

func TestParseSubject_NullSquadTokenDecodesToEmpty(t *testing.T) {
	got, err := ParseSubject("ksquad.run.proj-1._.completed")
	if err != nil {
		t.Fatal(err)
	}
	if got.Squad != "" {
		t.Fatalf("Squad = %q, want \"\" (the _ NULL token mirrors Event.Squad NULL)", got.Squad)
	}
}
