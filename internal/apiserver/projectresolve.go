package apiserver

// projectresolve.go — the SHARED project-by-name resolution + tenancy spine.
//
// This is the single implementation of "resolve a {projectId} path variable to a
// concrete (namespace, name) under the caller's scope" that BOTH the 8.8a
// dashboard read model (dashboard.go) and the S1 project-settings read model
// (projectsettings.go, ISI-3999) key on. Lifting it out of DashboardService's
// method set (rather than copying it) is a deliberate correctness guarantee: the
// two read models MUST resolve the same Project identically — a settings view
// that resolved a different Project than the dashboard for the same URL would be
// a tenancy bug. See the S1 tech note "reuse the dashboard's spine".
//
// The resolvers read through a client.Reader (the host's informer cache) and
// carry the exact semantics DashboardService had inline:
//   - non-admin: team-fenced, existence-hiding 404 for a foreign/unknown Project;
//   - admin: supra-tenant fleet-wide, UID-first, 409 on a bare-name collision.

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// resolveTeamNamespace resolves a caller's Team UID to its namespace (the §12.1
// "a squad IS a namespace" boundary). A UID that resolves to no Team is
// ErrTeamNotFound (404). This is the dashboard's team-scope root; the compose
// write model keeps its own Status.Namespace-gated variant (a write needs the
// namespace reconciled, a read tolerates the metadata namespace).
func resolveTeamNamespace(ctx context.Context, reader client.Reader, teamUID string) (string, error) {
	if teamUID == "" {
		return "", ErrTeamNotFound
	}
	var teams ksquadv1.TeamList
	if err := reader.List(ctx, &teams); err != nil {
		return "", err
	}
	for i := range teams.Items {
		if string(teams.Items[i].UID) == teamUID {
			return teams.Items[i].Namespace, nil
		}
	}
	return "", ErrTeamNotFound
}

// resolveProjectInTeam is the NON-ADMIN scope resolver: resolve the caller's Team
// by UID to its namespace, then require the named Project to live in that
// namespace. A Project outside it — or a wholly unknown one — is
// ErrProjectNotFound (404), indistinguishable (existence-hiding, NFR-SEC5).
// Returns the resolved (namespace, Project name).
func resolveProjectInTeam(ctx context.Context, reader client.Reader, teamUID, projectID string) (string, string, error) {
	ns, name, _, err := resolveProjectInTeamWithUID(ctx, reader, teamUID, projectID)
	return ns, name, err
}

// resolveProjectInTeamWithUID is resolveProjectInTeam plus the resolved Project
// CR UID — the value coord.work_item.project_id keys on (ISI-4132). A read model
// that spans BOTH the CRD cache (Runs, keyed by ProjectRef.Name) and the coord
// board store (work items, keyed by project_id UID) — the project-overview
// series (ISI-4509) — needs both identities from one resolution, so it cannot
// call the (ns, name)-only spine and separately re-derive the UID.
func resolveProjectInTeamWithUID(ctx context.Context, reader client.Reader, teamUID, projectID string) (string, string, string, error) {
	ns, err := resolveTeamNamespace(ctx, reader, teamUID)
	if err != nil {
		return "", "", "", err
	}
	var projects ksquadv1.ProjectList
	if err := reader.List(ctx, &projects, client.InNamespace(ns)); err != nil {
		return "", "", "", err
	}
	for i := range projects.Items {
		p := &projects.Items[i]
		if p.Name == projectID || p.Namespace+"/"+p.Name == projectID {
			return ns, p.Name, string(p.UID), nil
		}
	}
	return "", "", "", ErrProjectNotFound
}

