package apiserver

// ============================================================================
// S1 (ISI-3994 / ISI-3999) — Project settings READ model, served at
// GET /api/projects/{projectId}/settings.
// ============================================================================
//
// The board (ISI-3989) reported that a Project offers no way to see its SCM
// config (repo URL, tracked ref, whether a PAT is connected, …). The WRITE
// plumbing already exists — PUT /api/projects/{id} (compose planProject) writes
// spec.repo.{url,ref,auth}; POST /api/credentials (ISI-3937) writes the PAT
// Secret; POST /api/projects/repo-auth/test probes it. What was missing is a
// thin READ projection of a single Project's current config into the shape the
// console Settings tab (S2) renders. This file adds ONLY that projection — no
// new write path, no new CRD field.
//
// Design pins (S1 spec):
//   - Reuse the dashboard's resolve+tenancy+fleet-admin spine (projectresolve.go)
//     so settings and dashboard resolve the same Project identically.
//   - NEVER let token material cross this boundary (NFR-SEC8): the projection
//     returns the credential Secret ref NAME + a connected bool + the tri-state
//     last-test result — never the Secret data, the token, or a webhook/HMAC
//     secret value.
//   - canEdit MUST agree with ComposeService.authorizeWrite by construction —
//     both derive from the same ProjectRoleResolver + writeTierGranted threshold
//     (canWriteProject below), so a projection that says canEdit:true can never
//     face a PUT that then 403s (and vice-versa).

import (
	"context"
	"errors"
	"net/http"

	"github.com/gorilla/mux"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
)

// Last-test tri-state values (AC3): the cached repo test-connection result, or
// "untested" when no test was ever recorded. The mere presence of a credential
// ref does NOT imply "passed" — S2 badges honestly off this field.
const (
	RepoTestPassed   = "passed"
	RepoTestFailed   = "failed"
	RepoTestUntested = "untested"
)

// defaultRepoProvider is the v1 provider surfaced when spec.repo.sync is nil (the
// common "connected repo, sync not configured" state), so S2 renders a real
// provider rather than a blank dropdown (S1 tech note "provider default").
const defaultRepoProvider = "github"

// RepoSettings is the SCM projection of spec.repo (AC1). Ref is the empty string
// when unset (S2 renders "default branch"); Provider defaults to "github" and
// SyncEnabled is false when spec.repo.sync is nil.
type RepoSettings struct {
	URL                 string `json:"url"`
	Ref                 string `json:"ref"`
	Provider            string `json:"provider"`
	SyncEnabled         bool   `json:"syncEnabled"`
	PollIntervalSeconds int32  `json:"pollIntervalSeconds"`
	ReflectOutbound     bool   `json:"reflectOutbound"`
}

// AuthSettings projects the SCM credential state WITHOUT the token (AC2/AC3).
// Connected is true iff spec.repo.auth.credentialSecretRef.name is set;
// CredentialSecretRefName is that name (the ref, never the Secret's data);
// LastTest is the tri-state repo test-connection result.
type AuthSettings struct {
	Connected               bool   `json:"connected"`
	CredentialSecretRefName string `json:"credentialSecretRefName"`
	LastTest                string `json:"lastTest"`
}

// ProjectSettings is the composed read model behind GET
// /api/projects/{id}/settings (AC1). It carries NO token/Secret data field — the
// no-token invariant (AC2) is a structural property of this shape.
type ProjectSettings struct {
	Project ProjectRef   `json:"project"`
	Repo    RepoSettings `json:"repo"`
	Auth    AuthSettings `json:"auth"`
	CanEdit bool         `json:"canEdit"`
}

// ProjectSettingsService is the S1 read model. It shares the dashboard's reader
// (the host informer cache) and the 15.4 ProjectRoleResolver so canEdit tracks
// the same write-tier decision the compose PUT enforces. A nil service ⇒ the
// route keeps the documented 501 (cluster-less dev run), exactly like the
// dashboard.
type ProjectSettingsService struct {
	reader client.Reader
	roles  ProjectRoleResolver
}

// NewProjectSettingsService builds the S1 read model. reader MUST have
// api/v1alpha1 registered. roles is the 15.4 membership resolver (nil ⇒ canEdit
// is false for every non-admin — fail closed, matching authorizeWrite's nil-
// resolver posture).
func NewProjectSettingsService(reader client.Reader, roles ProjectRoleResolver) *ProjectSettingsService {
	return &ProjectSettingsService{reader: reader, roles: roles}
}

