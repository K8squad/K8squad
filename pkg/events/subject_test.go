package events

import "testing"

func TestSubject_FullTaxonomy(t *testing.T) {
	got := Subject("ksquad", "work_item", "proj-1", "squad-a", "claimed")
	if want := "ksquad.work_item.proj-1.squad-a.claimed"; got != want {
		t.Fatalf("Subject = %q, want %q", got, want)
	}
}

func TestSubject_NullSquadUsesUnderscore(t *testing.T) {
	got := Subject("ksquad", "run", "proj-1", "", "completed")
	if want := "ksquad.run.proj-1._.completed"; got != want {
		t.Fatalf("Subject with NULL squad = %q, want %q (positional taxonomy)", got, want)
	}
}

func TestSubject_DefaultPrefix(t *testing.T) {
	got := Subject("", "scm", "p", "s", "check_run.failed")
	// event_type dots are sanitized so a component cannot split into extra tokens.
	if want := "ksquad.scm.p.s.check_run_failed"; got != want {
		t.Fatalf("Subject = %q, want %q", got, want)
	}
}

func TestSubject_SanitizesWildcardsAndSeparators(t *testing.T) {
	// A malformed component must never inject a wildcard or an extra token.
	got := Subject("ksquad", "run", "a.b", "x*y", "e>f")
	if want := "ksquad.run.a_b.x_y.e_f"; got != want {
		t.Fatalf("Subject = %q, want %q (metacharacters neutralized)", got, want)
	}
}