// resolveProjectFleetWide is the ADMIN scope resolver (ISI-3951, extends
// ADR-0010). It lists Projects cluster-wide through the same informer cache (no
// InNamespace ⇒ no new watch, no new RBAC) and matches by UID OR name:
//
//   - a UID match is unique ⇒ return its (namespace, name) immediately, even if a
//     name also collides (UID-first, so a bare-name ambiguity is unambiguous);
//   - exactly one name match ⇒ return its (namespace, name);
//   - more than one name match ⇒ ErrProjectAmbiguous (409, address by UID) —
//     never a silent first-match-wins that would serve the wrong squad's data;
//   - no match ⇒ ErrProjectNotFound (404).
//
// TODO(ISI-3941): the fleet-wide name-only resolution is ambiguous across squads
// and collapses to 409; when ISI-3941's shared fleet-read helper lands, this
// resolver is the single place both read models pick it up.
func resolveProjectFleetWide(ctx context.Context, reader client.Reader, projectID string) (string, string, error) {
	ns, name, _, err := resolveProjectFleetWideWithUID(ctx, reader, projectID)
	return ns, name, err
}

// resolveProjectFleetWideWithUID is resolveProjectFleetWide plus the resolved
// Project CR UID (coord.work_item.project_id), for the cross-store project
// overview series — see resolveProjectInTeamWithUID.
func resolveProjectFleetWideWithUID(ctx context.Context, reader client.Reader, projectID string) (string, string, string, error) {
	var projects ksquadv1.ProjectList
	if err := reader.List(ctx, &projects); err != nil {
		return "", "", "", err
	}
	var nameNS, nameName, nameUID string
	nameMatches := 0
	for i := range projects.Items {
		p := &projects.Items[i]
		if string(p.UID) == projectID && projectID != "" {
			return p.Namespace, p.Name, string(p.UID), nil // UID match is unique — wins over any name collision.
		}
		if p.Namespace+"/"+p.Name == projectID {
			// Composite "namespace/name" match (the console's canonical id) — unique by construction.
			return p.Namespace, p.Name, string(p.UID), nil
		}
		if p.Name == projectID {
			nameNS, nameName, nameUID = p.Namespace, p.Name, string(p.UID)
			nameMatches++
		}
	}
	switch nameMatches {
	case 0:
		return "", "", "", ErrProjectNotFound
	case 1:
		return nameNS, nameName, nameUID, nil
	default:
		return "", "", "", ErrProjectAmbiguous
	}
}

// ProjectRefResolution is the resolved identity of a {projectId} path variable:
// the Project CR UID (coord.work_item.project_id) plus the UID of the Team that
// owns the Project's namespace (coord.work_item.team_id). TeamUID is "" when no
// Team claims the namespace (a dev host or a partially-composed squad) — callers
// then fall back to their previous team source rather than inventing one.
type ProjectRefResolution struct {
	UID     string
	TeamUID string
}

// ProjectRefResolver resolves the console's project reference to the Project CR
// identity the coord board store keys on (ISI-4132). The console addresses a
// Project by its canonical "namespace/name" id (app router + BFF, ISI-3982);
// the board store keys rows by the Project CR's Kubernetes UID. This is the ONE
// seam that translates between them — UID-bearing callers pass through
// unchanged, so agent-side writers (which already carry the UID) are untouched.
type ProjectRefResolver interface {
	ResolveProjectRef(ctx context.Context, projectRef string) (ProjectRefResolution, error)
}

// clientProjectRefResolver resolves through the shared informer cache — the same
// reader the dashboard/settings/github read models use, so the board resolves a
// Project identically to its sibling surfaces.
type clientProjectRefResolver struct {
	reader client.Reader
}

// NewClientProjectRefResolver binds the resolver to the host's informer cache.
func NewClientProjectRefResolver(reader client.Reader) ProjectRefResolver {
	return clientProjectRefResolver{reader: reader}
}

