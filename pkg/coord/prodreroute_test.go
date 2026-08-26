// prodreroute_test.go — unit coverage for the Story 2.10 PURE surface: the
// 3.7 escalation policy (ShouldReroute), the policy validation, and the 7.6
// alternate-credential pick. These need NO Postgres (same discipline as
// resume_test.go); the SQL-bound half of the story — fenced release, hold,
// different-credential claim guard — is the chaos gate
// (prod_reroute_chaos_test.go, TestSpineReroute).
package coord_test

import (
	"testing"
	"time"

	"github.com/K8squad/K8squad/pkg/coord"
)

func TestShouldReroute(t *testing.T) {
	pol := coord.ReroutePolicy{AfterAttempts: 2, MinWindow: 10 * time.Minute}
	long := 15 * time.Minute
	short := 30 * time.Second

	cases := []struct {
		name       string
		attempt    int
		retryAfter *time.Duration
		want       bool
	}{
		{"first episode short window retains checkout", 1, &short, false},
		{"first episode no Retry-After retains checkout", 1, nil, false},
		{"repeat escalates (attempt at threshold)", 2, nil, true},
		{"repeat escalates past threshold", 5, &short, true},
		{"long window escalates on first episode", 1, &long, true},
		{"window at threshold escalates", 1, &pol.MinWindow, true},
		// F10 (ISI-3083): this case fires on the REPEAT arm (9 >= AfterAttempts),
		// not the long-window arm — a nil window is fine because the repeat
		// threshold already triggers. The "first episode, no Retry-After" long-arm
		// property is covered by the retains-checkout case above.
		{"repeat arm escalates even with nil window", 9, nil, true},
		{"attempt clamped to 1", 0, &short, false},
		{"negative attempt clamped to 1", -3, &short, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := coord.ShouldReroute(tc.attempt, tc.retryAfter, pol); got != tc.want {
				t.Fatalf("ShouldReroute(attempt=%d, retryAfter=%v) = %v, want %v",
					tc.attempt, tc.retryAfter, got, tc.want)
			}
		})
	}
}

func TestPickAlternateCredential(t *testing.T) {
	cases := []struct {
		name       string
		throttled  string
		candidates []string
		want       string
		wantOK     bool
	}{
		{"empty roster: no alternate", "cred-a", nil, "", false},
		{"only the throttled credential: no alternate", "cred-a", []string{"cred-a"}, "", false},
		{"first differing candidate wins, order preserved", "cred-a", []string{"cred-a", "cred-c", "cred-b"}, "cred-c", true},
		{"empty-string candidates are skipped", "cred-a", []string{"", "cred-b"}, "cred-b", true},
		{"all-empty roster: no alternate", "cred-a", []string{"", ""}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := coord.PickAlternateCredential(tc.throttled, tc.candidates)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("PickAlternateCredential(%q, %v) = (%q, %v), want (%q, %v)",
					tc.throttled, tc.candidates, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestNewProdRerouteStore(t *testing.T) {
	if _, err := coord.NewProdRerouteStore(nil); err == nil {
		t.Fatal("nil db accepted, want rejection")
	}
}

// TestDefaultReroutePolicy pins the shipped default's field values (F4/ISI-3083).
// Without this, mutating DefaultReroutePolicy() to return ReroutePolicy{} left
// BOTH lanes green — the function was at 0% coverage and TestShouldReroute built
// the struct literally. This test kills that mutation and proves the default is
// Validate-clean.
func TestDefaultReroutePolicy(t *testing.T) {
	got := coord.DefaultReroutePolicy()
	if got.AfterAttempts != 2 {
		t.Fatalf("DefaultReroutePolicy().AfterAttempts = %d, want 2", got.AfterAttempts)
	}
	if got.MinWindow != 10*time.Minute {
		t.Fatalf("DefaultReroutePolicy().MinWindow = %s, want 10m", got.MinWindow)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("DefaultReroutePolicy() fails Validate: %v", err)
	}
}

// TestReroutePolicyValidate proves the fail-open guard (F4/ISI-3083): a
// zero-value or AfterAttempts<2 policy re-routes every first, short pause and
// must be rejected before it drives a release.
func TestReroutePolicyValidate(t *testing.T) {
	cases := []struct {
		name    string
		policy  coord.ReroutePolicy
		wantErr bool
	}{
		{"zero value fails open", coord.ReroutePolicy{}, true},
		{"AfterAttempts 1 fires on first pause", coord.ReroutePolicy{AfterAttempts: 1, MinWindow: time.Minute}, true},
		{"non-positive window", coord.ReroutePolicy{AfterAttempts: 2, MinWindow: 0}, true},
		{"negative window", coord.ReroutePolicy{AfterAttempts: 2, MinWindow: -time.Minute}, true},
		{"valid policy", coord.ReroutePolicy{AfterAttempts: 2, MinWindow: 10 * time.Minute}, false},
		{"default is valid", coord.DefaultReroutePolicy(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.policy.Validate(); (err != nil) != tc.wantErr {
				t.Fatalf("Validate(%+v) err=%v, wantErr=%v", tc.policy, err, tc.wantErr)
			}
		})
	}
}
