package apiserver

// ============================================================================
// Story 8.5 CRD-apply write surface (ISI-3198) — the apiserver BFF that turns a
// typed compose request from the console 04-compose-crd screen into an applied
// ksquad.io CRD (Team / Project / Agent / Role / Skill). It is the write sibling
// of the 8.8a dashboard read model and mirrors the OTelConfig(1.5) compose shape:
// a NARROW, typed wire contract per kind — never arbitrary YAML exec (scope guard
// R6, parent ISI-3196) — server-side validated, RBAC-gated, and provenanced.
//
// The five invariants (parent ISI-3196), enforced here:
//  1. Server-side field validation BEFORE apply: an invalid request is a
//     field-level 422, never a partial apply (nothing touches the cluster until
//     the whole request validates).
//  2. RBAC write-tier, REUSING the 15.3/15.4 membership primitive (pkg/auth
//     RoleForPrincipal + RoleAtLeast, ADR-035) — the SAME resolver the dashboard
//     gate uses. admin is fleet-wide authority (bypass); a viewer is 403; a
//     write-level (contributor+) grant is required. Team is the tenancy root, so
//     Team compose is admin-only.
//  3. Team-scoped: every CR is applied into the CALLER'S Team namespace (resolved
//     from AuthorContext.TeamID, the §12.1 tenancy root). A caller can only ever
//     read/write their own namespace, so a cross-tenant name is structurally a
//     404 (existence-hiding), never a 200. The ONE exception is the Team kind
//     itself: a Team IS the tenancy root (ADR-011) that mints the squad namespace,
//     so it cannot be namespaced inside it — Team CRs land in the control-plane
//     namespace (systemNS) and never require a pre-existing caller namespace
//     (that requirement broke first-run onboarding step 1 — ISI-3919).
//  4. Idempotent by (kind, team, name): PUT is an upsert keyed on the CR name in
//     the Team namespace — it never duplicates. Each apply is a NEW revision.
//  5. Every apply writes a durable provenanced row (who/what/when) to the
//     coordination record (coord.audit_log, §6.5) via the same sink the admin
//     mutations use — the 2.6 audit-trail contract.
//
// Revision-on-edit (§6.4/3.6 goal-versioning): an edit is a NEW revision, never
// an in-place mutation of a RUNNING snapshot. In-flight Runs snapshot their CRD
// inputs at assembly (project_types.go: "a goal change is a new Project revision;
// the next Run assembles against it while in-flight Runs keep their snapshot"), so
// updating the CR here cannot retroactively change a live Run. The revision is
// surfaced explicitly via a monotonic `ksquad.io/revision` annotation so the
// console (and provenance) can show "you created revision N" without depending on
// server-assigned generation semantics.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/auth"
	"github.com/K8squad/K8squad/pkg/capability"
	"github.com/K8squad/K8squad/pkg/modelendpoint"
)

// RevisionAnnotation carries the monotonic apply revision on every composed CR
// (create ⇒ "1", each edit ⇒ +1). It is the explicit, inspectable "new revision
// on edit" signal (§6.4) surfaced back to the console and stamped into provenance.
const RevisionAnnotation = "ksquad.io/revision"

// CRDApplier is the write seam over the cluster: the subset of the
// controller-runtime client.Client the compose surface uses. Production wires a
// real client.Client (client.New, main.go); unit tests inject the fake client.
// A nil applier ⇒ the routes keep the documented 501 (cluster-less dev run),
// exactly like the read models.
type CRDApplier interface {
	Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error
	Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error
	Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error
	List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error
	Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error
}

// ComposeProvenance is the append-only provenance sink (§6.5 / 2.6): who applied
// what, when. It is the SAME shape as AuthRoutesOptions.Audit so main.go wires one
// coord.audit_log writer for both. Best-effort by design (the apply has already
// landed); a failed append is logged loudly, never a silent drop.
type ComposeProvenance func(ctx context.Context, eventType, principal string, payload map[string]any)

// AdminTenancyRebinder is the ONE seam where the k8s-CR tenancy root meets the
// auth DB (ISI-3924 / ADR-0009). The bootstrap admin is seeded with an invented
// team_id (cmd/apiserver/main.go bootstrapAdmin) that no server-assigned Team CR
// uid can ever match, so every team-scoped op 404s until the admin acquires a
// backing Team. The first Team an admin creates closes that loop: on a
// successful create, if the caller is a global admin whose current team_id backs
// no Team, RebindTeamID moves their team_id to the new Team's uid and invalidates
// their sessions so the next login mints a JWT with the correct tid.
//
// A nil rebinder disables the rebind entirely (cluster-less dev / pre-3924
// behavior): Teams are still created, admins just never rebound.
type AdminTenancyRebinder interface {
	// RebindTeamID compare-and-swaps `principal`'s team_id from `from` to `to`,
	// invalidating their sessions on a successful swap, and reports whether a row
	// was rebound. The CAS on `from` means an already-bound admin is never moved.
	RebindTeamID(ctx context.Context, principal string, from, to uuid.UUID) (bool, error)
}

// ComposeService is the 8.5 write model. It applies typed CRDs into the caller's
// Team namespace behind the membership write-tier gate, recording a provenance
// row per apply.
type ComposeService struct {
	applier CRDApplier
	roles   ProjectRoleResolver
	audit   ComposeProvenance
	// systemNS is the control-plane namespace (arch §4 / ADR-011) where Team CRs —
	// the tenancy roots — live. Every other kind is applied into the caller's own
	// squad namespace, but a Team is what MINTS that namespace, so it cannot be
	// namespaced inside it (see apply()). Overridable via $POD_NAMESPACE (downward
	// API); defaults to the ksquad-system control-plane namespace.
	systemNS string
	// rebinder closes the first-team-create tenancy loop (ISI-3924 / ADR-0009).
	// nil ⇒ rebind disabled (Teams still create; admins are never rebound).
	rebinder AdminTenancyRebinder
}

// defaultSystemNamespace is the control-plane namespace Team CRs are applied into
// when $POD_NAMESPACE is unset (dev / test), matching the operator's own default
// (cmd/operator/main.go) and the Team reconciler's SystemNamespace.
const defaultSystemNamespace = "ksquad-system"

// NewComposeService builds the compose write model. applier MUST have
// api/v1alpha1 registered on its scheme. roles is the 15.4 membership resolver
// (nil ⇒ the write-tier gate fails closed: only admins may compose). audit is the
// provenance sink (nil ⇒ provenance is logged only).
func NewComposeService(applier CRDApplier, roles ProjectRoleResolver, audit ComposeProvenance) *ComposeService {
	systemNS := os.Getenv("POD_NAMESPACE")
	if systemNS == "" {
		systemNS = defaultSystemNamespace
	}
	return &ComposeService{applier: applier, roles: roles, audit: audit, systemNS: systemNS}
}

// WithAdminRebinder installs the first-team-create tenancy rebinder (ISI-3924 /
// ADR-0009) and returns the service for chaining. It is a separate wiring step
// (not a NewComposeService param) because the rebind spans the auth DB, a
// dependency the compose write model otherwise never touches — the seam is
// opt-in and stays nil in cluster-less dev.
func (s *ComposeService) WithAdminRebinder(r AdminTenancyRebinder) *ComposeService {
	s.rebinder = r
	return s
}

// ============================================================================
// Validation — typed field checks producing field-level 422s (invariant 1)
// ============================================================================

// fieldError is one validation failure, keyed to the offending wire field.
type fieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// dns1123Label matches a valid Kubernetes object name (RFC 1123 label) — the
// name becomes metadata.name, so an invalid name is rejected here as a
// field-level 422 rather than surfacing later as an opaque apply error.
var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// validateName enforces the DNS-1123 label shape (and the 253-char ceiling) on a
// CR name, appending a field error for `field` when it fails.
func validateName(field, name string, errs []fieldError) []fieldError {
	switch {
	case name == "":
		return append(errs, fieldError{field, "is required"})
	case len(name) > 253:
		return append(errs, fieldError{field, "must be at most 253 characters"})
	case !dns1123Label.MatchString(name):
		return append(errs, fieldError{field, "must be a valid DNS-1123 label (lowercase alphanumeric or '-', starting/ending alphanumeric)"})
	}
	return errs
}

// required appends a field error when a string field is empty.
func required(field, value string, errs []fieldError) []fieldError {
	if value == "" {
		return append(errs, fieldError{field, "is required"})
	}
	return errs
}

// ============================================================================
// Wire contracts — one typed struct per kind (R6: typed shape, no raw YAML)
// ============================================================================

type teamRequest struct {
	Name              string `json:"name"`
	NamespaceStrategy string `json:"namespaceStrategy,omitempty"`
	// Agents + Projects are the squad composition member-refs (TeamSpec.Agents /
	// .Projects), surfaced read-side as name lists on TeamDetail (fleetlist.go).
	// A compose PUT is a full-spec upsert, so an edit that did not carry them
	// through would silently drop the whole composition on every Team save
	// (ISI-5360 Gap 4) — writable here, mapped back to ObjectRef{Name} so the
	// edit form round-trips the TeamDetail name lists verbatim.
	Agents   []string `json:"agents,omitempty"`
	Projects []string `json:"projects,omitempty"`
	// Grants bind capability slugs to roles in this squad's composition
	// (TeamSpec.Grants, ADR-0024a D3). Pure config — widening a grant is a Team
	// edit, never a rebuild. Surfaced read-side on TeamDetail; writable here with
	// the same no-silent-drop discipline (ISI-5360 Gap 4). The squad onboarding
	// path (composeSquad) still default-grants coordinator roles over this.
	Grants []capabilityGrantWire `json:"grants,omitempty"`
}

