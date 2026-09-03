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
//     404 (existence-hiding), never a 200.
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
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/auth"
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
}

// ComposeProvenance is the append-only provenance sink (§6.5 / 2.6): who applied
// what, when. It is the SAME shape as AuthRoutesOptions.Audit so main.go wires one
// coord.audit_log writer for both. Best-effort by design (the apply has already
// landed); a failed append is logged loudly, never a silent drop.
type ComposeProvenance func(ctx context.Context, eventType, principal string, payload map[string]any)

// ComposeService is the 8.5 write model. It applies typed CRDs into the caller's
// Team namespace behind the membership write-tier gate, recording a provenance
// row per apply.
type ComposeService struct {
	applier CRDApplier
	roles   ProjectRoleResolver
	audit   ComposeProvenance
}

// NewComposeService builds the compose write model. applier MUST have
// api/v1alpha1 registered on its scheme. roles is the 15.4 membership resolver
// (nil ⇒ the write-tier gate fails closed: only admins may compose). audit is the
// provenance sink (nil ⇒ provenance is logged only).
func NewComposeService(applier CRDApplier, roles ProjectRoleResolver, audit ComposeProvenance) *ComposeService {
	return &ComposeService{applier: applier, roles: roles, audit: audit}
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

type projectRequest struct {
	Name string `json:"name"`
	Repo struct {
		URL string `json:"url"`
		Ref string `json:"ref,omitempty"`
	} `json:"repo"`
	Goals           []string       `json:"goals,omitempty"`
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
	Permissions []string `json:"permissions,omitempty"`
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
	if !auth.RoleAtLeast(role, auth.ProjectRoleContributor) {
		return http.StatusForbidden, "insufficient project role (write-level required)"
	}
	return 0, ""
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
// concrete type used to read the current revision + resourceVersion. Returns the
// new revision, whether it was an update, and any error.
func (s *ComposeService) upsert(ctx context.Context, ns, name string, obj, existing client.Object) (rev int, updated bool, err error) {
	getErr := s.applier.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, existing)
	switch {
	case apierrors.IsNotFound(getErr):
		r, cerr := s.create(ctx, obj)
		return r, false, cerr
	case getErr != nil:
		return 0, false, getErr
	}
	next := readRevision(existing) + 1
	setRevision(obj, next)
	// Carry the current resourceVersion so the Update is a compare-and-swap on the
	// live object the caller edited — a concurrent change surfaces as a conflict.
	obj.SetResourceVersion(existing.GetResourceVersion())
	if err := s.applier.Update(ctx, obj); err != nil {
		return 0, false, err
	}
	return next, true, nil
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
	kind     string
	scope    writeScope
	errs     []fieldError
	desired  client.Object // spec/name filled; namespace set by run()
	existing client.Object // fresh zero of the same type
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
	// 3. Team-namespace scope (cross-tenant is structurally a 404).
	ns, err := s.teamNamespace(ctx, author.TeamID.String())
	if errors.Is(err, ErrTeamNamespaceUnresolved) {
		return applyOutcome{status: http.StatusNotFound, msg: "no team namespace for this caller"}
	}
	if err != nil {
		return applyOutcome{status: http.StatusBadGateway, msg: "team scope resolution unavailable"}
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
		rev, updated, applyErr = s.upsert(ctx, ns, name, plan.desired, plan.existing)
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

	return applyOutcome{result: composeResult{
		Kind: plan.kind, Name: name, Namespace: ns, Revision: rev, Operation: operation,
	}}
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
	team := &ksquadv1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: req.Name},
		Spec:       ksquadv1.TeamSpec{NamespaceStrategy: strategy},
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
		Repo:  ksquadv1.RepoSpec{URL: req.Repo.URL, Ref: req.Repo.Ref},
		Goals: req.Goals,
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
	}
	for _, ds := range req.DefaultSkills {
		spec.DefaultSkills = append(spec.DefaultSkills, ds.toRef())
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

// ============================================================================
// Route handlers — thin decode shells over run()
// ============================================================================

func (s *ComposeService) handleTeam(create bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req teamRequest
		if err := decodeJSON(w, r, &req); err != nil {
			return
		}
		s.applyEdit(w, r, create, s.planTeam(req))
	}
}

func (s *ComposeService) handleProject(create bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req projectRequest
		if err := decodeJSON(w, r, &req); err != nil {
			return
		}
		s.applyEdit(w, r, create, s.planProject(req))
	}
}

func (s *ComposeService) handleAgent(create bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req agentRequest
		if err := decodeJSON(w, r, &req); err != nil {
			return
		}
		s.applyEdit(w, r, create, s.planAgent(req))
	}
}

func (s *ComposeService) handleRole(create bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req roleRequest
		if err := decodeJSON(w, r, &req); err != nil {
			return
		}
		s.applyEdit(w, r, create, s.planRole(req))
	}
}

func (s *ComposeService) handleSkill(create bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req skillRequest
		if err := decodeJSON(w, r, &req); err != nil {
			return
		}
		s.applyEdit(w, r, create, s.planSkill(req))
	}
}

// applyEdit binds the {name} path var (PUT) to the desired object's name so an
// edit targets the path identity, then delegates to run(). For POST the name is
// the body's own name (already set by the plan).
func (s *ComposeService) applyEdit(w http.ResponseWriter, r *http.Request, create bool, plan applyPlan) {
	if !create {
		if id, ok := pathVar(r, "name"); ok && id != "" {
			plan.desired.SetName(id)
		}
	}
	s.run(w, r, create, plan)
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

// handleComposeSquad materializes a squad from a template in one authorized
// transaction. Validation of EVERY planned object happens before any apply
// (invariant 1); applies then proceed object-by-object (Team first, then
// Agents) with per-object outcomes reported verbatim.
func (s *ComposeService) handleComposeSquad(w http.ResponseWriter, r *http.Request) {
	author, ok := discussion.AuthFromContext(r.Context())
	if !ok || author.Principal == "" {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	var req squadRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	agents, ok := squadTemplates[req.Template]
	if !ok {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "validation failed",
			"fields": []fieldError{{"template", "must be one of minimal-trio, bmad, solo"}},
		})
		return
	}

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
		writeJSONError(w, http.StatusNotFound, "no team namespace for this caller")
		return
	} else if err != nil {
		writeJSONError(w, http.StatusBadGateway, "team scope resolution unavailable")
		return
	}

	// 1. Field validation across EVERY planned object BEFORE any apply
	//    (invariant 1 — never a partial-validation apply).
	var fields []fieldError
	var teamPlan applyPlan
	if req.Team != nil {
		teamPlan = s.planTeam(*req.Team)
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
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "validation failed",
			"fields": fields,
		})
		return
	}

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
	if len(resp.Errors) > 0 {
		code = http.StatusMultiStatus // 207 — partial materialize; errors carried verbatim
	}
	writeJSON(w, code, resp)
}
