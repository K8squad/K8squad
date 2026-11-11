package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// rosterStubResolver backs NewCoordRosterResolver with a canned informer-cache
// answer (names on success, err on failure).
type rosterStubResolver struct {
	names []string
	err   error
}

func (s rosterStubResolver) TeamAgents(context.Context, string) ([]string, error) {
	return s.names, s.err
}

// A nil cache yields a nil RosterResolver: dispatch-on-mention stays OFF on
// the tool path (parity with the apiserver nil-roster degrade) instead of
// dispatching against an empty world.
func TestNewCoordRosterResolverNilDegradesOff(t *testing.T) {
	if got := NewCoordRosterResolver(nil); got != nil {
		t.Fatalf("nil TeamAgentResolver must yield nil RosterResolver, got %#v", got)
	}
}

func TestCoordRosterResolverRosterForTeam(t *testing.T) {
	team := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	t.Run("maps cache names onto status-less TeamAgents", func(t *testing.T) {
		r := NewCoordRosterResolver(rosterStubResolver{names: []string{"alfred", "hermes"}})
		got, err := r.RosterForTeam(context.Background(), team)
		if err != nil {
			t.Fatalf("RosterForTeam: %v", err)
		}
		if len(got) != 2 || got[0].Name != "alfred" || got[1].Name != "hermes" {
			t.Fatalf("roster names not mapped 1:1, got %+v", got)
		}
		for _, a := range got {
			if a.Status != "" {
				t.Fatalf("cache-backed roster must leave Status empty (opt-out degrades), got %+v", a)
			}
		}
	})

	t.Run("propagates cache errors", func(t *testing.T) {
		r := NewCoordRosterResolver(rosterStubResolver{err: errors.New("cache miss")})
		if _, err := r.RosterForTeam(context.Background(), team); err == nil {
			t.Fatal("RosterForTeam must propagate the cache read error")
		}
	})

	t.Run("empty cache yields empty non-nil roster", func(t *testing.T) {
		r := NewCoordRosterResolver(rosterStubResolver{names: []string{}})
		got, err := r.RosterForTeam(context.Background(), team)
		if err != nil {
			t.Fatalf("RosterForTeam: %v", err)
		}
		if got == nil || len(got) != 0 {
			t.Fatalf("empty cache must yield an empty non-nil roster, got %#v", got)
		}
	})
}