// capabilityGrantWire is the compose wire shape for one TeamSpec.Grants entry
// (CapabilityGrant, team_types.go). It is the exact round-trip shape TeamDetail
// projects back (fleetlist.go teamGrantsToWire), so the console's fromWire is the
// inverse of toWire. The CRD requires role (MinLength=1) and ≥1 capability
// (MinItems=1); planTeam enforces both as pre-apply field-level 422s.
type capabilityGrantWire struct {
	Role         string   `json:"role"`
	Capabilities []string `json:"capabilities"`
}

type objectRefWire struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

func (o objectRefWire) toRef() ksquadv1.ObjectRef {
	return ksquadv1.ObjectRef{Name: o.Name, Namespace: o.Namespace}
}

type secretRefWire struct {
	Name string `json:"name"`
	Key  string `json:"key,omitempty"`
}

func (s secretRefWire) toRef() ksquadv1.SecretRef {
	return ksquadv1.SecretRef{Name: s.Name, Key: s.Key}
}

// fallbackModelWire is the compose wire shape for Agent.spec.fallbackModel
// (ISI-3681 E3-S3 / AD-4): a secondary model for mid-Run switches on a
// rate_limited signal, optionally with its own BYO endpoint Secret. It mirrors
// the FallbackModel CRD type (api/v1alpha1/common_types.go): Model is required,
// the endpoint ref is optional (unset ⇒ the fallback resolves against the
// Agent's own endpoint).
type fallbackModelWire struct {
	Model            string         `json:"model"`
	ModelEndpointRef *secretRefWire `json:"modelEndpointRef,omitempty"`
}

func (f fallbackModelWire) toSpec() *ksquadv1.FallbackModel {
	fb := &ksquadv1.FallbackModel{Model: f.Model}
	if f.ModelEndpointRef != nil {
		ref := f.ModelEndpointRef.toRef()
		fb.ModelEndpointRef = &ref
	}
	return fb
}

// repoAuthWire is the compose wire shape for spec.repo.auth (ISI-3683 E4-S1 /
// AD-8, F-API-1): the per-Project BYO provider credential ref the
// connect-repo wizard captures. It maps 1:1 onto the CRD RepoAuth
// (project_types.go) — camelCase credentialSecretRef reusing secretRefWire so
// the console round-trips the same ref form the repo-auth test endpoint
// (repoauthtest.go) accepts. Optional: absent repo.auth means an anonymous /
// not-yet-connected repo, exactly like the CRD leaves spec.repo.auth nil.
type repoAuthWire struct {
	CredentialSecretRef secretRefWire `json:"credentialSecretRef"`
}

// toSpec maps the wire auth onto the CRD RepoAuth.
func (a repoAuthWire) toSpec() *ksquadv1.RepoAuth {
	return &ksquadv1.RepoAuth{CredentialSecretRef: a.CredentialSecretRef.toRef()}
}

type projectRequest struct {
	Name string `json:"name"`
	Repo struct {
		URL  string        `json:"url"`
		Ref  string        `json:"ref,omitempty"`
		Auth *repoAuthWire `json:"auth,omitempty"`
		// Sync round-trips spec.repo.sync (§5.4, ISI-4843). Nil ⇒ sync disabled.
		// The WHOLE sub-spec rides the wire. Since ISI-5359 a compose PUT is a
		// FIELD-SCOPED merge, so a save that omits `repo` leaves repo.sync intact;
		// but a save that DOES send `repo` replaces the whole repo object (top-level
		// merge), so the Settings SyncCard editing only poll/reflect must still carry
		// provider/webhookSecretRef/mirror/issueSync through untouched when it sends
		// repo, or they are dropped within that one field.
		Sync *ksquadv1.RepoSyncSpec `json:"sync,omitempty"`
	} `json:"repo"`
	Goals []string `json:"goals,omitempty"`
	// Conventions + ArchDocRefs are the NON-SECRET project descriptor fields
	// edited by the Settings "Docs & conventions" card (ISI-5303, ISI-5280 WS-E).
	// Like Goals they ride the compose PUT and are injected into every Run's context
	// envelope by the contextsource controller. Since ISI-5359 the PUT merges per
	// top-level field, so a repo/sync-only save (one that does not send conventions/
	// archDocRefs/goals) now leaves these intact — the goals full-replace footgun is
	// closed at the server.
	Conventions     string         `json:"conventions,omitempty"`
	ArchDocRefs     []string       `json:"archDocRefs,omitempty"`
	EgressPolicyRef *objectRefWire `json:"egressPolicyRef,omitempty"`
}

type agentRequest struct {
	// Project scopes the compose operation for the write-tier RBAC check (the
	// squad the Agent composes within). It is NOT written to the Agent spec — the
	// Agent CR is team-namespace-scoped; this is the membership scope only.
	Project             string          `json:"project"`
	Name                string          `json:"name"`
	RuntimeRef          objectRefWire   `json:"runtimeRef"`
	RoleRef             objectRefWire   `json:"roleRef"`
	SkillRefs           []objectRefWire `json:"skillRefs,omitempty"`
	Model               string          `json:"model"`
	ModelEndpointRef    *secretRefWire  `json:"modelEndpointRef,omitempty"`
	CredentialSecretRef secretRefWire   `json:"credentialSecretRef"`
	// CredentialClass persists the human-seat vs service-account axis onto
	// spec.credentialClass (agent_types.go:65, read by the injector resolve.go
	// and webhook validator.go). Empty ⇒ default (service-account) at injection
	// time. Persisting it is MANDATORY: an advisory-only value would leave the
	// D2 auth-mode fork inert (ISI-3681 E3-S3 AC5, R-CR1 C1).
	CredentialClass string `json:"credentialClass,omitempty"`
	// FallbackModel persists spec.fallbackModel (agent_types.go:104) — the
	// secondary model for mid-Run rate_limited recovery, mirroring the
	// modelEndpointRef optional-pointer round-trip.
	FallbackModel *fallbackModelWire `json:"fallbackModel,omitempty"`
}

type roleRequest struct {
	Project          string          `json:"project"`
	Name             string          `json:"name"`
	PromptRef        objectRefWire   `json:"promptRef"`
	DefaultSkills    []objectRefWire `json:"defaultSkills,omitempty"`
	RuntimeClassHint string          `json:"runtimeClassHint,omitempty"`
	// Model is the role-tier model applied to any agent assuming this role that
	// leaves Agent.spec.model blank (Model-Per-Role, ISI-4430; role_types.go:54).
	// Empty ⇒ the role does not pin a model and resolution falls through to the
	// system-default ModelConfig singleton. Note: RoleSpec has NO primary
	// modelEndpointRef (unlike Agent/ModelConfig), so the role's primary model
	// resolves against the inherited/provider-default endpoint — a BYO endpoint is
	// only expressible on the fallback below (ISI-4891 S2 / plan §14.1).
	Model string `json:"model,omitempty"`
	// FallbackModel persists spec.fallbackModel (role_types.go:64) — the role-tier
	// secondary model for mid-Run rate_limited recovery. Reuses fallbackModelWire
	// verbatim (its own optional modelEndpointRef is the role's ONE BYO endpoint seam).
	FallbackModel *fallbackModelWire `json:"fallbackModel,omitempty"`
	// ActivePhases / Coordinator / CoordinatorMode persist the phase-lifecycle spec
	// fields (role_types.go, ISI-4431 E3/E5). Before ISI-5358 roleRequest lacked
	// them, so a compose PUT — a full-spec REPLACE — silently wiped any phase/
	// coordinator config the edit-form could not resend (ISI-5305 §5 Gap 2). Enum
	// and coordinator-cardinality validation stays at admission (the Role/Team
	// webhooks); the wire only carries them through for a lossless round-trip.
	ActivePhases    []string `json:"activePhases,omitempty"`
	Coordinator     bool     `json:"coordinator,omitempty"`
	CoordinatorMode string   `json:"coordinatorMode,omitempty"`
	// ResourceVersion and UsedBy are READ-ONLY fields RoleDetail projects (ISI-5361
	// Gaps 6/7). The role PUT decode is strict (decodeComposeRequestStrict,
	// ISI-5358 Gap 2), so
	// the write struct must ACCEPT them for the edit-form's read→edit→write loop to
	// round-trip without a 400 — but planRole ignores both. ResourceVersion's
	// optimistic-concurrency consumption is server-side CAS on the live object
	// (upsert) / future S2 compose-PUT wiring; UsedBy is a derived blast-radius index
	// with no spec meaning. Tolerated-and-dropped, never persisted.
	ResourceVersion string  `json:"resourceVersion,omitempty"`
	UsedBy          *UsedBy `json:"usedBy,omitempty"`
}

type skillRequest struct {
	Project string `json:"project"`
	Name    string `json:"name"`
	Source  struct {
		Type   string `json:"type"`
		Inline string `json:"inline,omitempty"`
		Git    *struct {
			RepoRef string `json:"repoRef"`
			Ref     string `json:"ref"`
			Path    string `json:"path,omitempty"`
		} `json:"git,omitempty"`
	} `json:"source"`
	// McpToolRefs are the granted MCP tool endpoints (SkillSpec.McpToolRefs) —
	// part of the CRD-authorized capability envelope. SkillView already projects
	// them read-side as a name list, so an edit that did not write them back was a
	// silent drop on every skill PUT (ISI-5360 Gap 3). Mapped to ObjectRef{Name},
	// the exact inverse of the SkillView projection (objectRefNames).
	McpToolRefs []string `json:"mcpToolRefs,omitempty"`
	Permissions []string `json:"permissions,omitempty"`
	// Toolchains + Sidecars are SkillSpec.Requires (the operator's pod-assembly
	// needs, §5.3.4), surfaced flat on SkillView. Writable here with the same
	// no-silent-drop discipline so the edit form round-trips them (ISI-5360 Gap 3).
	Toolchains []string `json:"toolchains,omitempty"`
	Sidecars   []string `json:"sidecars,omitempty"`
}

