package auth

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ============================================================================
// 15.9 OIDC group→access-level mapping — the SEAM (ISI-2920).
//
// This type + Resolve implement the pure mapping contract of story 15.9: given the
// group claims an OIDC token carries, resolve them against auth.oidc.groupMapping
// and yield the global_role / project memberships to upsert. It deliberately adds
// NO new authorization code path: whatever it yields is written as ordinary
// global_role/project_memberships records that the 15.4 middleware already (will)
// enforce.
//
// The OIDC login leg itself (IdP redirect, token exchange, claim extraction) is the
// AuthProvider fast-follow ADR-033 scoped; when it lands it calls Resolve with the
// real claims. The local-cred path calls it with no groups (no-op), so the seam is
// wired end-to-end today without an IdP.
// ============================================================================

// Project membership roles (ADR-035 three-tier vocabulary).
const (
	ProjectRoleViewer      = "viewer"
	ProjectRoleContributor = "contributor"
	ProjectRoleMaintainer  = "maintainer"
)

// ProjectMembership is one (project, role) pair a group mapping grants.
type ProjectMembership struct {
	Project string `json:"project"`
	Role    string `json:"role"`
}

// GroupAccess is the mapped access for one OIDC group claim value: either the
// string "admin" (global admin) or a {project, role} object.
type GroupAccess struct {
	Admin      bool               `json:"-"`
	Membership *ProjectMembership `json:"-"`
}

// UnmarshalJSON accepts the values.yaml / config shapes:
//
//	{"platform-admins": "admin",
//	 "k8s-devs": {"project": "my-project", "role": "contributor"}}
func (g *GroupAccess) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		if s != RoleAdmin {
			return fmt.Errorf("auth: groupMapping string form must be %q, got %q", RoleAdmin, s)
		}
		*g = GroupAccess{Admin: true}
		return nil
	}
	var m ProjectMembership
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("auth: groupMapping entry must be \"admin\" or {project,role}: %w", err)
	}
	switch m.Role {
	case ProjectRoleViewer, ProjectRoleContributor, ProjectRoleMaintainer:
	default:
		return fmt.Errorf("auth: groupMapping project role must be viewer|contributor|maintainer, got %q", m.Role)
	}
	if m.Project == "" {
		return fmt.Errorf("auth: groupMapping entry requires a project")
	}
	*g = GroupAccess{Membership: &m}
	return nil
}

// GroupMapping is the parsed auth.oidc.groupMapping config: group claim value → access.
type GroupMapping map[string]GroupAccess

// ParseGroupMapping parses the raw JSON config, failing closed on any malformed entry
// (a typo'd mapping must never silently grant nothing or everything).
func ParseGroupMapping(raw string) (GroupMapping, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var gm GroupMapping
	if err := json.Unmarshal([]byte(raw), &gm); err != nil {
		return nil, fmt.Errorf("auth: parse groupMapping: %w", err)
	}
	return gm, nil
}

// RoleAssignment is the resolved outcome of mapping a user's group claims.
type RoleAssignment struct {
	// GlobalRole is "admin" when a mapped group grants admin (conflict promotion:
	// admin wins over project-level memberships, 15.9), or "user" when any mapped
	// group grants a project membership — the base global_role the provisioned
	// record carries (auth.user.global_role ∈ {admin,user}, NOT NULL, 0008).
	// Empty means NOTHING was mapped: no implicit grant (15.9).
	GlobalRole string
	// Memberships are the (project, role) pairs granted by mapped groups.
	Memberships []ProjectMembership
}

// Resolve maps the user's group claims against the mapping. Unmapped groups are
// silently ignored (no implicit grant); a user in both an admin group and project
// groups is promoted to admin; duplicate projects collapse to the strongest role
// (maintainer > contributor > viewer) so the assignment is deterministic.
func (gm GroupMapping) Resolve(groups []string) RoleAssignment {
	var out RoleAssignment
	rank := map[string]int{ProjectRoleViewer: 1, ProjectRoleContributor: 2, ProjectRoleMaintainer: 3}
	byProject := map[string]string{}
	for _, g := range groups {
		access, ok := gm[g]
		if !ok {
			continue // unmapped ⇒ ignored, no implicit grant
		}
		if access.Admin {
			out.GlobalRole = RoleAdmin
			continue
		}
		if m := access.Membership; m != nil {
			if out.GlobalRole != RoleAdmin {
				// A mapped membership grants the base global role (never demotes
				// an already-promoted admin).
				out.GlobalRole = RoleUser
			}
			if prev, seen := byProject[m.Project]; !seen || rank[m.Role] > rank[prev] {
				byProject[m.Project] = m.Role
			}
		}
	}
	// Admin promotion resolves conflicts: memberships still populate (reviewable via
	// 8.15) but the global role carries the wider authority.
	for _, p := range sortedKeys(byProject) {
		out.Memberships = append(out.Memberships, ProjectMembership{Project: p, Role: byProject[p]})
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
