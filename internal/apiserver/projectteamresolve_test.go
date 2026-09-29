package apiserver

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestNewProjectTeamResolver covers the ISI-5198 adapter that maps the shared ProjectRefResolver onto
// the discussion room-tenancy seam: a resolved Team passes through as a parsed UID; a nil refs, an
// unresolved ref, or a Team-less project all report ok=false so the handler falls back to the caller's
// own Team scope.
func TestNewProjectTeamResolver(t *testing.T) {
	if NewProjectTeamResolver(nil) != nil {
		t.Fatal("nil refs must yield a nil resolver (SetProjectTeamResolver(nil) → legacy caller-team scope)")
	}

	const teamUID = "7191cc8c-f4b7-4b60-b63e-d25408ac0d1c"

	t.Run("resolved team passes through", func(t *testing.T) {
		r := NewProjectTeamResolver(&fakeProjectRefs{res: ProjectRefResolution{UID: "p1", TeamUID: teamUID}})
		team, ok, err := r.ResolveProjectTeam(context.Background(), "bmad-squad/bmad-demo")
		if err != nil || !ok {
			t.Fatalf("got ok=%v err=%v, want ok=true err=nil", ok, err)
		}
		if team != uuid.MustParse(teamUID) {
			t.Errorf("team = %v, want %v", team, teamUID)
		}
	})

	t.Run("project with no owning team → ok=false", func(t *testing.T) {
		r := NewProjectTeamResolver(&fakeProjectRefs{res: ProjectRefResolution{UID: "p1"}}) // TeamUID == ""
		if _, ok, err := r.ResolveProjectTeam(context.Background(), "x"); ok || err != nil {
			t.Fatalf("got ok=%v err=%v, want ok=false err=nil", ok, err)
		}
	})

	t.Run("unresolved ref (ErrProjectNotFound) → ok=false, no error", func(t *testing.T) {
		r := NewProjectTeamResolver(&fakeProjectRefs{err: ErrProjectNotFound})
		if _, ok, err := r.ResolveProjectTeam(context.Background(), "missing"); ok || err != nil {
			t.Fatalf("got ok=%v err=%v, want ok=false err=nil", ok, err)
		}
	})

	t.Run("ambiguous ref → ok=false, no error", func(t *testing.T) {
		r := NewProjectTeamResolver(&fakeProjectRefs{err: ErrProjectAmbiguous})
		if _, ok, err := r.ResolveProjectTeam(context.Background(), "dupe"); ok || err != nil {
			t.Fatalf("got ok=%v err=%v, want ok=false err=nil", ok, err)
		}
	})
}