// modelConfigRequest is the compose wire shape for the org-default model tier
// (ISI-4890, epic ISI-4822 Flow A). It is the floor of the Model-Per-Role
// resolution ladder (agent → role → this default, ISI-4430). Unlike every other
// compose kind it carries NO `name` or `project`: it is a fixed singleton
// (k8squad-system/default, admin-only) — planModelConfig pins the identity so the
// wire never picks the target. The three fields map 1:1 onto ModelConfigSpec and
// reuse the Agent model triple's wire types (secretRefWire / fallbackModelWire),
// so the console's ModelSelector output serializes here unchanged.
type modelConfigRequest struct {
	// Model is the system-default primary model. REQUIRED (MinLength=1 on the CRD):
	// the default tier has nothing below it to fall through to, so an empty primary
	// is a fail-closed 422 here — the ONE tier where blank is never "inherit".
	Model            string             `json:"model"`
	FallbackModel    *fallbackModelWire `json:"fallbackModel,omitempty"`
	ModelEndpointRef *secretRefWire     `json:"modelEndpointRef,omitempty"`
}

// ============================================================================
// RBAC write-tier gate (invariant 2) — reuse the 15.4 membership primitive
// ============================================================================

// writeScope is the membership project-scope the write-tier check keys on for a
// kind, plus whether the kind is admin-only. Team is the tenancy root (admin-only);
// Project scopes on its own name; Agent/Role/Skill scope on the `project` field.
type writeScope struct {
	adminOnly bool
	project   string
	// scopeIsName ⇒ the membership scope IS the object's own name (Project). It is
	// resolved in run() after the name is bound (the body's name for POST, the
	// {name} path var for PUT), so an edit checks membership on the edited Project.
	scopeIsName bool
}

// authorizeWrite applies the write-tier decision, returning the HTTP status to
// answer (0 ⇒ allowed). It mirrors internal/apiserver/rbac.go exactly:
// unauthenticated ⇒ 401; admin ⇒ allow; admin-only kind for a non-admin ⇒ 403;
// no membership ⇒ 404 (existence-hiding); role below contributor ⇒ 403; a
// resolver error ⇒ 502; a nil resolver ⇒ fail closed (only admins compose).
func (s *ComposeService) authorizeWrite(ctx context.Context, author discussion.AuthorContext, scope writeScope) (int, string) {
	if author.Principal == "" {
		return http.StatusUnauthorized, "unauthenticated"
	}
	if author.IsAdmin {
		return 0, ""
	}
	if scope.adminOnly {
		return http.StatusForbidden, "team composition is admin-only"
	}
	if s.roles == nil {
		// A mounted-but-unwired resolver must not fail open: only admins (handled
		// above) may compose when no membership backing exists.
		return http.StatusForbidden, "write-level project membership required"
	}
	if scope.project == "" {
		// A project-scoped kind with no scope is a request bug — fail closed.
		return http.StatusBadRequest, "project scope is required"
	}
	role, err := s.roles.RoleForPrincipal(ctx, author.Principal, scope.project)
	switch {
	case errors.Is(err, auth.ErrNoMembership):
		return http.StatusNotFound, "no such project"
	case err != nil:
		return http.StatusBadGateway, "authorization check unavailable"
	}
	if !writeTierGranted(role) {
		return http.StatusForbidden, "insufficient project role (write-level required)"
	}
	return 0, ""
}

// writeTierGranted is the SINGLE project write-tier threshold: a resolved role
// clears the write tier iff it ranks at least contributor (ADR-035 ordering
// viewer < contributor < maintainer). Both ComposeService.authorizeWrite (the
// PUT /api/projects/{id} gate) and canWriteProject (the settings projection's
// canEdit, ISI-3999 AC6) call it, so the read model's canEdit and the write
// path's allow/deny can never disagree on the threshold by construction.
func writeTierGranted(role string) bool {
	return auth.RoleAtLeast(role, auth.ProjectRoleContributor)
}

// ============================================================================
// Apply — team-namespace scope (invariant 3) + upsert-with-revision (4)
// ============================================================================

// ErrTeamNamespaceUnresolved is returned when the caller's Team UID resolves to
// no Team (or a Team without a reconciled namespace). The handler answers 404 —
// a caller with no Team scope has nowhere to apply.
var ErrTeamNamespaceUnresolved = errors.New("apiserver: caller team namespace unresolved")

// teamNamespace resolves the caller's Team UID to its reconciled namespace (the
// §12.1 tenancy root), the SAME resolution the dashboard read model uses. An
// unknown UID, or a Team whose namespace the reconciler has not yet stamped, is
// ErrTeamNamespaceUnresolved (404) — never a fallback to a shared namespace.
func (s *ComposeService) teamNamespace(ctx context.Context, teamUID string) (string, error) {
	if teamUID == "" {
		return "", ErrTeamNamespaceUnresolved
	}
	var teams ksquadv1.TeamList
	if err := s.applier.List(ctx, &teams); err != nil {
		return "", err
	}
	for i := range teams.Items {
		if string(teams.Items[i].UID) == teamUID && teams.Items[i].Status.Namespace != "" {
			return teams.Items[i].Status.Namespace, nil
		}
	}
	return "", ErrTeamNamespaceUnresolved
}

// composeResult is the response body for an apply.
type composeResult struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Revision  int    `json:"revision"`
	Operation string `json:"operation"` // "created" | "updated"
	// Warning carries a non-fatal post-apply advisory (ISI-3924): the CR landed,
	// but a follow-on best-effort step (the tenancy-root rebind) did not fully
	// complete. Empty ⇒ nothing to report. Surfaced so a failed rebind is never
	// silently swallowed — the admin can retry login.
	Warning string `json:"warning,omitempty"`
}

// errConflict is the sentinel a POST create returns when the CR already exists.
var errConflict = errors.New("apiserver: compose object already exists")

// create applies a fresh CR (POST). It stamps revision 1 and Creates; an existing
// name is errConflict (409) — POST is strict create, never an implicit edit.
func (s *ComposeService) create(ctx context.Context, obj client.Object) (int, error) {
	setRevision(obj, 1)
	if err := s.applier.Create(ctx, obj); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return 0, errConflict
		}
		return 0, err
	}
	return 1, nil
}

// upsert applies an edit (PUT): it Gets the existing CR by (namespace, name);
// present ⇒ a NEW revision (existing+1) via Update (never an in-place snapshot
// mutation — in-flight Runs already snapshotted); absent ⇒ Create at revision 1.
// `obj` carries the desired spec; `existing` is a fresh zero object of the same
// concrete type used to read the current revision + resourceVersion. `sent` is
// the set of request fields the caller actually provided — when non-nil the
// Update is a FIELD-SCOPED MERGE (ISI-5359), not a full-spec replace. Returns the
// new revision, whether it was an update, and any error.
func (s *ComposeService) upsert(ctx context.Context, ns, name string, obj, existing client.Object, sent map[string]json.RawMessage) (rev int, updated bool, err error) {
	getErr := s.applier.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, existing)
	switch {
	case apierrors.IsNotFound(getErr):
		r, cerr := s.create(ctx, obj)
		return r, false, cerr
	case getErr != nil:
		return 0, false, getErr
	}
	// Field-scoped merge (Gap 1): the PUT must only touch the fields the caller
	// sent. The historical behavior Updated `obj` — a freshly-built object carrying
	// ONLY the narrow wire's fields — which silently wiped every live spec field the
	// caller did not (or could not) resend. Instead merge the sent fields onto a
	// fresh copy of the live object, so unsent fields (and spec fields the wire does
	// not even model, e.g. activePhases / ownedBy) are carried through verbatim.
	// `sent == nil` ⇒ no merge: the fixed-identity ModelConfig upsert keeps its
	// deliberate full-spec replace.
	target := obj
	if sent != nil {
		merged, mErr := mergeSpecFields(existing, obj, sent)
		if mErr != nil {
			return 0, false, mErr
		}
		target = merged
	}
	next := readRevision(existing) + 1
	setRevision(target, next)
	// Carry the current resourceVersion so the Update is a compare-and-swap on the
	// live object the caller edited — a concurrent change surfaces as a conflict.
	target.SetResourceVersion(existing.GetResourceVersion())
	if err := s.applier.Update(ctx, target); err != nil {
		return 0, false, err
	}
	return next, true, nil
}