// Settings composes the projection for projectID under the caller's scope. It
// resolves the Project exactly as the dashboard does — team-fenced for a
// non-admin (ErrProjectNotFound → 404 existence-hiding, AC4), fleet-wide for an
// admin (ErrProjectAmbiguous → 409, AC5) — then maps spec.repo into the AC1
// shape, reads the tri-state last-test from the owning Team's annotation (AC3),
// and computes canEdit from the shared write-tier predicate (AC6).
func (s *ProjectSettingsService) Settings(ctx context.Context, auth discussion.AuthorContext, projectID string) (ProjectSettings, error) {
	var ns, name string
	var err error
	if auth.IsAdmin {
		ns, name, err = resolveProjectFleetWide(ctx, s.reader, projectID)
	} else {
		ns, name, err = resolveProjectInTeam(ctx, s.reader, auth.TeamID.String(), projectID)
	}
	if err != nil {
		return ProjectSettings{}, err
	}

	// Re-Get the resolved Project by (namespace, name) to read its spec.repo.
	var project ksquadv1.Project
	if err := s.reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &project); err != nil {
		return ProjectSettings{}, err
	}

	out := ProjectSettings{
		Project: ProjectRef{Name: name, Namespace: ns},
		Repo:    projectRepoSettings(&project.Spec.Repo),
		Auth:    projectAuthSettings(&project.Spec.Repo),
		CanEdit: canWriteProject(ctx, s.roles, auth, name),
	}

	// Last-test tri-state (AC3): the repo test-connection result is cached on the
	// owning Team CR annotation (ISI-3683, AD-7). Resolve the Team by the project's
	// namespace; absent Team or absent annotation ⇒ "untested" (never fabricated).
	out.Auth.LastTest = RepoTestUntested
	if team, terr := teamInNamespace(ctx, s.reader, ns); terr == nil && team != nil {
		if recorded, passed := ksquadOnboardingRepoTest(team); recorded {
			if passed {
				out.Auth.LastTest = RepoTestPassed
			} else {
				out.Auth.LastTest = RepoTestFailed
			}
		}
	}
	return out, nil
}

// projectRepoSettings maps spec.repo into the AC1 repo projection. Nil
// spec.repo.sync ⇒ provider "github", syncEnabled false (the "connected repo,
// sync not configured" default); otherwise the sync fields carry through.
func projectRepoSettings(repo *ksquadv1.RepoSpec) RepoSettings {
	rs := RepoSettings{
		URL:      repo.URL,
		Ref:      repo.Ref, // empty ⇒ provider default branch (caller renders "default branch")
		Provider: defaultRepoProvider,
	}
	if repo.Sync != nil {
		rs.SyncEnabled = true
		if repo.Sync.Provider != "" {
			rs.Provider = repo.Sync.Provider
		}
		rs.PollIntervalSeconds = repo.Sync.PollIntervalSeconds
		rs.ReflectOutbound = repo.Sync.ReflectOutbound
	}
	return rs
}

// projectAuthSettings projects the credential state WITHOUT the token (AC2): a
// PAT is connected iff spec.repo.auth.credentialSecretRef.name is set (the SAME
// rule projectMilestoneComplete uses, onboarding.go). LastTest is filled by the
// caller (it needs the Team annotation, not the Project).
func projectAuthSettings(repo *ksquadv1.RepoSpec) AuthSettings {
	as := AuthSettings{LastTest: RepoTestUntested}
	if repo.Auth != nil && repo.Auth.CredentialSecretRef.Name != "" {
		as.Connected = true
		as.CredentialSecretRefName = repo.Auth.CredentialSecretRef.Name
	}
	return as
}

// ksquadOnboardingRepoTest reads the cached repo test-connection tri-state off a
// Team CR (the ksquad.io/onboarding-test-connection-repo annotation, AD-7),
// reusing the onboarding read model's own RepoTestConnectionFlag so the two
// project the identical flag.
func ksquadOnboardingRepoTest(team *ksquadv1.Team) (recorded, passed bool) {
	return RepoTestConnectionFlag(team)
}

// canWriteProject reports whether author may perform a write-tier mutation on the
// named Project — the read-side twin of ComposeService.authorizeWrite. It mirrors
// authorizeWrite's decision exactly: an empty principal ⇒ false (defence in
// depth); an admin ⇒ true (supra-tenant, no membership needed); otherwise a wired
// resolver must return a role clearing the SAME writeTierGranted threshold the
// write path uses. A nil resolver, empty project, no membership, or a resolver
// error ⇒ false (fail closed — the exact non-zero-status branches of
// authorizeWrite). This is the AC6 "agree by construction" guarantee.
func canWriteProject(ctx context.Context, roles ProjectRoleResolver, author discussion.AuthorContext, project string) bool {
	if author.Principal == "" {
		return false
	}
	if author.IsAdmin {
		return true
	}
	if roles == nil || project == "" {
		return false
	}
	role, err := roles.RoleForPrincipal(ctx, author.Principal, project)
	if err != nil {
		return false
	}
	return writeTierGranted(role)
}

// ============================================================================
// Handler — GET /api/projects/{projectId}/settings behind the §13 choke point.
// ============================================================================

// projectSettings is the handler behind the route. BFFAuthz has already resolved
// the AuthorContext; the projection reads NOTHING from the request except the
// path variable. Statuses mirror the dashboard's exactly (AC4): 401
// unauthenticated, 404 no-team-scope / foreign-or-unknown Project
// (existence-hiding), 409 admin cross-squad name collision, 502 read-model
// unavailable, 200 with the projection.
func (s *Server) projectSettings(svc *ProjectSettingsService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := discussion.AuthFromContext(r.Context())
		if !ok || auth.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		projectID := mux.Vars(r)["projectId"]
		settings, err := svc.Settings(r.Context(), auth, projectID)
		switch {
		case err == nil:
			writeJSON(w, http.StatusOK, settings)
		case errors.Is(err, ErrTeamNotFound):
			writeJSONError(w, http.StatusNotFound, "no settings for this team scope")
		case errors.Is(err, ErrProjectNotFound):
			writeJSONError(w, http.StatusNotFound, "no settings for this project")
		case errors.Is(err, ErrProjectAmbiguous):
			writeJSONError(w, http.StatusConflict, "project name ambiguous across squads; address by uid")
		default:
			writeJSONError(w, http.StatusBadGateway, "settings read model unavailable")
		}
	}
}
