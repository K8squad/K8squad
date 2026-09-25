package main

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/apiserver"
	"github.com/K8squad/K8squad/pkg/auth"
)

// fakeBootstrapStore records the reconcile's decisions. Create/UpdatePassword are
// the two write paths reconcileBootstrapAdmin must choose between.
type fakeBootstrapStore struct {
	user       *auth.User // seeded existing user, or nil for a fresh install
	created    *auth.User
	updatedID  uuid.UUID
	updatedPwd string
}

func (f *fakeBootstrapStore) ByUsername(_ context.Context, name string) (*auth.User, error) {
	if f.user != nil && f.user.Username == name {
		cp := *f.user
		return &cp, nil
	}
	return nil, auth.ErrNotFound
}

func (f *fakeBootstrapStore) Create(_ context.Context, u *auth.User) error {
	u.ID = uuid.New()
	u.Principal = "user:" + u.Username
	f.created = u
	return nil
}

func (f *fakeBootstrapStore) UpdatePassword(_ context.Context, id uuid.UUID, hash string) error {
	f.updatedID = id
	f.updatedPwd = hash
	return nil
}

func TestReconcileBootstrapAdmin(t *testing.T) {
	ctx := context.Background()
	cfg := apiserver.Config{
		BootstrapAdminUsername: "admin",
		BootstrapAdminPassword: "correct horse battery staple",
	}

	t.Run("fresh install creates the admin", func(t *testing.T) {
		s := &fakeBootstrapStore{}
		reconcileBootstrapAdmin(ctx, s, cfg)
		if s.created == nil {
			t.Fatal("expected the admin to be created on a fresh install")
		}
		if s.created.GlobalRole != auth.RoleAdmin {
			t.Fatalf("created user role = %q, want admin", s.created.GlobalRole)
		}
		if s.updatedPwd != "" {
			t.Fatal("did not expect an UpdatePassword on a fresh install")
		}
	})

	t.Run("matching password is a no-op", func(t *testing.T) {
		hash, err := auth.HashPassword(cfg.BootstrapAdminPassword)
		if err != nil {
			t.Fatal(err)
		}
		s := &fakeBootstrapStore{user: &auth.User{
			ID: uuid.New(), Username: "admin", PasswordHash: hash, GlobalRole: auth.RoleAdmin,
		}}
		reconcileBootstrapAdmin(ctx, s, cfg)
		if s.created != nil {
			t.Fatal("did not expect a Create when the admin already exists")
		}
		if s.updatedPwd != "" {
			t.Fatal("did not expect an UpdatePassword when the stored hash already matches")
		}
	})

	t.Run("diverged password is reconciled (the ISI-4232 upgrade case)", func(t *testing.T) {
		staleHash, err := auth.HashPassword("some other password from a prior install")
		if err != nil {
			t.Fatal(err)
		}
		existing := &auth.User{
			ID: uuid.New(), Username: "admin", PasswordHash: staleHash, GlobalRole: auth.RoleAdmin,
		}
		s := &fakeBootstrapStore{user: existing}
		reconcileBootstrapAdmin(ctx, s, cfg)
		if s.created != nil {
			t.Fatal("did not expect a Create when the admin already exists")
		}
		if s.updatedID != existing.ID {
			t.Fatalf("UpdatePassword id = %v, want %v", s.updatedID, existing.ID)
		}
		// The reconciled hash must verify against the configured password so the
		// documented login works again.
		if err := auth.VerifyPassword(cfg.BootstrapAdminPassword, s.updatedPwd); err != nil {
			t.Fatalf("reconciled hash does not verify against the configured password: %v", err)
		}
	})
}

// stubOrgReader satisfies apiserver.OrgReader with just enough to drive the mention roster seam.
type stubOrgReader struct{ agents []apiserver.OrgAgent }

func (s stubOrgReader) Org(_ context.Context, teamUID string) (apiserver.TeamOrg, error) {
	return apiserver.TeamOrg{TeamID: teamUID, Agents: s.agents}, nil
}
func (stubOrgReader) Agent(context.Context, string, string, bool) (apiserver.OrgAgent, error) {
	return apiserver.OrgAgent{}, nil
}
func (stubOrgReader) AgentRuns(context.Context, string, string, int, int, bool) ([]apiserver.RunSummary, error) {
	return nil, nil
}
func (stubOrgReader) AgentStatuses(context.Context, string) ([]apiserver.AgentStatusDelta, error) {
	return nil, nil
}

// TestRosterForMentionsNilOrg is the ISI-4926 regression: on the cache-less boot shape org is a nil
// apiserver.OrgReader, and wrapping it in a non-nil mentionRoster would pass the handler's
// `h.org != nil` guard and then panic inside TeamAgents. rosterForMentions must collapse a nil org
// to a nil discussion.OrgReader so the /mentions route degrades to ticket-only instead.
func TestRosterForMentionsNilOrg(t *testing.T) {
	if got := rosterForMentions(nil); got != nil {
		t.Fatalf("rosterForMentions(nil) = %#v, want nil discussion.OrgReader", got)
	}
}

func TestRosterForMentionsDelegates(t *testing.T) {
	org := stubOrgReader{agents: []apiserver.OrgAgent{
		{ID: "a-1", Name: "Robo-Coder", Status: "working"},
	}}
	roster := rosterForMentions(org)
	if roster == nil {
		t.Fatal("rosterForMentions(org) = nil, want a wired seam")
	}
	agents, err := roster.TeamAgents(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("TeamAgents: %v", err)
	}
	if len(agents) != 1 || agents[0].Name != "Robo-Coder" || agents[0].Status != "working" {
		t.Fatalf("TeamAgents = %+v, want the delegated Robo-Coder (working)", agents)
	}
}