// mergeSpecFields computes the field-scoped merge of the live object `existing`
// with the caller-authored fields on `desired`, returning a fresh object of the
// same concrete type ready to Update (ISI-5359 / ISI-5305 §5 Gap 1). It is an
// RFC 7386-style JSON merge patch restricted to top-level spec fields:
//
//   - a field in `sent` whose value is present ⇒ taken from `desired` (the
//     post-validation, defaulted spec), overwriting the live value;
//   - a field in `sent` whose value is JSON null ⇒ deleted (cleared to zero);
//   - every other live spec field ⇒ carried through from `existing` untouched,
//     including fields the narrow compose wire does not model at all.
//
// Non-spec request fields (name, project) never match a spec key, so they are
// ignored. All live metadata (labels, operator-managed annotations, uid,
// resourceVersion) rides through from `existing` — the Update is a true
// read-modify-write, not a bare rebuild. The merged document is decoded into a
// zero-valued object so a null-delete genuinely clears the field rather than
// leaving a stale value from `desired`.
func mergeSpecFields(existing, desired client.Object, sent map[string]json.RawMessage) (client.Object, error) {
	spec, err := specFields(existing)
	if err != nil {
		return nil, err
	}
	authored, err := specFields(desired)
	if err != nil {
		return nil, err
	}
	for key, raw := range sent {
		switch {
		case isJSONNull(raw):
			delete(spec, key) // RFC 7386 delete; a no-op for non-spec keys (name/project)
		default:
			if v, ok := authored[key]; ok {
				spec[key] = v // overwrite with the defaulted, validated spec value
			}
			// else: a sent-but-omitempty-empty scalar, or a non-spec field — left
			// untouched (to CLEAR an optional field, send it as JSON null).
		}
	}
	// Re-encode the live object's full document with the merged spec, then decode
	// into a fresh zero of its concrete type so no stale subfield survives a delete.
	doc, err := objectDocument(existing)
	if err != nil {
		return nil, err
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	doc["spec"] = specJSON
	docJSON, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	fresh, ok := reflect.New(reflect.TypeOf(existing).Elem()).Interface().(client.Object)
	if !ok {
		return nil, fmt.Errorf("apiserver: merge target %T is not a client.Object", existing)
	}
	if err := json.Unmarshal(docJSON, fresh); err != nil {
		return nil, err
	}
	return fresh, nil
}

// specFields marshals a CR and returns its `spec` as a top-level field map (an
// empty, non-nil map for a spec-less or empty object).
func specFields(obj client.Object) (map[string]json.RawMessage, error) {
	doc, err := objectDocument(obj)
	if err != nil {
		return nil, err
	}
	fields := map[string]json.RawMessage{}
	if spec, ok := doc["spec"]; ok && len(spec) > 0 {
		if err := json.Unmarshal(spec, &fields); err != nil {
			return nil, err
		}
	}
	return fields, nil
}

// objectDocument marshals a CR to its top-level JSON field map (metadata, spec,
// status, …) so the spec can be swapped without disturbing the rest.
func objectDocument(obj client.Object) (map[string]json.RawMessage, error) {
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// isJSONNull reports whether a raw request value is the JSON literal null (the
// RFC 7386 delete sentinel).
func isJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

// readRevision parses the RevisionAnnotation off a CR (absent/garbage ⇒ 0, so the
// next apply becomes revision 1).
func readRevision(obj client.Object) int {
	if obj == nil {
		return 0
	}
	if v, ok := obj.GetAnnotations()[RevisionAnnotation]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// setRevision stamps the RevisionAnnotation on a CR (invariant 4: every apply
// carries its revision).
func setRevision(obj client.Object, rev int) {
	ann := obj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[RevisionAnnotation] = strconv.Itoa(rev)
	obj.SetAnnotations(ann)
}

// ============================================================================
// Handlers — POST (create) + PUT (edit/upsert) per kind, behind BFFAuthz
// ============================================================================

// applyPlan bundles everything a handler resolves from a decoded request: the RBAC
// scope, the concrete objects (desired + a zero for the Get), and the kind label.
type applyPlan struct {
	kind  string
	scope writeScope
	errs  []fieldError
	// fixedNamespace pins a kind to a well-known namespace, bypassing BOTH the
	// systemNS (Team) and the caller-team-namespace resolution in apply(). It
	// exists for singleton kinds whose identity is not team-scoped: ModelConfig
	// (ISI-4890) is a fixed k8squad-system/default object the resolver reads
	// (pkg/modelendpoint DefaultSystemNamespace) — the write MUST land exactly
	// where the read looks, so the target can never be a caller-derived namespace.
	// Empty ⇒ the normal Team/team-namespace resolution applies.
	fixedNamespace string
	desired        client.Object // spec/name filled; namespace set by run()
	existing       client.Object // fresh zero of the same type
	// editFields is the set of top-level request fields the caller actually sent on
	// a PUT edit (the raw JSON object keys), used by upsert() for the field-scoped
	// merge (ISI-5359 / ISI-5305 §5 Gap 1): only these fields overwrite the live
	// spec; every unsent field — INCLUDING spec fields the narrow wire does not even
	// model — is carried through from the existing CR. Nil ⇒ no merge (the pre-5359
	// full-spec-replace), so POST creates and the fixed-identity ModelConfig upsert
	// are untouched.
	editFields map[string]json.RawMessage
}

// applyOutcome is the result of applying one plan against the cluster. Exactly
// one of the shapes is populated: success (status 0, result set), a field-level
// validation failure (status 422, fields set), or a single-status failure
// (status + msg) carrying the SAME status the single-object endpoints answer.
type applyOutcome struct {
	result composeResult
	status int          // 0 ⇒ applied
	msg    string       // failure message when status != 0 and no fields
	fields []fieldError // field-level 422 when validation failed
}

// apply is the shared apply tail factored out of run(): validate → authorize →
// resolve namespace → apply (create or upsert) → provenance. `create` selects
// POST vs PUT semantics. It never writes a response, so multi-object flows
// (POST /api/compose/squad, ISI-3677) can apply N plans and report each
// outcome verbatim (NFR-5) instead of one response swallowing the rest.
func (s *ComposeService) apply(ctx context.Context, author discussion.AuthorContext, create bool, plan applyPlan) applyOutcome {
	// 1. Field validation BEFORE any cluster contact (never a partial apply).
	if len(plan.errs) > 0 {
		return applyOutcome{status: http.StatusUnprocessableEntity, fields: plan.errs}
	}
	// 2. RBAC write-tier. For a Project the membership scope IS the object name
	// (already bound: body name on POST, {name} path var on PUT).
	if plan.scope.scopeIsName {
		plan.scope.project = plan.desired.GetName()
	}
	if status, msg := s.authorizeWrite(ctx, author, plan.scope); status != 0 {
		return applyOutcome{status: status, msg: msg}
	}
	// 3. Namespace resolution. A Team IS the tenancy root (ADR-011): the Team CR
	// itself lives in the control-plane namespace and its reconciler provisions the
	// squad namespace FROM it — so a Team apply must NOT require a caller team
	// namespace. During first-run onboarding the caller has no squad namespace yet,
	// so requiring one made "Create a Team" (onboarding step 1) fail closed with a
	// 404 for every fresh tenant (ISI-3919). Team is admin-only (step 2 gate), and
	// lands in systemNS; every OTHER kind is team-scoped into the caller's own
	// namespace (a cross-tenant name is structurally a 404, existence-hiding).
	var ns string
	switch {
	case plan.fixedNamespace != "":
		// A well-known singleton (ModelConfig, ISI-4890): the target is pinned so
		// the write lands exactly where the resolver reads — never a caller ns.
		ns = plan.fixedNamespace
	case plan.kind == "Team":
		ns = s.systemNS
	default:
		resolved, err := s.teamNamespace(ctx, author.TeamID.String())
		if errors.Is(err, ErrTeamNamespaceUnresolved) {
			return applyOutcome{status: http.StatusNotFound, msg: "no team namespace for this caller"}
		}
		if err != nil {
			return applyOutcome{status: http.StatusBadGateway, msg: "team scope resolution unavailable"}
		}
		ns = resolved
	}
	name := plan.desired.GetName()
	plan.desired.SetNamespace(ns)

	// 4. Apply (create or upsert-with-revision).
	var (
		rev       int
		updated   bool
		applyErr  error
		operation = "created"
	)
	if create {
		rev, applyErr = s.create(ctx, plan.desired)
	} else {
		rev, updated, applyErr = s.upsert(ctx, ns, name, plan.desired, plan.existing, plan.editFields)
	}
	switch {
	case errors.Is(applyErr, errConflict):
		return applyOutcome{status: http.StatusConflict, msg: plan.kind + " already exists in this team"}
	case apierrors.IsConflict(applyErr):
		return applyOutcome{status: http.StatusConflict, msg: "concurrent modification; reload and retry"}
	case apierrors.IsInvalid(applyErr):
		// Admission (CEL/webhook) rejected the CR — surface as a 422, not a 502.
		return applyOutcome{status: http.StatusUnprocessableEntity, msg: applyErr.Error()}
	case applyErr != nil:
		return applyOutcome{status: http.StatusBadGateway, msg: "apply failed"}
	}
	if updated {
		operation = "updated"
	}

	// 5. Durable provenance row (who/what/when).
	s.recordProvenance(ctx, author.Principal, plan.kind, name, ns, rev, operation, plan.scope.project)

	// 6. First-team-create tenancy rebind (ISI-3924 / ADR-0009). Runs ONLY on a
	// fresh Team create by a global admin whose current team_id backs no Team —
	// the bootstrap admin's dangling tenancy root. Best-effort AFTER the CR exists;
	// a failure is logged loudly and surfaced as a result warning, never swallowed
	// (the Team is real; only re-login is deferred). See rebindAdminTenancy.
	warning := ""
	if create && plan.kind == "Team" && author.IsAdmin && s.rebinder != nil {
		warning = s.rebindAdminTenancy(ctx, author, plan.desired.GetUID())
	}

	return applyOutcome{result: composeResult{
		Kind: plan.kind, Name: name, Namespace: ns, Revision: rev, Operation: operation,
		Warning: warning,
	}}
}

// rebindAdminTenancy rebinds the creating admin's dangling tenancy root to the
// Team CR they just created (ISI-3924 / ADR-0009). It is called ONLY for a fresh
// Team create by a global admin; the remaining guardrail — that the admin's
// current team_id actually resolves to NO Team — is checked here so an
// already-bound admin creating a 2nd Team is never moved (that would be a
// cross-tenant hijack). Returns a non-empty warning to surface on the response
// when the rebind was attempted but did not fully succeed; "" otherwise.
func (s *ComposeService) rebindAdminTenancy(ctx context.Context, author discussion.AuthorContext, newUID types.UID) string {
	// Guardrail: rebind ONLY a dangling root. If the caller's team_id already
	// resolves to a Team (bound tenant), or resolution is merely unavailable, do
	// NOT rebind — a bound admin's 2nd Team create is a normal scoped create.
	if _, err := s.teamNamespace(ctx, author.TeamID.String()); !errors.Is(err, ErrTeamNamespaceUnresolved) {
		return ""
	}
	newTeamID, perr := uuid.Parse(string(newUID))
	if perr != nil || newTeamID == uuid.Nil {
		msg := "tenancy rebind skipped: created Team has no server-assigned uid"
		slog.ErrorContext(ctx, msg, "principal", author.Principal, "uid", string(newUID),
			"detail", "re-login will still 404 until remediated")
		return msg
	}
	rebound, err := s.rebinder.RebindTeamID(ctx, author.Principal, author.TeamID, newTeamID)
	if err != nil {
		msg := "tenancy rebind FAILED; re-login may still 404 until retried"
		slog.ErrorContext(ctx, msg, "principal", author.Principal,
			"from", author.TeamID.String(), "to", newTeamID.String(), "error", err)
		return msg
	}
	if rebound {
		slog.InfoContext(ctx, "tenancy root rebound; sessions invalidated, re-login required to pick up the new tid",
			"principal", author.Principal, "from", author.TeamID.String(), "to", newTeamID.String())
	}
	return ""
}

// run is the shared handler tail: authenticate, then apply via apply() and
// render the outcome. `create` selects POST vs PUT.
func (s *ComposeService) run(w http.ResponseWriter, r *http.Request, create bool, plan applyPlan) {
	author, ok := discussion.AuthFromContext(r.Context())
	if !ok || author.Principal == "" {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	out := s.apply(r.Context(), author, create, plan)
	switch {
	case out.status == 0:
		code := http.StatusCreated
		if out.result.Operation == "updated" {
			code = http.StatusOK
		}
		writeJSON(w, code, out.result)
	case len(out.fields) > 0:
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "validation failed",
			"fields": out.fields,
		})
	default:
		writeJSONError(w, out.status, out.msg)
	}
}

// recordProvenance appends the §6.5 apply event on a context DETACHED from the
// request (a client disconnect right after the apply must not cancel the row that
// records it), matching the admin-mutation audit discipline.
func (s *ComposeService) recordProvenance(ctx context.Context, principal, kind, name, ns string, rev int, op, project string) {
	detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	payload := map[string]any{
		"kind": kind, "name": name, "namespace": ns, "revision": rev, "operation": op,
	}
	if project != "" {
		payload["project"] = project
	}
	if s.audit != nil {
		s.audit(detached, "crd_applied", principal, payload)
	}
}

// ── Team ──────────────────────────────────────────────────────────────────────

func (s *ComposeService) planTeam(req teamRequest) applyPlan {
	var errs []fieldError
	errs = validateName("name", req.Name, errs)
	strategy := req.NamespaceStrategy
	if strategy == "" {
		strategy = "perTeam" // the compose default; the reconciler owns the semantics (§12.1)
	}
	spec := ksquadv1.TeamSpec{NamespaceStrategy: strategy}
	for _, n := range req.Agents {
		spec.Agents = append(spec.Agents, ksquadv1.ObjectRef{Name: n})
	}
	for _, n := range req.Projects {
		spec.Projects = append(spec.Projects, ksquadv1.ObjectRef{Name: n})
	}
	// Grants: fail-closed validation mirroring the CRD (role non-empty, ≥1
	// capability) so an invalid grant is a clean pre-apply 422, never a partial
	// apply or an opaque admission reject (ISI-5360 Gap 4, no-silent-drop).
	for i, g := range req.Grants {
		errs = required(fmt.Sprintf("grants[%d].role", i), g.Role, errs)
		if len(g.Capabilities) == 0 {
			errs = append(errs, fieldError{fmt.Sprintf("grants[%d].capabilities", i), "must list at least one capability"})
		}
		spec.Grants = append(spec.Grants, ksquadv1.CapabilityGrant{Role: g.Role, Capabilities: g.Capabilities})
	}
	team := &ksquadv1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: req.Name},
		Spec:       spec,
	}
	return applyPlan{
		kind:  "Team",
		scope: writeScope{adminOnly: true},
		errs:  errs, desired: team, existing: &ksquadv1.Team{},
	}
}