// ResolveProjectRef matches, in priority order (UID-first, exactly like the
// fleet-wide resolver above so a bare-name collision can never shadow a UID):
//  1. Project CR UID (unique ⇒ wins immediately);
//  2. "namespace/name" composite — the console's canonical id;
//  3. bare Project name — 409 ErrProjectAmbiguous on a cross-squad collision.
//
// No match ⇒ ErrProjectNotFound (existence-hiding, NFR-SEC5).
func (r clientProjectRefResolver) ResolveProjectRef(ctx context.Context, projectRef string) (ProjectRefResolution, error) {
	if projectRef == "" {
		return ProjectRefResolution{}, ErrProjectNotFound
	}
	var projects ksquadv1.ProjectList
	if err := r.reader.List(ctx, &projects); err != nil {
		return ProjectRefResolution{}, err
	}
	var match *ksquadv1.Project
	nameMatches := 0
	for i := range projects.Items {
		p := &projects.Items[i]
		if string(p.UID) == projectRef {
			match = p
			nameMatches = 1
			break
		}
		if p.Namespace+"/"+p.Name == projectRef {
			match = p
			nameMatches = 1
			break // composite ids are unique by construction (namespace-scoped name)
		}
		if p.Name == projectRef {
			match = p
			nameMatches++
		}
	}
	switch {
	case nameMatches == 0 || match == nil:
		return ProjectRefResolution{}, ErrProjectNotFound
	case nameMatches > 1:
		return ProjectRefResolution{}, ErrProjectAmbiguous
	}
	out := ProjectRefResolution{UID: string(match.UID)}
	if team, err := teamInNamespace(ctx, r.reader, match.Namespace); err != nil {
		return ProjectRefResolution{}, err
	} else if team != nil {
		out.TeamUID = string(team.UID)
	}
	return out, nil
}

// teamInNamespace returns the Team CR whose namespace is ns, or (nil, nil) when
// none is found (a namespace with no Team CR — the settings projection then
// reports lastTest "untested" rather than failing). The repo test-connection
// result (ISI-3683, AD-7) is cached as an annotation on the Team CR, so the
// settings read model resolves the owning Team by its namespace to read it.
func teamInNamespace(ctx context.Context, reader client.Reader, ns string) (*ksquadv1.Team, error) {
	var teams ksquadv1.TeamList
	if err := reader.List(ctx, &teams); err != nil {
		return nil, err
	}
	for i := range teams.Items {
		if teams.Items[i].Namespace == ns {
			return &teams.Items[i], nil
		}
	}
	return nil, nil
}

// ── Run/Project namespace bridge (ISI-4565) ────────────────────────────────
//
// The operator provisions a per-Team EXECUTION namespace (Team.Status.Namespace,
// e.g. "ksquad-team-bmad-squad-<uid>") where the agent Run CRs are created, while
// the Team and Project CRs stay in the squad HOME namespace (e.g. "bmad-squad").
// The §12.1 "a squad IS a namespace" assumption these read models were written
// against therefore no longer holds: Runs and Projects are NOT co-tenant. Any
// read model that joins Runs to Projects must bridge the two namespaces, or every
// Run is silently dropped and the overview renders empty (the reported defect).

// runNamespaceForHome resolves the execution namespace where a squad's Run CRs
// live, given the squad's home namespace (where the Team/Project CRs live). Falls
// back to homeNS when no Team owns it or Status.Namespace is unset (a co-tenant
// dev host, or the pre-per-team-namespace layout) — those setups still work.
func runNamespaceForHome(ctx context.Context, reader client.Reader, homeNS string) (string, error) {
	team, err := teamInNamespace(ctx, reader, homeNS)
	if err != nil {
		return "", err
	}
	if team != nil && team.Status.Namespace != "" {
		return team.Status.Namespace, nil
	}
	return homeNS, nil
}

// execToHomeNamespace maps each squad's execution namespace (Team.Status.Namespace,
// where Run CRs live) to its home namespace (Team.Namespace, where Project/Team
// CRs live). The fleet overview normalizes a Run's namespace through this map so
// the projectRef join lands on the Project's own (homeNamespace, name) key.
func execToHomeNamespace(teams []ksquadv1.Team) map[string]string {
	m := make(map[string]string, len(teams))
	for i := range teams {
		if teams[i].Status.Namespace != "" {
			m[teams[i].Status.Namespace] = teams[i].Namespace
		}
	}
	return m
}
