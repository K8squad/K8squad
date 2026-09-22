package reviewdispatch

import (
	"context"
	"errors"
	"testing"
)

// fakeResolver is a coord.TeamAgentResolver returning a scripted composition.
type fakeResolver struct {
	agents map[string][]string
	err    error
}

func (f fakeResolver) TeamAgents(_ context.Context, teamUID string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.agents[teamUID], nil
}

func TestIsTeamAgent(t *testing.T) {
	r := fakeResolver{agents: map[string][]string{"team-1": {"alice-bot", "bob-bot"}}}
	m := NewTeamMembership(r)

	cases := []struct {
		name        string
		team, actor string
		want        bool
	}{
		{"member", "team-1", "alice-bot", true},
		{"non-member", "team-1", "carol-bot", false},
		{"unknown-team", "team-x", "alice-bot", false},
		{"empty-team", "", "alice-bot", false},
		{"empty-actor", "team-1", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := m.IsTeamAgent(context.Background(), tc.team, tc.actor)
			if err != nil {
				t.Fatalf("IsTeamAgent: %v", err)
			}
			if got != tc.want {
				t.Errorf("IsTeamAgent(%q,%q) = %v, want %v", tc.team, tc.actor, got, tc.want)
			}
		})
	}
}

// TestIsTeamAgentPropagatesResolverError: a dangling-team / infra error must fail
// loudly, never be swallowed into a vacuous "not a member".
func TestIsTeamAgentPropagatesResolverError(t *testing.T) {
	boom := errors.New("team uid resolves to no Team CR")
	m := NewTeamMembership(fakeResolver{err: boom})
	if _, err := m.IsTeamAgent(context.Background(), "team-1", "alice-bot"); !errors.Is(err, boom) {
		t.Fatalf("want resolver error propagated, got %v", err)
	}
}