// ── Project ─────────────────────────────────────────────────────────────────

func (s *ComposeService) planProject(req projectRequest) applyPlan {
	var errs []fieldError
	errs = validateName("name", req.Name, errs)
	errs = required("repo.url", req.Repo.URL, errs)
	spec := ksquadv1.ProjectSpec{
		Repo:        ksquadv1.RepoSpec{URL: req.Repo.URL, Ref: req.Repo.Ref},
		Goals:       req.Goals,
		Conventions: req.Conventions,
		ArchDocRefs: req.ArchDocRefs,
	}
	if req.Repo.Auth != nil {
		// AD-8: repo.auth capture — the ref must name a Secret (the wizard's
		// PAT-paste stored through the E3-S1 credential surface) before it can
		// ride the Project spec; an empty name is a field-level 422, mirroring
		// agentRequest.credentialSecretRef.
		errs = required("repo.auth.credentialSecretRef.name", req.Repo.Auth.CredentialSecretRef.Name, errs)
		spec.Repo.Auth = req.Repo.Auth.toSpec()
	}
	if req.Repo.Sync != nil {
		// Round-trip the whole sync sub-spec; only default the required provider
		// (enum-validated by admission) so an enable-sync write from a tab that
		// doesn't render a provider picker still yields a valid object. Poll
		// interval min/default are enforced by the CRD (Minimum=60, default=300).
		sync := *req.Repo.Sync
		if sync.Provider == "" {
			sync.Provider = defaultRepoProvider
		}
		spec.Repo.Sync = &sync
	}
	if req.EgressPolicyRef != nil {
		ref := req.EgressPolicyRef.toRef()
		spec.EgressPolicyRef = &ref
	}
	project := &ksquadv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: req.Name},
		Spec:       spec,
	}
	return applyPlan{
		kind:  "Project",
		scope: writeScope{scopeIsName: true}, // write-tier on the project's own name (bound in run)
		errs:  errs, desired: project, existing: &ksquadv1.Project{},
	}
}

// ── Agent ─────────────────────────────────────────────────────────────────────

func (s *ComposeService) planAgent(req agentRequest) applyPlan {
	var errs []fieldError
	errs = validateName("name", req.Name, errs)
	errs = required("runtimeRef.name", req.RuntimeRef.Name, errs)
	errs = required("roleRef.name", req.RoleRef.Name, errs)
	errs = required("credentialSecretRef.name", req.CredentialSecretRef.Name, errs)
	errs = required("model", req.Model, errs)
	spec := ksquadv1.AgentSpec{
		RuntimeRef:          req.RuntimeRef.toRef(),
		RoleRef:             req.RoleRef.toRef(),
		CredentialSecretRef: req.CredentialSecretRef.toRef(),
		CredentialClass:     req.CredentialClass,
		Model:               req.Model,
	}
	for _, sr := range req.SkillRefs {
		spec.SkillRefs = append(spec.SkillRefs, sr.toRef())
	}
	if req.ModelEndpointRef != nil {
		ref := req.ModelEndpointRef.toRef()
		spec.ModelEndpointRef = &ref
	}
	if req.FallbackModel != nil {
		spec.FallbackModel = req.FallbackModel.toSpec()
	}
	agent := &ksquadv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: req.Name},
		Spec:       spec,
	}
	return applyPlan{
		kind:  "Agent",
		scope: writeScope{project: req.Project},
		errs:  errs, desired: agent, existing: &ksquadv1.Agent{},
	}
}

// ── Role ──────────────────────────────────────────────────────────────────────

func (s *ComposeService) planRole(req roleRequest) applyPlan {
	var errs []fieldError
	errs = validateName("name", req.Name, errs)
	errs = required("promptRef.name", req.PromptRef.Name, errs)
	if req.RuntimeClassHint != "" {
		switch req.RuntimeClassHint {
		case "gvisor", "kata", "runc":
		default:
			errs = append(errs, fieldError{"runtimeClassHint", "must be one of gvisor, kata, runc"})
		}
	}
	spec := ksquadv1.RoleSpec{
		PromptRef:        req.PromptRef.toRef(),
		RuntimeClassHint: req.RuntimeClassHint,
		Model:            req.Model,
		ActivePhases:     req.ActivePhases,
		Coordinator:      req.Coordinator,
		CoordinatorMode:  req.CoordinatorMode,
	}
	for _, ds := range req.DefaultSkills {
		spec.DefaultSkills = append(spec.DefaultSkills, ds.toRef())
	}
	// Role-tier fallback rides the shared fallbackModelWire (its optional
	// modelEndpointRef is the role's only BYO-endpoint seam; the primary has none).
	if req.FallbackModel != nil {
		spec.FallbackModel = req.FallbackModel.toSpec()
	}
	role := &ksquadv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: req.Name},
		Spec:       spec,
	}
	return applyPlan{
		kind:  "Role",
		scope: writeScope{project: req.Project},
		errs:  errs, desired: role, existing: &ksquadv1.Role{},
	}
}

// ── Skill ─────────────────────────────────────────────────────────────────────

