package orgops

import (
	"sort"
	"testing"
)

// bmadRoles mirrors examples/bmad-team/05-roles.yaml's reports-to graph so the
// derivation is pinned to the reference squad the ADR names.
func bmadRoles() []RoleView {
	return []RoleView{
		{Name: "ceo", ReportsTo: ""},
		{Name: "product-manager", ReportsTo: "ceo"},
		{Name: "architect", ReportsTo: "ceo"},
		{Name: "ux-designer", ReportsTo: "ceo"},
		{Name: "brainstormer", ReportsTo: "product-manager"},
		{Name: "challenger", ReportsTo: "product-manager"},
		{Name: "content-writer", ReportsTo: "product-manager"},
		{Name: "code-reviewer", ReportsTo: "architect"},
		{Name: "test-architect", ReportsTo: "architect"},
		{Name: "coder", ReportsTo: "architect"},
		{Name: "devops-engineer", ReportsTo: "architect"},
		{Name: "observability-engineer", ReportsTo: "architect"},
		{Name: "graphical-designer", ReportsTo: "ux-designer"},
	}
}

func roleByName(rs []RoleView, name string) RoleView {
	for _, r := range rs {
		if r.Name == name {
			return r
		}
	}
	return RoleView{Name: name}
}

func sortedScopes(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

func eqScopes(a, b []string) bool {
	a, b = sortedScopes(a), sortedScopes(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDeriveScopesBMAD(t *testing.T) {
	all := bmadRoles()
	cases := []struct {
		role string
		want []string
	}{
		// CEO: manager AND hierarchy root → both scopes.
		{"ceo", []string{ScopeOrgWrite, ScopeProjectWrite}},
		// Managers (reports-to targets that themselves report to the CEO) → org only.
		{"product-manager", []string{ScopeOrgWrite}},
		{"architect", []string{ScopeOrgWrite}},
		{"ux-designer", []string{ScopeOrgWrite}},
		// ICs (leaf roles nobody reports to) → nothing.
		{"brainstormer", nil},
		{"challenger", nil},
		{"content-writer", nil},
		{"code-reviewer", nil},
		{"test-architect", nil},
		{"coder", nil},
		{"devops-engineer", nil},
		{"observability-engineer", nil},
		{"graphical-designer", nil},
	}
	for _, c := range cases {
		got := DeriveScopes(roleByName(all, c.role), all)
		if !eqScopes(got, c.want) {
			t.Errorf("DeriveScopes(%s) = %v, want %v", c.role, got, c.want)
		}
	}
}

// A mid-level manager (a reports-to target that ALSO reports to someone) gets
// org:write but NOT project:write — only the root is the CEO.
func TestDeriveScopesMidManagerNoProjectWrite(t *testing.T) {
	roles := []RoleView{
		{Name: "ceo", ReportsTo: ""},
		{Name: "director", ReportsTo: "ceo"},
		{Name: "lead", ReportsTo: "director"},
	}
	got := DeriveScopes(roleByName(roles, "director"), roles)
	if !eqScopes(got, []string{ScopeOrgWrite}) {
		t.Fatalf("mid-manager scopes = %v, want [org:write]", got)
	}
	if DeriveScopes(roleByName(roles, "lead"), roles) != nil {
		t.Fatal("leaf 'lead' should have no scope")
	}
}

// The scope derives from the Role graph, never from an Agent's skills: an IC
// leaf role yields no scope regardless of anything else. This is the closed
// union loophole (ADR-0005 D2).
func TestDeriveScopesLeafAlwaysEmpty(t *testing.T) {
	roles := bmadRoles()
	if DeriveScopes(roleByName(roles, "coder"), roles) != nil {
		t.Fatal("an IC role must derive no scope — the union loophole must stay closed")
	}
}

// Empty / single-role graphs are safe: a lone root reports-to-nobody with no
// reports is NOT a manager (nobody reports to it) → no scope. Fail-closed.
func TestDeriveScopesDegenerate(t *testing.T) {
	if DeriveScopes(RoleView{Name: "solo"}, nil) != nil {
		t.Fatal("a role with no graph should derive no scope")
	}
	single := []RoleView{{Name: "solo"}}
	if DeriveScopes(roleByName(single, "solo"), single) != nil {
		t.Fatal("a lone role nobody reports to is an IC, not a CEO")
	}
}

// A self-referential reports-to edge must not make a role its own manager.
func TestDeriveScopesIgnoresSelfEdge(t *testing.T) {
	roles := []RoleView{{Name: "weird", ReportsTo: "weird"}}
	if DeriveScopes(roleByName(roles, "weird"), roles) != nil {
		t.Fatal("a self-reports-to edge must not confer manager scope")
	}
}
