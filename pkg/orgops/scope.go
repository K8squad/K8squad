// Package orgops implements the run-scoped board-ops coord API (ISI-3626,
// ADR-0005): the privileged org/project surface a K8squad agent drives mid-run
// to create agents/skills (org:write) and create/archive projects
// (project:write). It is the security-bearing sibling of the task-io seam
// (ISI-3601): same run-scoped bearer token (KSQUAD_COORD_TOKEN), same
// mount-outside-the-cookie-choke-point shape — but every verb is gated on a
// role-DERIVED scope stamped into the token at mint time.
//
// Why the scope, not the skill, is the boundary (ADR-0005 D2): ADR-0004 Phase 1
// skill permissions are advertise-only (no runtime deny), and skills union with
// per-Agent skillRefs, so neither the skill body nor its attachment can be a
// security boundary for a privileged verb. The one authority is this token
// scope, minted from the run's Role by the operator and enforced here
// server-side — exactly as task-io rejects cross-run access. An IC that somehow
// holds the org-ops body still gets a token with no org:write scope, so the API
// says no.
package orgops

// Scope values stamped into a run token (auth.Claims.Scopes → taskio.RunToken.
// Scopes) and checked on every privileged verb. They are coarse capability
// grants keyed on the run's Role, NOT per-object ACLs.
const (
	// ScopeOrgWrite authorizes create-agent / create-skill. Granted to CEO +
	// manager roles (any Role that is a ksquad.io/reports-to target).
	ScopeOrgWrite = "org:write"
	// ScopeProjectWrite authorizes create-project / archive-project. Granted to
	// the CEO role only (the hierarchy root).
	ScopeProjectWrite = "project:write"
)

// LabelReportsTo is the Role/Agent label that mirrors the reporting hierarchy
// (the CRD model has no manager edge — see examples/bmad-team/README.md). A
// Role R is a "manager" iff some other Role carries `ksquad.io/reports-to: R`.
const LabelReportsTo = "ksquad.io/reports-to"

// RoleView is the minimal projection of a Role that scope derivation needs: its
// name and the value of its own ksquad.io/reports-to label (empty when it
// reports to no one). Keeping the logic over this view — not the CRD — makes it
// pure and unit-testable, and keeps this file free of controller-runtime.
type RoleView struct {
	Name      string
	ReportsTo string
}

// DeriveScopes computes the token scopes for a run whose agent assumes `role`,
// given every Role in the namespace (needed to decide, structurally, whether
// anyone reports to `role`). The derivation is the ADR-0005 D2 rule, keyed
// ENTIRELY on the Role graph so per-Agent skillRefs cannot widen it:
//
//   - manager  = `role` is a reports-to target (some other role reports to it)
//     ⇒ org:write.
//   - CEO      = a manager that itself reports to no one (the hierarchy root)
//     ⇒ additionally project:write.
//   - IC       = a leaf role no one reports to ⇒ no scope (task-io still works;
//     every org/project verb is denied).
//
// The result is deterministic and order-independent. A role absent from
// allRoles (or an allRoles that does not include `role`) is handled the same as
// any other: only the reports-to edges decide.
func DeriveScopes(role RoleView, allRoles []RoleView) []string {
	isManager := false
	for _, r := range allRoles {
		if r.Name != role.Name && r.ReportsTo == role.Name {
			isManager = true
			break
		}
	}
	if !isManager {
		return nil // IC / leaf role — least privilege by construction.
	}
	scopes := []string{ScopeOrgWrite}
	if role.ReportsTo == "" {
		// A manager who reports to no one is the root of the hierarchy — the CEO.
		scopes = append(scopes, ScopeProjectWrite)
	}
	return scopes
}