func (s *ComposeService) planSkill(req skillRequest) applyPlan {
	var errs []fieldError
	errs = validateName("name", req.Name, errs)
	switch req.Source.Type {
	case string(ksquadv1.SkillSourceInline):
		errs = required("source.inline", req.Source.Inline, errs)
		if len(req.Source.Inline) > ksquadv1.MaxInlineSkillBodySize {
			errs = append(errs, fieldError{
				Field:   "source.inline",
				Message: fmt.Sprintf("must be at most %d characters", ksquadv1.MaxInlineSkillBodySize),
			})
		}
		if req.Source.Git != nil {
			errs = append(errs, fieldError{"source.git", "must be unset when source.type is inline"})
		}
	case string(ksquadv1.SkillSourceGit):
		if req.Source.Git == nil {
			errs = append(errs, fieldError{"source.git", "is required when source.type is git"})
		} else {
			errs = required("source.git.repoRef", req.Source.Git.RepoRef, errs)
			errs = required("source.git.ref", req.Source.Git.Ref, errs)
		}
		if req.Source.Inline != "" {
			errs = append(errs, fieldError{"source.inline", "must be unset when source.type is git"})
		}
	default:
		errs = append(errs, fieldError{"source.type", "must be one of inline, git"})
	}
	spec := ksquadv1.SkillSpec{
		Source:      ksquadv1.SkillSource{Type: ksquadv1.SkillSourceType(req.Source.Type), Inline: req.Source.Inline},
		Permissions: req.Permissions,
		Requires: ksquadv1.SkillRequires{
			Toolchains: req.Toolchains,
			Sidecars:   req.Sidecars,
		},
	}
	for _, n := range req.McpToolRefs {
		spec.McpToolRefs = append(spec.McpToolRefs, ksquadv1.ObjectRef{Name: n})
	}
	if req.Source.Git != nil {
		spec.Source.Git = &ksquadv1.GitSkillSource{
			RepoRef: req.Source.Git.RepoRef,
			Ref:     req.Source.Git.Ref,
			Path:    req.Source.Git.Path,
		}
	}
	skill := &ksquadv1.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: req.Name},
		Spec:       spec,
	}
	return applyPlan{
		kind:     "Skill",
		scope:    writeScope{project: req.Project},
		existing: &ksquadv1.Skill{}, errs: errs, desired: skill,
	}
}

// ── ModelConfig (org default tier, ISI-4890) ────────────────────────────────────

// planModelConfig maps the org-default model request onto a fixed-identity
// ModelConfig apply. It is the ONE compose kind that is NOT team-scoped: the
// target is pinned to the well-known singleton the resolver reads —
// modelendpoint.DefaultSystemNamespace / DefaultModelConfigName (k8squad-system/
// default) — via applyPlan.fixedNamespace, so the team-namespace resolver is
// deliberately NOT run (any {name} path var is ignored by handleModelConfig too).
// It is admin-only (writeScope.adminOnly → authorizeWrite 403/401/fail-closed,
// the same gate Teams use — no new RBAC). `required("model")` is the fail-closed
// guardrail: an empty primary is a clean pre-apply 422, with the CRD's
// Required,MinLength=1 as the admission backstop (belt + suspenders, ISI-4430 D3).
func (s *ComposeService) planModelConfig(req modelConfigRequest) applyPlan {
	var errs []fieldError
	errs = required("model", req.Model, errs)
	spec := ksquadv1.ModelConfigSpec{Model: req.Model}
	if req.FallbackModel != nil {
		spec.FallbackModel = req.FallbackModel.toSpec()
	}
	if req.ModelEndpointRef != nil {
		ref := req.ModelEndpointRef.toRef()
		spec.ModelEndpointRef = &ref
	}
	mc := &ksquadv1.ModelConfig{
		ObjectMeta: metav1.ObjectMeta{Name: modelendpoint.DefaultModelConfigName},
		Spec:       spec,
	}
	return applyPlan{
		kind:           "ModelConfig",
		scope:          writeScope{adminOnly: true},
		fixedNamespace: modelendpoint.DefaultSystemNamespace,
		errs:           errs, desired: mc, existing: &ksquadv1.ModelConfig{},
	}
}

// modelConfigToWire maps a ModelConfig CR back onto the modelConfigRequest wire
// shape (the exact inverse of planModelConfig) so the console's fromWire hydrates
// the Model Priority form on load. Only the three authored fields ride back.
func modelConfigToWire(mc *ksquadv1.ModelConfig) modelConfigRequest {
	out := modelConfigRequest{Model: mc.Spec.Model}
	if fb := mc.Spec.FallbackModel; fb != nil {
		w := &fallbackModelWire{Model: fb.Model}
		if fb.ModelEndpointRef != nil {
			w.ModelEndpointRef = &secretRefWire{Name: fb.ModelEndpointRef.Name, Key: fb.ModelEndpointRef.Key}
		}
		out.FallbackModel = w
	}
	if ref := mc.Spec.ModelEndpointRef; ref != nil {
		out.ModelEndpointRef = &secretRefWire{Name: ref.Name, Key: ref.Key}
	}
	return out
}

// ============================================================================
// Route handlers — thin decode shells over run()
// ============================================================================

// decodeComposeRequest reads the request body ONCE and decodes it into BOTH the
// typed wire struct `v` and a raw top-level field map (ISI-5359). The raw map is
// the set of fields the caller actually sent, which the PUT edit path threads
// into the field-scoped merge so an unsent field is never wiped. The route caps
// the body (maxBytesBody, server.go §13), so io.ReadAll is bounded. On a
// malformed body it writes the 400 (like decodeJSON) and returns ok=false. A
// `null` / empty body yields an empty (non-nil) map — an edit that touches
// nothing — never a nil map that would fall back to a full-spec replace.
func decodeComposeRequest(w http.ResponseWriter, r *http.Request, v any) (map[string]json.RawMessage, bool) {
	return decodeComposeBody(w, r, v, false)
}

// decodeComposeRequestStrict is decodeComposeRequest with the strict typed
// decode (ISI-5358 Gap 2, role path): DisallowUnknownFields turns a would-be
// silent drop into a loud 400, so a client sending a field the roleRequest wire
// does not model learns of it instead of losing live config. The sent-fields
// map is still captured — strict and merge semantics compose: unknown fields
// are rejected, known-but-unsent ones are merged from the live object.
func decodeComposeRequestStrict(w http.ResponseWriter, r *http.Request, v any) (map[string]json.RawMessage, bool) {
	return decodeComposeBody(w, r, v, true)
}

func decodeComposeBody(w http.ResponseWriter, r *http.Request, v any, strict bool) (map[string]json.RawMessage, bool) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "unable to read request body")
		return nil, false
	}
	if strict {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(v); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return nil, false
		}
	} else if err := json.Unmarshal(body, v); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return nil, false
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		// A top-level JSON array/scalar cannot carry named compose fields; the typed
		// decode above only accepts an object for these struct targets, so this is a
		// belt-and-suspenders guard rather than a reachable path.
		writeJSONError(w, http.StatusBadRequest, "request body must be a JSON object")
		return nil, false
	}
	if raw == nil { // body was the literal `null`
		raw = map[string]json.RawMessage{}
	}
	return raw, true
}

func (s *ComposeService) handleTeam(create bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req teamRequest
		sent, ok := decodeComposeRequest(w, r, &req)
		if !ok {
			return
		}
		s.applyEdit(w, r, create, s.planTeam(req), sent)
	}
}

func (s *ComposeService) handleProject(create bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req projectRequest
		sent, ok := decodeComposeRequest(w, r, &req)
		if !ok {
			return
		}
		s.applyEdit(w, r, create, s.planProject(req), sent)
	}
}

func (s *ComposeService) handleAgent(create bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req agentRequest
		sent, ok := decodeComposeRequest(w, r, &req)
		if !ok {
			return
		}
		s.applyEdit(w, r, create, s.planAgent(req), sent)
	}
}

func (s *ComposeService) handleRole(create bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req roleRequest
		// Strict decode (ISI-5358 Gap 2): an unknown field is a loud 400 rather
		// than a silent drop, so the model-per-role UI can never quietly wipe
		// phase/coordinator config it failed to re-send on a full-spec PUT. The
		// sent map (ISI-5359) still threads into the field-scoped merge, so
		// known-but-unsent fields are merged from the live object.
		sent, ok := decodeComposeRequestStrict(w, r, &req)
		if !ok {
			return
		}
		s.applyEdit(w, r, create, s.planRole(req), sent)
	}
}

func (s *ComposeService) handleSkill(create bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req skillRequest
		sent, ok := decodeComposeRequest(w, r, &req)
		if !ok {
			return
		}
		s.applyEdit(w, r, create, s.planSkill(req), sent)
	}
}

// handleModelConfig writes the org-default model singleton (ISI-4890). It always
// UPSERTs (run with create=false): the Settings Save is idempotent create-or-edit
// of the one well-known object, so a second Save revises rather than 409ing. It
// deliberately does NOT call applyEdit — the identity is fixed in planModelConfig
// and must never be retargeted by a {name} path var, so any path name is ignored.
// Bound to BOTH the POST (collection) and PUT ({name}) compose routes; both are
// the same upsert against k8squad-system/default.
func (s *ComposeService) handleModelConfig() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req modelConfigRequest
		if err := decodeJSON(w, r, &req); err != nil {
			return
		}
		s.run(w, r, false, s.planModelConfig(req))
	}
}

