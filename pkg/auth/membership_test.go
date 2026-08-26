package auth

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestRoleAtLeast(t *testing.T) {
	cases := []struct {
		have, min string
		want      bool
	}{
		// exact match satisfies.
		{ProjectRoleViewer, ProjectRoleViewer, true},
		{ProjectRoleContributor, ProjectRoleContributor, true},
		{ProjectRoleMaintainer, ProjectRoleMaintainer, true},
		// stronger held role satisfies a weaker requirement.
		{ProjectRoleContributor, ProjectRoleViewer, true},
		{ProjectRoleMaintainer, ProjectRoleViewer, true},
		{ProjectRoleMaintainer, ProjectRoleContributor, true},
		// weaker held role fails a stronger requirement.
		{ProjectRoleViewer, ProjectRoleContributor, false},
		{ProjectRoleViewer, ProjectRoleMaintainer, false},
		{ProjectRoleContributor, ProjectRoleMaintainer, false},
		// unknown/empty held role never satisfies anything (fail-closed).
		{"", ProjectRoleViewer, false},
		{"owner", ProjectRoleViewer, false},
		{"", "", false},
	}
	for _, c := range cases {
		if got := RoleAtLeast(c.have, c.min); got != c.want {
			t.Errorf("RoleAtLeast(%q, %q) = %v, want %v", c.have, c.min, got, c.want)
		}
	}
}

// fakeMembershipStore is a map-backed MembershipStore for unit tests (mirrors the
// map-backed stores in service_test.go).
type fakeMembershipStore struct {
	// byPrincipal[principal][project] = role
	byPrincipal map[string]map[string]string
}

func newFakeMembershipStore() *fakeMembershipStore {
	return &fakeMembershipStore{byPrincipal: map[string]map[string]string{}}
}

func (f *fakeMembershipStore) RoleForPrincipal(_ context.Context, principal, project string) (string, error) {
	if projs, ok := f.byPrincipal[principal]; ok {
		if role, ok := projs[project]; ok {
			return role, nil
		}
	}
	return "", ErrNoMembership
}

func (f *fakeMembershipStore) set(principal, project, role string) {
	if f.byPrincipal[principal] == nil {
		f.byPrincipal[principal] = map[string]string{}
	}
	f.byPrincipal[principal][project] = role
}

func (f *fakeMembershipStore) ListForUser(context.Context, uuid.UUID) ([]ProjectMembership, error) {
	return nil, nil
}
func (f *fakeMembershipStore) Grant(context.Context, uuid.UUID, string, string, string) error {
	return nil
}
func (f *fakeMembershipStore) Revoke(context.Context, uuid.UUID, string) error { return nil }

// Ensure the fake satisfies the seam (compile-time contract).
var _ MembershipStore = (*fakeMembershipStore)(nil)

func TestFakeMembershipStore_RoleForPrincipal(t *testing.T) {
	f := newFakeMembershipStore()
	f.set("alice", "checkout", ProjectRoleMaintainer)

	role, err := f.RoleForPrincipal(context.Background(), "alice", "checkout")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if role != ProjectRoleMaintainer {
		t.Fatalf("role = %q, want %q", role, ProjectRoleMaintainer)
	}

	// Unknown project ⇒ ErrNoMembership (fail-closed).
	if _, err := f.RoleForPrincipal(context.Background(), "alice", "billing"); err != ErrNoMembership {
		t.Fatalf("err = %v, want ErrNoMembership", err)
	}
	// Unknown principal ⇒ ErrNoMembership.
	if _, err := f.RoleForPrincipal(context.Background(), "mallory", "checkout"); err != ErrNoMembership {
		t.Fatalf("err = %v, want ErrNoMembership", err)
	}
}