// handleModelConfigGet hydrates the Settings→Configuration Model Priority section
// (ISI-4890 AC1): GET /api/modelconfig returns the org-default ModelConfig/default
// mapped onto the write wire shape (so the console's fromWire is the exact inverse
// of toWire). 404 ⇒ no default configured yet (the empty-form state, mirroring the
// OTLP surface's opt-in 404). Admin-only, matching the adminOnly write scope so the
// read and the write agree on who may see it; a resolver 404 never leaks existence.
func (s *ComposeService) handleModelConfigGet() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		author, ok := discussion.AuthFromContext(r.Context())
		if !ok || author.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		if !author.IsAdmin {
			writeJSONError(w, http.StatusForbidden, "the org default model is admin-only")
			return
		}
		var mc ksquadv1.ModelConfig
		key := client.ObjectKey{
			Namespace: modelendpoint.DefaultSystemNamespace,
			Name:      modelendpoint.DefaultModelConfigName,
		}
		if err := s.applier.Get(r.Context(), key, &mc); err != nil {
			if apierrors.IsNotFound(err) {
				writeJSONError(w, http.StatusNotFound, "no org default model configured")
				return
			}
			writeJSONError(w, http.StatusBadGateway, "model config read unavailable")
			return
		}
		writeJSON(w, http.StatusOK, modelConfigToWire(&mc))
	}
}

// applyEdit binds the {name} path var (PUT) to the desired object's name so an
// edit targets the path identity, then delegates to run(). For POST the name is
// the body's own name (already set by the plan). `sent` is the raw set of fields
// the caller provided; on an edit it rides onto the plan so upsert() performs the
// field-scoped merge (ISI-5359) — it is ignored for a create.
func (s *ComposeService) applyEdit(w http.ResponseWriter, r *http.Request, create bool, plan applyPlan, sent map[string]json.RawMessage) {
	if !create {
		if id, ok := pathVar(r, "name"); ok && id != "" {
			plan.desired.SetName(id)
		}
		plan.editFields = sent
	}
	s.run(w, r, create, plan)
}

// ============================================================================
// Delete — DELETE /api/{kind}/{name} (ISI-4107, gap 2 of ISI-4106)
// ============================================================================
//
// Delete removes a compose CR the user created by mistake. It reuses EVERY seam
// the edit path uses — write-tier RBAC (authorizeWrite), team-namespace scoping
// (a cross-tenant name is structurally a 404, existence-hiding), and a durable
// provenance row — so allow/deny can never disagree with edit by construction.
// Only Agent and Project are wired here: Team delete cascades the whole squad
// namespace (agents/projects/roles/skills/running work) and is deferred pending
// the board teardown decision (ISI-4107 open decision #1). Roles/skills are out
// of scope for this story.

// deletePlan is the delete analog of applyPlan: the RBAC scope plus a fresh zero
// object of the target's concrete type (used for the existence Get and the
// Delete). The {name} identity is bound from the path in deleteObj.
type deletePlan struct {
	kind  string
	scope writeScope
	obj   client.Object
}

// deleteObj is the shared delete tail: authenticate → resolve scope/RBAC →
// resolve the caller's team namespace → existence-check inside that namespace
// (missing or cross-tenant ⇒ 404, mirroring the edit read's existence-hiding) →
// Delete → best-effort provenance. Returns 204 No Content on success.
func (s *ComposeService) deleteObj(w http.ResponseWriter, r *http.Request, plan deletePlan) {
	author, ok := discussion.AuthFromContext(r.Context())
	if !ok || author.Principal == "" {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	name, _ := pathVar(r, "name")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "name is required")
		return
	}
	// A Project's membership scope IS its own name (mirror edit's scopeIsName).
	if plan.scope.scopeIsName {
		plan.scope.project = name
	}
	if status, msg := s.authorizeWrite(r.Context(), author, plan.scope); status != 0 {
		writeJSONError(w, status, msg)
		return
	}
	// Every deletable kind here is team-scoped into the caller's own namespace
	// (Team, the only systemNS kind, is not wired). Cross-tenant names 404 below.
	ns, err := s.teamNamespace(r.Context(), author.TeamID.String())
	if errors.Is(err, ErrTeamNamespaceUnresolved) {
		writeJSONError(w, http.StatusNotFound, "no team namespace for this caller")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "team scope resolution unavailable")
		return
	}
	// Existence check inside the caller's namespace before Delete: a missing name,
	// or one that lives in another tenant's namespace, is a 404 either way.
	if getErr := s.applier.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, plan.obj); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			writeJSONError(w, http.StatusNotFound, "no such "+plan.kind)
			return
		}
		writeJSONError(w, http.StatusBadGateway, "lookup failed")
		return
	}
	// ponytail: straight CR delete — the operator garbage-collects the Agent/Project's
	// child resources on removal. Blocking the delete while Runs are still in flight
	// (ISI-4107 open decision #3) is a deliberate follow-up, not wired here.
	plan.obj.SetNamespace(ns)
	plan.obj.SetName(name)
	if delErr := s.applier.Delete(r.Context(), plan.obj); delErr != nil {
		if apierrors.IsNotFound(delErr) {
			writeJSONError(w, http.StatusNotFound, "no such "+plan.kind)
			return
		}
		writeJSONError(w, http.StatusBadGateway, "delete failed")
		return
	}
	// Durable provenance (who/what/when). Revision 0 ⇒ the object no longer exists.
	s.recordProvenance(r.Context(), author.Principal, plan.kind, name, ns, 0, "deleted", plan.scope.project)
	w.WriteHeader(http.StatusNoContent)
}

// handleProjectDelete deletes a Project CR by path name; the project's own name
// is its write-tier membership scope (scopeIsName), mirroring the edit gate.
func (s *ComposeService) handleProjectDelete() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.deleteObj(w, r, deletePlan{kind: "Project", scope: writeScope{scopeIsName: true}, obj: &ksquadv1.Project{}})
	}
}

// handleAgentDelete deletes an Agent CR by path name. Agent RBAC is project-scoped
// (same as edit); the delete carries the project scope via ?project= (edit carries
// it in the body). Admins compose fleet-wide and need no project scope.
func (s *ComposeService) handleAgentDelete() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.deleteObj(w, r, deletePlan{kind: "Agent", scope: writeScope{project: r.URL.Query().Get("project")}, obj: &ksquadv1.Agent{}})
	}
}

// ============================================================================
// Squad materialize (ISI-3677, AD-3) — POST /api/compose/squad
// ============================================================================
//
// One authorized call turns a chosen template into a Team (if absent) + N
// Agents, each Agent referencing a seeded Role preset (E2-S1: role-boss /
// role-implementer / role-manager) and the ONE shared credentialSecretRef
// (AD-5). The handler composes the existing planTeam/planAgent planners and
// the apply() tail — no new CR-write primitive.
//
// Conventions (AC3): same write middleware as the rest of the compose surface
// (authz + sameOriginGuard + maxBytesBody(64<<10), mounted in server.go),
// provenance server-stamped per applied object, tenancy resolved from
// AuthorContext.TeamID (cross-tenant is a 404), JSON via writeJSON/writeJSONError.
// Partial failures are reported VERBATIM (AC4, NFR-5): a 403/409/422 on one
// object is surfaced per-object in `errors`, never masked, and the response is
// 207 Multi-Status.

// squadRequest is the typed wire contract for POST /api/compose/squad (R6:
// typed shape, no raw YAML).
type squadRequest struct {
	// Template selects the agent set: minimal-trio (D1 default) | bmad | solo.
	Template string `json:"template"`
	// Team, when present, is created first if absent (an existing Team of the
	// same name is reported as "existing", not an error).
	Team *teamRequest `json:"team,omitempty"`
	// Project is the write-tier membership scope for the Agents (the same
	// `project` field the single-Agent compose carries). Required for
	// non-admin callers; admins compose fleet-wide.
	Project string `json:"project,omitempty"`
	// RuntimeRef applies to every Agent (default: claude-code).
	RuntimeRef objectRefWire `json:"runtimeRef,omitempty"`
	// CredentialSecretRef is the ONE shared credential across all agents
	// (AD-5; default: model-credentials/token, matching examples/bmad-team).
	CredentialSecretRef secretRefWire `json:"credentialSecretRef,omitempty"`
	// Models optionally overrides the per-preset default model
	// (boss/implementer/manager). The console supplies these from the
	// presets table (console/lib/presets.ts, FR-6.1); absent ⇒ the
	// server-side defaults below.
	Models map[string]string `json:"models,omitempty"`
}

// squadObjectError is one object's verbatim failure inside a 207 response.
type squadObjectError struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Status int    `json:"status"` // the HTTP status the single-object endpoint would answer
	Error  string `json:"error"`
}

// squadResponse is the materialize result: what was created, what failed.
type squadResponse struct {
	Team   *composeResult     `json:"team,omitempty"`
	Agents []composeResult    `json:"agents"`
	Errors []squadObjectError `json:"errors,omitempty"`
}

// squadAgentTemplate is one agent in a template: its CR name and the seeded
// Role preset it references (role-<preset>).
type squadAgentTemplate struct {
	Name   string
	Preset string // boss | implementer | manager
}

const (
	// squadRolePrefix turns a preset key into the seeded Role CR name (E2-S1).
	squadRolePrefix = "role-"
	// Defaults matching examples/bmad-team so the endpoint works standalone.
	defaultSquadRuntime          = "claude-code"
	defaultSquadCredentialSecret = "model-credentials" // #nosec G101 -- a Secret NAME (metadata), not a credential value.
	defaultSquadCredentialKey    = "token"
)

// defaultSquadModels is the server-side fallback for the console's preset →
// default-model table (console/lib/presets.ts, FR-6.1): Boss → Opus-5,
// Implementer/Manager → Sonnet-5.
var defaultSquadModels = map[string]string{
	"boss":        "claude-opus-5",
	"implementer": "claude-sonnet-5",
	"manager":     "claude-sonnet-5",
}

// squadTemplates is the template-set mapping (AC2). Minimal Trio is the D1
// default; Solo is Boss+Impl; BMAD is the examples/bmad-team persona set,
// each persona mapped onto the nearest seeded Role preset.
var squadTemplates = map[string][]squadAgentTemplate{
	"minimal-trio": {
		{Name: "boss", Preset: "boss"},
		{Name: "implementer", Preset: "implementer"},
		{Name: "manager", Preset: "manager"},
	},
	"solo": {
		{Name: "boss", Preset: "boss"},
		{Name: "implementer", Preset: "implementer"},
	},
	"bmad": {
		{Name: "sam", Preset: "boss"},        // ceo
		{Name: "john", Preset: "manager"},    // product-manager
		{Name: "winston", Preset: "boss"},    // architect (planning)
		{Name: "uma", Preset: "implementer"}, // ux-designer
		{Name: "mary", Preset: "manager"},    // brainstormer (analysis)
		{Name: "cade", Preset: "manager"},    // challenger (review)
		{Name: "quill", Preset: "implementer"},
		{Name: "amelia", Preset: "manager"}, // code-reviewer
		{Name: "tess", Preset: "implementer"},
		{Name: "ada", Preset: "implementer"}, // coder
	},
}

// coordinatorPresets is the set of seeded Role presets that are COORDINATORS
// (Role.Spec.Coordinator=true — config/roles/role-manager.yaml). Only the
// manager preset drives the squad's lifecycle. Kept as a static table (like
// defaultSquadModels) because compose/squad materializes Agents referencing
// role-<preset> without reading the seeded Role CRs; the source of truth is the
// seeded manifests, mirrored here.
var coordinatorPresets = map[string]bool{
	"manager": true,
}

// coordinatorGrants returns the default capability grants for a template's
// coordinator roles (ISI-5223, orchestration MVP): every coordinator preset in
// the roster is granted work_item.author on the Team, so a PM/coordinator agent
// assigned a ticket can decompose it into sub-tickets and hand them off through
// the ADR-0024 authoring lane (work_item_create / work_item_assign) instead of
// hitting "capability denied". Non-coordinator roles are omitted — deny-by-
// default is preserved for them (ADR-0024a).
//
// This is a DELIBERATE, auditable relaxation of ADR-0024's deny-by-default
// posture for coordinator roles only: it is written into Team.Spec.Grants (pure
// config, not code), so the board can inspect it on the Team CR and veto it with
// a one-line edit. Result is sorted+deduped for stable bytes across composes.
func coordinatorGrants(agents []squadAgentTemplate) []ksquadv1.CapabilityGrant {
	seen := map[string]bool{}
	var roles []string
	for _, a := range agents {
		if !coordinatorPresets[a.Preset] {
			continue
		}
		role := squadRolePrefix + a.Preset
		if seen[role] {
			continue
		}
		seen[role] = true
		roles = append(roles, role)
	}
	if len(roles) == 0 {
		return nil
	}
	sort.Strings(roles)
	grants := make([]ksquadv1.CapabilityGrant, 0, len(roles))
	for _, role := range roles {
		grants = append(grants, ksquadv1.CapabilityGrant{
			Role:         role,
			Capabilities: []string{capability.CapabilityWorkItemAuthor},
		})
	}
	return grants
}

// handleComposeSquad materializes a squad from a template in one authorized
// transaction. Validation of EVERY planned object happens before any apply
// (invariant 1); applies then proceed object-by-object (Team first, then
// Agents) with per-object outcomes reported verbatim.
func (s *ComposeService) handleComposeSquad(w http.ResponseWriter, r *http.Request) {
	// Funnel span + metric (ISI-3669, obs plan §3/§4): the M2 squad-materialize
	// step. team.id is span-only; template is the bounded preset enum.
	ctx, span := funnelSpan(r.Context(), "ksquad.compose.squad")
	defer span.End()
	author, ok := discussion.AuthFromContext(r.Context())
	if !ok || author.Principal == "" {
		funnelOutcome(ctx, span, funnelInst().composeSquad, outcomeUnauthenticated)
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	funnelAttrs(span, attribute.String("team.id", author.TeamID.String()))
	var req squadRequest
	if err := decodeJSON(w, r, &req); err != nil {
		funnelOutcome(ctx, span, funnelInst().composeSquad, outcomeSquadInvalid)
		return
	}

	agents, ok := squadTemplates[req.Template]
	if !ok {
		funnelOutcome(ctx, span, funnelInst().composeSquad, outcomeSquadInvalid)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "validation failed",
			"fields": []fieldError{{"template", "must be one of minimal-trio, bmad, solo"}},
		})
		return
	}
	funnelAttrs(span, attribute.String("compose.template", req.Template))

	// Wire defaults (AD-5 shared credential; claude-code runtime).
	if req.RuntimeRef.Name == "" {
		req.RuntimeRef.Name = defaultSquadRuntime
	}
	if req.CredentialSecretRef.Name == "" {
		req.CredentialSecretRef.Name = defaultSquadCredentialSecret
	}
	if req.CredentialSecretRef.Key == "" {
		req.CredentialSecretRef.Key = defaultSquadCredentialKey
	}

	// Request-level tenancy check (AC3): a caller whose Team UID resolves to no
	// namespace has nowhere to apply — 404 before touching anything, exactly
	// like the single-kind compose endpoints.
	if _, err := s.teamNamespace(r.Context(), author.TeamID.String()); errors.Is(err, ErrTeamNamespaceUnresolved) {
		funnelOutcome(ctx, span, funnelInst().composeSquad, outcomeSquadNoNamespace)
		writeJSONError(w, http.StatusNotFound, "no team namespace for this caller")
		return
	} else if err != nil {
		slog.WarnContext(ctx, "team scope resolution unavailable", "error", err)
		funnelOutcome(ctx, span, funnelInst().composeSquad, outcomeSquadNoNamespace)
		writeJSONError(w, http.StatusBadGateway, "team scope resolution unavailable")
		return
	}

	// 1. Field validation across EVERY planned object BEFORE any apply
	//    (invariant 1 — never a partial-validation apply).
	var fields []fieldError
	var teamPlan applyPlan
	if req.Team != nil {
		teamPlan = s.planTeam(*req.Team)
		// ISI-5223: default-grant work_item.author to the template's coordinator
		// roles on the Team it creates, so a coordinator agent can orchestrate
		// (decompose + delegate) out of the box. Written to Team.Spec.Grants —
		// visible + vetoable config. Only applies when this call creates the Team;
		// an existing Team keeps its grants (a one-line Team edit adds the grant).
		if grants := coordinatorGrants(agents); len(grants) > 0 {
			if team, ok := teamPlan.desired.(*ksquadv1.Team); ok {
				team.Spec.Grants = grants
			}
		}
		for _, fe := range teamPlan.errs {
			fields = append(fields, fieldError{"team." + fe.Field, fe.Message})
		}
	}
	if !author.IsAdmin && req.Project == "" {
		fields = append(fields, fieldError{"project", "is required (the write-tier membership scope for the squad's agents)"})
	}
	plans := make([]applyPlan, 0, len(agents))
	for _, a := range agents {
		model := req.Models[a.Preset]
		if model == "" {
			model = defaultSquadModels[a.Preset]
		}
		plan := s.planAgent(agentRequest{
			Project:             req.Project,
			Name:                a.Name,
			RuntimeRef:          req.RuntimeRef,
			RoleRef:             objectRefWire{Name: squadRolePrefix + a.Preset},
			Model:               model,
			CredentialSecretRef: req.CredentialSecretRef,
		})
		for _, fe := range plan.errs {
			fields = append(fields, fieldError{"agents[" + a.Name + "]." + fe.Field, fe.Message})
		}
		plans = append(plans, plan)
	}
	if len(fields) > 0 {
		funnelOutcome(ctx, span, funnelInst().composeSquad, outcomeSquadInvalid)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "validation failed",
			"fields": fields,
		})
		return
	}
	funnelAttrs(span,
		attribute.Int("compose.agents_planned", len(agents)),
		attribute.Bool("compose.team_requested", req.Team != nil),
	)

	// 2. Apply: Team first (if requested), then each Agent — composing the
	//    existing planners + apply() tail (AD-3). Failures are verbatim (AC4).
	resp := squadResponse{Agents: []composeResult{}}
	if req.Team != nil {
		out := s.apply(r.Context(), author, true, teamPlan)
		switch out.status {
		case 0:
			team := out.result
			resp.Team = &team
		case http.StatusConflict:
			// "creates a Team (if absent)" — an existing Team is not a failure.
			resp.Team = &composeResult{Kind: "Team", Name: req.Team.Name, Operation: "existing"}
		default:
			resp.Errors = append(resp.Errors, squadObjectError{Kind: "Team", Name: req.Team.Name, Status: out.status, Error: out.msg})
		}
	}
	for i, plan := range plans {
		out := s.apply(r.Context(), author, true, plan)
		if out.status == 0 {
			resp.Agents = append(resp.Agents, out.result)
			continue
		}
		resp.Errors = append(resp.Errors, squadObjectError{Kind: "Agent", Name: agents[i].Name, Status: out.status, Error: out.msg})
	}

	code := http.StatusCreated
	outcome := outcomeSquadCreated
	if len(resp.Errors) > 0 {
		code = http.StatusMultiStatus // 207 — partial materialize; errors carried verbatim
		outcome = outcomeSquadPartial
	}
	funnelAttrs(span, attribute.Int("compose.agents_applied", len(resp.Agents)))
	funnelInst().composeAgents.Record(ctx, int64(len(resp.Agents)))
	slog.InfoContext(ctx, "squad materialize complete",
		"template", req.Template, "outcome", outcome,
		"agents_planned", len(agents), "agents_applied", len(resp.Agents))
	funnelOutcome(ctx, span, funnelInst().composeSquad, outcome)
	writeJSON(w, code, resp)
}
