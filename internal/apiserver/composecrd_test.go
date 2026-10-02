package apiserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/auth"
	"github.com/K8squad/K8squad/pkg/capability"
)

// ── test harness ────────────────────────────────────────────────────────────

const (
	teamNS   = "team-acme" // the caller's resolved Team namespace
	teamUID  = "11111111-1111-1111-1111-111111111111"
	otherNS  = "team-globex" // a foreign tenant's namespace
	otherUID = "22222222-2222-2222-2222-222222222222"
)

// newComposeFixture builds a ComposeService over a fake client seeded with two
// Teams (the caller's + a foreign tenant) and a map-backed membership resolver.
// It returns the service and a captured-provenance slice pointer.
func newComposeFixture(t *testing.T, roles map[string]map[string]string, seed ...client.Object) (*ComposeService, *[]map[string]any) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := ksquadv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	objs := []client.Object{
		&ksquadv1.Team{
			ObjectMeta: metav1.ObjectMeta{Name: "acme", UID: teamUID},
			Status:     ksquadv1.TeamStatus{Namespace: teamNS},
		},
		&ksquadv1.Team{
			ObjectMeta: metav1.ObjectMeta{Name: "globex", UID: otherUID},
			Status:     ksquadv1.TeamStatus{Namespace: otherNS},
		},
	}
	objs = append(objs, seed...)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

	var captured []map[string]any
	sink := func(_ context.Context, eventType, principal string, payload map[string]any) {
		row := map[string]any{"eventType": eventType, "principal": principal}
		for k, v := range payload {
			row[k] = v
		}
		captured = append(captured, row)
	}
	return NewComposeService(c, fakeRoleResolver{roles: roles}, sink), &captured
}

// caller builds an AuthorContext with a fixed Team UID scope.
func caller(principal, teamUID string, admin bool) discussion.AuthorContext {
	return discussion.AuthorContext{
		Principal: principal,
		TeamID:    uuid.MustParse(teamUID),
		IsAdmin:   admin,
	}
}

// do drives a handler with a JSON body and an AuthorContext already on the
// context (BFFAuthz is upstream), returning the recorded response.
func do(h http.HandlerFunc, method, target string, author discussion.AuthorContext, body any, vars map[string]string) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(method, target, bytes.NewReader(b))
	r = r.WithContext(discussion.WithAuth(r.Context(), author))
	if vars != nil {
		r = mux.SetURLVars(r, vars)
	}
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

// grant is a convenience for the roles map: principal → project → role.
func grant(principal, project, role string) map[string]map[string]string {
	return map[string]map[string]string{principal: {project: role}}
}

// validProject is a minimal happy-path Project body.
func validProject(name string) projectRequest {
	req := projectRequest{Name: name}
	req.Repo.URL = "https://github.com/acme/widget"
	return req
}

// ── happy-path create (invariant 4, DoD) ─────────────────────────────────────

func TestComposeCreateProject_HappyPath(t *testing.T) {
	svc, prov := newComposeFixture(t, grant("alice", "widget", auth.ProjectRoleMaintainer))
	w := do(svc.handleProject(true), http.MethodPost, "/api/projects",
		caller("alice", teamUID, false), validProject("widget"), nil)

	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body.String())
	}
	var res composeResult
	mustJSON(t, w, &res)
	if res.Revision != 1 || res.Operation != "created" || res.Namespace != teamNS {
		t.Fatalf("unexpected result: %+v", res)
	}
	// The CR landed in the caller's namespace with revision 1.
	var got ksquadv1.Project
	if err := svc.applier.Get(context.Background(), client.ObjectKey{Namespace: teamNS, Name: "widget"}, &got); err != nil {
		t.Fatalf("project not applied: %v", err)
	}
	if got.Annotations[RevisionAnnotation] != "1" {
		t.Fatalf("want revision annotation 1, got %q", got.Annotations[RevisionAnnotation])
	}
	// Provenance row recorded (invariant 5).
	if len(*prov) != 1 || (*prov)[0]["eventType"] != "crd_applied" || (*prov)[0]["operation"] != "created" {
		t.Fatalf("want one crd_applied/created provenance row, got %+v", *prov)
	}
}

// ── repo.auth capture (ISI-3683 E4-S1 / AD-8, F-API-1) ──────────────────────

// TestComposeCreateProject_RepoAuth — an optional repo.auth.credentialSecretRef
// rides the wire onto spec.repo.auth (the onboarding "project" milestone's
// derived signal), defaulting the data key exactly like the wire ref states.
func TestComposeCreateProject_RepoAuth(t *testing.T) {
	svc, _ := newComposeFixture(t, grant("alice", "widget", auth.ProjectRoleMaintainer))
	req := validProject("widget")
	req.Repo.Auth = &repoAuthWire{CredentialSecretRef: secretRefWire{Name: "alpha-repo-pat", Key: "token"}}
	w := do(svc.handleProject(true), http.MethodPost, "/api/projects",
		caller("alice", teamUID, false), req, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body.String())
	}

	var got ksquadv1.Project
	if err := svc.applier.Get(context.Background(), client.ObjectKey{Namespace: teamNS, Name: "widget"}, &got); err != nil {
		t.Fatalf("project not applied: %v", err)
	}
	auth := got.Spec.Repo.Auth
	if auth == nil {
		t.Fatal("spec.repo.auth not mapped from the wire")
	}
	if auth.CredentialSecretRef.Name != "alpha-repo-pat" || auth.CredentialSecretRef.Key != "token" {
		t.Fatalf("auth ref mapped wrong: %+v", auth.CredentialSecretRef)
	}
}

// TestComposeCreateProject_RepoAuthEmptyRefFailsClosed — a present repo.auth
// whose credentialSecretRef names nothing is a field-level 422 (mirrors
// agentRequest.credentialSecretRef), never a Project carrying an unusable ref.
func TestComposeCreateProject_RepoAuthEmptyRefFailsClosed(t *testing.T) {
	svc, _ := newComposeFixture(t, grant("alice", "widget", auth.ProjectRoleMaintainer))
	req := validProject("widget")
	req.Repo.Auth = &repoAuthWire{}
	w := do(svc.handleProject(true), http.MethodPost, "/api/projects",
		caller("alice", teamUID, false), req, nil)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "repo.auth.credentialSecretRef.name") {
		t.Fatalf("422 must name the field: %s", w.Body.String())
	}
}

// TestComposeCreateProject_RepoSync — a repo.sync sub-spec rides the wire onto
// spec.repo.sync (ISI-4843 S5), defaulting only the required provider so an
// enable-sync write from the Settings SyncCard (no provider picker) is valid; the
// opaque passthrough (webhookSecretRef) round-trips untouched.
func TestComposeCreateProject_RepoSync(t *testing.T) {
	svc, _ := newComposeFixture(t, grant("alice", "widget", auth.ProjectRoleMaintainer))
	req := validProject("widget")
	req.Repo.Sync = &ksquadv1.RepoSyncSpec{
		PollIntervalSeconds: 600,
		ReflectOutbound:     true,
		WebhookSecretRef:    &ksquadv1.SecretRef{Name: "widget-hook"},
	}
	w := do(svc.handleProject(true), http.MethodPost, "/api/projects",
		caller("alice", teamUID, false), req, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body.String())
	}
	var got ksquadv1.Project
	if err := svc.applier.Get(context.Background(), client.ObjectKey{Namespace: teamNS, Name: "widget"}, &got); err != nil {
		t.Fatalf("project not applied: %v", err)
	}
	sync := got.Spec.Repo.Sync
	if sync == nil {
		t.Fatal("spec.repo.sync not mapped from the wire")
	}
	if sync.Provider != defaultRepoProvider {
		t.Fatalf("provider must default to %q, got %q", defaultRepoProvider, sync.Provider)
	}
	if sync.PollIntervalSeconds != 600 || !sync.ReflectOutbound {
		t.Fatalf("poll/reflect mapped wrong: %+v", sync)
	}
	if sync.WebhookSecretRef == nil || sync.WebhookSecretRef.Name != "widget-hook" {
		t.Fatalf("webhookSecretRef passthrough dropped: %+v", sync.WebhookSecretRef)
	}
}

// ── viewer → 403 (invariant 2, DoD) ──────────────────────────────────────────

func TestComposeViewerForbidden(t *testing.T) {
	svc, _ := newComposeFixture(t, grant("val", "widget", auth.ProjectRoleViewer),
		&ksquadv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "widget", Namespace: teamNS}})
	w := do(svc.handleProject(false), http.MethodPut, "/api/projects/widget",
		caller("val", teamUID, false), validProject("widget"), map[string]string{"name": "widget"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("viewer must get 403, got %d: %s", w.Code, w.Body.String())
	}
}

// ── cross-tenant → 404 (invariant 3, DoD) ────────────────────────────────────

func TestComposeCrossTenantNotFound(t *testing.T) {
	// A Project owned by the FOREIGN tenant exists; the caller (acme) edits it by
	// name. Because applies are scoped to the caller's namespace, the object is
	// invisible → 404, never a 200 that would leak or clobber cross-tenant state.
	svc, _ := newComposeFixture(t, grant("alice", "secret", auth.ProjectRoleMaintainer),
		&ksquadv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "secret", Namespace: otherNS}})
	// Caller is admin so RBAC passes; the 404 must come from namespace scoping,
	// proving tenant isolation independently of the RBAC gate.
	w := do(svc.handleProject(false), http.MethodPut, "/api/projects/secret",
		caller("root", teamUID, true), validProject("secret"), map[string]string{"name": "secret"})
	if w.Code != http.StatusCreated {
		// Edit of a name absent in the caller's namespace upserts a NEW CR in the
		// caller's namespace (revision 1) — the foreign object is untouched.
		t.Fatalf("want 201 (new CR in caller ns), got %d: %s", w.Code, w.Body.String())
	}
	// The foreign tenant's object is unchanged (still no revision annotation).
	var foreign ksquadv1.Project
	if err := svc.applier.Get(context.Background(), client.ObjectKey{Namespace: otherNS, Name: "secret"}, &foreign); err != nil {
		t.Fatalf("foreign project vanished: %v", err)
	}
	if _, ok := foreign.Annotations[RevisionAnnotation]; ok {
		t.Fatalf("cross-tenant object was mutated: %+v", foreign.Annotations)
	}
	// And a NEW object exists in the caller's namespace.
	var mine ksquadv1.Project
	if err := svc.applier.Get(context.Background(), client.ObjectKey{Namespace: teamNS, Name: "secret"}, &mine); err != nil {
		t.Fatalf("caller-namespace project not created: %v", err)
	}
}

// ── invalid CR → field-level 422 (invariant 1, DoD) ──────────────────────────

func TestComposeInvalidField422(t *testing.T) {
	svc, prov := newComposeFixture(t, grant("alice", "widget", auth.ProjectRoleMaintainer))
	// Missing repo.url and an invalid (uppercase) name.
	bad := projectRequest{Name: "Widget_BAD"}
	w := do(svc.handleProject(true), http.MethodPost, "/api/projects",
		caller("alice", teamUID, false), bad, nil)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Error  string       `json:"error"`
		Fields []fieldError `json:"fields"`
	}
	mustJSON(t, w, &body)
	if len(body.Fields) < 2 {
		t.Fatalf("want field-level errors for name + repo.url, got %+v", body.Fields)
	}
	// Nothing was applied and no provenance was written (never a partial apply).
	if len(*prov) != 0 {
		t.Fatalf("invalid request must not provenance an apply: %+v", *prov)
	}
	var got ksquadv1.ProjectList
	_ = svc.applier.List(context.Background(), &got, client.InNamespace(teamNS))
	if len(got.Items) != 0 {
		t.Fatalf("invalid request must not create a CR, found %d", len(got.Items))
	}
}

// ── docs descriptor fields map onto spec (ISI-5303 / ISI-5280 WS-E) ──────────

// TestComposeProjectDocs — conventions + archDocRefs ride the compose wire onto
// spec.Conventions / spec.ArchDocRefs (the "Docs & conventions" settings card
// write). They are non-secret descriptor fields injected into every Run context.
func TestComposeProjectDocs(t *testing.T) {
	svc, _ := newComposeFixture(t, grant("alice", "widget", auth.ProjectRoleMaintainer))
	req := validProject("widget")
	req.Conventions = "squash-merge only; tabs not spaces"
	req.ArchDocRefs = []string{"docs/architecture.md", "https://wiki/adr-7"}
	w := do(svc.handleProject(true), http.MethodPost, "/api/projects",
		caller("alice", teamUID, false), req, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body.String())
	}
	var got ksquadv1.Project
	if err := svc.applier.Get(context.Background(), client.ObjectKey{Namespace: teamNS, Name: "widget"}, &got); err != nil {
		t.Fatalf("project not applied: %v", err)
	}
	if got.Spec.Conventions != "squash-merge only; tabs not spaces" {
		t.Fatalf("conventions not mapped: %q", got.Spec.Conventions)
	}
	if len(got.Spec.ArchDocRefs) != 2 || got.Spec.ArchDocRefs[0] != "docs/architecture.md" {
		t.Fatalf("archDocRefs not mapped: %+v", got.Spec.ArchDocRefs)
	}
}

// ── edit makes a new revision (§6.4, DoD) ────────────────────────────────────

func TestComposeEditMakesNewRevision(t *testing.T) {
	svc, prov := newComposeFixture(t, grant("alice", "widget", auth.ProjectRoleMaintainer))
	// Create (revision 1).
	if w := do(svc.handleProject(true), http.MethodPost, "/api/projects",
		caller("alice", teamUID, false), validProject("widget"), nil); w.Code != http.StatusCreated {
		t.Fatalf("seed create failed: %d %s", w.Code, w.Body.String())
	}
	// Edit with new goals → revision 2, operation "updated".
	edit := validProject("widget")
	edit.Goals = []string{"ship v2"}
	w := do(svc.handleProject(false), http.MethodPut, "/api/projects/widget",
		caller("alice", teamUID, false), edit, map[string]string{"name": "widget"})
	if w.Code != http.StatusOK {
		t.Fatalf("edit want 200, got %d: %s", w.Code, w.Body.String())
	}
	var res composeResult
	mustJSON(t, w, &res)
	if res.Revision != 2 || res.Operation != "updated" {
		t.Fatalf("edit must be revision 2/updated, got %+v", res)
	}
	// The live CR shows the new revision AND the new spec (goals) — the running
	// snapshot model means in-flight Runs are untouched, but the CR itself advances.
	var got ksquadv1.Project
	if err := svc.applier.Get(context.Background(), client.ObjectKey{Namespace: teamNS, Name: "widget"}, &got); err != nil {
		t.Fatalf("get after edit: %v", err)
	}
	if got.Annotations[RevisionAnnotation] != "2" || len(got.Spec.Goals) != 1 {
		t.Fatalf("edit not applied: rev=%q goals=%v", got.Annotations[RevisionAnnotation], got.Spec.Goals)
	}
	if len(*prov) != 2 || (*prov)[1]["operation"] != "updated" || (*prov)[1]["revision"] != 2 {
		t.Fatalf("want create+update provenance rows, got %+v", *prov)
	}
}

// ── idempotent re-apply = new revision each time (invariant 4, DoD) ──────────

func TestComposeIdempotentReapply(t *testing.T) {
	svc, _ := newComposeFixture(t, grant("alice", "widget", auth.ProjectRoleMaintainer))
	body := validProject("widget")
	vars := map[string]string{"name": "widget"}

	// Three PUTs of the SAME body: no duplicate object (idempotent identity), but
	// each apply is a new revision (1 → 2 → 3).
	for i, wantRev := range []int{1, 2, 3} {
		w := do(svc.handleProject(false), http.MethodPut, "/api/projects/widget",
			caller("alice", teamUID, false), body, vars)
		if w.Code != http.StatusCreated && w.Code != http.StatusOK {
			t.Fatalf("apply %d failed: %d %s", i, w.Code, w.Body.String())
		}
		var res composeResult
		mustJSON(t, w, &res)
		if res.Revision != wantRev {
			t.Fatalf("apply %d want revision %d, got %d", i, wantRev, res.Revision)
		}
	}
	// Exactly one object exists in the namespace (idempotent by (kind, team, name)).
	var got ksquadv1.ProjectList
	if err := svc.applier.List(context.Background(), &got, client.InNamespace(teamNS)); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("re-apply must not duplicate; found %d projects", len(got.Items))
	}
}

// ── Team is admin-only (tenancy-root gate) ───────────────────────────────────

func TestComposeTeamAdminOnly(t *testing.T) {
	svc, _ := newComposeFixture(t, grant("alice", "widget", auth.ProjectRoleMaintainer))
	// Even a maintainer cannot compose a Team.
	w := do(svc.handleTeam(true), http.MethodPost, "/api/teams",
		caller("alice", teamUID, false), teamRequest{Name: "newsquad"}, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin Team compose must be 403, got %d: %s", w.Code, w.Body.String())
	}
	// An admin succeeds.
	w = do(svc.handleTeam(true), http.MethodPost, "/api/teams",
		caller("root", teamUID, true), teamRequest{Name: "newsquad"}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("admin Team compose must be 201, got %d: %s", w.Code, w.Body.String())
	}
}

// ── first-run onboarding: creating the FIRST Team must not require a caller
//
//	namespace (ISI-3919 regression) ───────────────────────────────────────────
//
// A Team is the tenancy root that MINTS the squad namespace, so it lands in the
// control-plane namespace (systemNS) and must never require a pre-existing caller
// namespace. Before the fix, apply() resolved the caller's Team namespace for
// EVERY kind including Team — so a fresh tenant (whose UID resolves to no Team
// with a reconciled namespace) got a 404, breaking onboarding step 1.
func TestComposeTeamFirstRunNoCallerNamespace(t *testing.T) {
	// Admin caller whose Team UID is NOT among the seeded Teams ⇒ teamNamespace()
	// would return ErrTeamNamespaceUnresolved. The first Team create must still 201.
	const freshUID = "99999999-9999-9999-9999-999999999999"
	svc, _ := newComposeFixture(t, nil)
	w := do(svc.handleTeam(true), http.MethodPost, "/api/teams",
		caller("root", freshUID, true), teamRequest{Name: "isitobservable"}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("first-run Team compose must be 201 (ISI-3919), got %d: %s", w.Code, w.Body.String())
	}
	// The Team CR lands in the control-plane namespace, not a per-team namespace.
	var got ksquadv1.Team
	if err := svc.applier.Get(context.Background(),
		client.ObjectKey{Namespace: defaultSystemNamespace, Name: "isitobservable"}, &got); err != nil {
		t.Fatalf("first Team not applied into %s: %v", defaultSystemNamespace, err)
	}
	if got.Spec.NamespaceStrategy != "perTeam" {
		t.Fatalf("default namespaceStrategy want perTeam, got %q", got.Spec.NamespaceStrategy)
	}
}

// ── contributor may compose an Agent scoped to a project they can write ──────

func TestComposeAgentContributorAllowed(t *testing.T) {
	svc, _ := newComposeFixture(t, grant("bob", "widget", auth.ProjectRoleContributor))
	req := agentRequest{
		Project: "widget", Name: "backend-dev", Model: "claude-opus-4-8",
	}
	req.RuntimeRef = objectRefWire{Name: "claude-code"}
	req.RoleRef = objectRefWire{Name: "engineer"}
	req.CredentialSecretRef = secretRefWire{Name: "bob-claude"}
	w := do(svc.handleAgent(true), http.MethodPost, "/api/agents",
		caller("bob", teamUID, false), req, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("contributor Agent compose want 201, got %d: %s", w.Code, w.Body.String())
	}
	var got ksquadv1.Agent
	if err := svc.applier.Get(context.Background(), client.ObjectKey{Namespace: teamNS, Name: "backend-dev"}, &got); err != nil {
		t.Fatalf("agent not applied: %v", err)
	}
	if got.Spec.Model != "claude-opus-4-8" {
		t.Fatalf("agent spec not carried: %+v", got.Spec)
	}
}

// ── credentialClass + fallbackModel persist onto the Agent spec (ISI-3681 E3-S3 AC5, R-CR1 C1) ──
//
// Both fields must round-trip through agentRequest → planAgent onto Agent.spec, mirroring the
// modelEndpointRef path exactly. credentialClass persist is MANDATORY (the injector/webhook read
// it); fallbackModel carries its optional own-endpoint ref.
func TestComposeAgentPersistsCredentialClassAndFallback(t *testing.T) {
	svc, _ := newComposeFixture(t, grant("bob", "widget", auth.ProjectRoleContributor))
	req := agentRequest{
		Project: "widget", Name: "backend-dev", Model: "claude-opus-4-8",
		CredentialClass: "human-seat",
		FallbackModel: &fallbackModelWire{
			Model:            "claude-haiku-4-5",
			ModelEndpointRef: &secretRefWire{Name: "fb-endpoint", Key: "url"},
		},
	}
	req.RuntimeRef = objectRefWire{Name: "claude-code"}
	req.RoleRef = objectRefWire{Name: "engineer"}
	req.CredentialSecretRef = secretRefWire{Name: "bob-claude"}
	w := do(svc.handleAgent(true), http.MethodPost, "/api/agents",
		caller("bob", teamUID, false), req, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("compose want 201, got %d: %s", w.Code, w.Body.String())
	}
	var got ksquadv1.Agent
	if err := svc.applier.Get(context.Background(), client.ObjectKey{Namespace: teamNS, Name: "backend-dev"}, &got); err != nil {
		t.Fatalf("agent not applied: %v", err)
	}
	if got.Spec.CredentialClass != "human-seat" {
		t.Fatalf("credentialClass not persisted: %q", got.Spec.CredentialClass)
	}
	if got.Spec.FallbackModel == nil || got.Spec.FallbackModel.Model != "claude-haiku-4-5" {
		t.Fatalf("fallbackModel not persisted: %+v", got.Spec.FallbackModel)
	}
	if got.Spec.FallbackModel.ModelEndpointRef == nil ||
		got.Spec.FallbackModel.ModelEndpointRef.Name != "fb-endpoint" ||
		got.Spec.FallbackModel.ModelEndpointRef.Key != "url" {
		t.Fatalf("fallbackModel endpoint ref not persisted: %+v", got.Spec.FallbackModel.ModelEndpointRef)
	}
}

// ── role-tier model + fallback persist onto the Role spec (ISI-4891 S2, Flow B) ──
//
// Model-Per-Role (ISI-4430) resolves Agent→Role→ModelConfig; S2 lets Compose→Roles author the
// role tier. `model` + `fallbackModel` must round-trip through roleRequest → planRole onto
// Role.spec, reusing the shared fallbackModelWire. RoleSpec has NO primary modelEndpointRef, so
// the role's only BYO seam is the fallback's own endpoint ref (plan §14.1).
func TestComposeRolePersistsModelAndFallback(t *testing.T) {
	svc, _ := newComposeFixture(t, grant("bob", "widget", auth.ProjectRoleContributor))
	req := roleRequest{
		Project:   "widget",
		Name:      "engineer",
		PromptRef: objectRefWire{Name: "engineer-prompt"},
		Model:     "claude-opus-4-8",
		FallbackModel: &fallbackModelWire{
			Model:            "claude-haiku-4-5",
			ModelEndpointRef: &secretRefWire{Name: "fb-endpoint", Key: "url"},
		},
	}
	w := do(svc.handleRole(true), http.MethodPost, "/api/roles",
		caller("bob", teamUID, false), req, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("compose want 201, got %d: %s", w.Code, w.Body.String())
	}
	var got ksquadv1.Role
	if err := svc.applier.Get(context.Background(), client.ObjectKey{Namespace: teamNS, Name: "engineer"}, &got); err != nil {
		t.Fatalf("role not applied: %v", err)
	}
	if got.Spec.Model != "claude-opus-4-8" {
		t.Fatalf("role model not persisted: %q", got.Spec.Model)
	}
	if got.Spec.FallbackModel == nil || got.Spec.FallbackModel.Model != "claude-haiku-4-5" {
		t.Fatalf("role fallbackModel not persisted: %+v", got.Spec.FallbackModel)
	}
	if got.Spec.FallbackModel.ModelEndpointRef == nil ||
		got.Spec.FallbackModel.ModelEndpointRef.Name != "fb-endpoint" ||
		got.Spec.FallbackModel.ModelEndpointRef.Key != "url" {
		t.Fatalf("role fallback endpoint ref not persisted: %+v", got.Spec.FallbackModel.ModelEndpointRef)
	}
}

// ── a Role composed with phase + coordinator config persists it (ISI-5358 Gap 2) ───────────────
//
// Before ISI-5358 roleRequest/planRole lacked activePhases/coordinator/coordinatorMode, so a
// compose PUT silently discarded them. They must now ride the wire onto Role.spec.
func TestComposeRolePersistsPhaseAndCoordinator(t *testing.T) {
	svc, _ := newComposeFixture(t, grant("bob", "widget", auth.ProjectRoleContributor))
	req := roleRequest{
		Project:         "widget",
		Name:            "lead",
		PromptRef:       objectRefWire{Name: "lead-prompt"},
		ActivePhases:    []string{"implementation", "code_review"},
		Coordinator:     true,
		CoordinatorMode: "propose",
	}
	w := do(svc.handleRole(true), http.MethodPost, "/api/roles",
		caller("bob", teamUID, false), req, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("compose want 201, got %d: %s", w.Code, w.Body.String())
	}
	var got ksquadv1.Role
	if err := svc.applier.Get(context.Background(), client.ObjectKey{Namespace: teamNS, Name: "lead"}, &got); err != nil {
		t.Fatalf("role not applied: %v", err)
	}
	if !reflect.DeepEqual(got.Spec.ActivePhases, []string{"implementation", "code_review"}) {
		t.Fatalf("activePhases not persisted: %+v", got.Spec.ActivePhases)
	}
	if !got.Spec.Coordinator || got.Spec.CoordinatorMode != "propose" {
		t.Fatalf("coordinator config not persisted: coordinator=%v mode=%q", got.Spec.Coordinator, got.Spec.CoordinatorMode)
	}
}

// ── the headline round-trip: read → edit → write loses NOTHING (ISI-5358 AC) ────────────────────
//
// A Role carrying all five Model-Per-Role / phase-lifecycle fields must survive the full
// edit-form loop: Role.spec → roleDetail read projection → JSON (what the UI holds) →
// roleRequest decode → planRole → Role.spec. The projected spec must equal the original, proving
// an inline edit that re-sends the read payload unchanged drops no field (ISI-5305 §5 Gap 2).
func TestRoleDetailReadWriteRoundTrip(t *testing.T) {
	orig := &ksquadv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "architect"},
		Spec: ksquadv1.RoleSpec{
			PromptRef:        ksquadv1.ObjectRef{Name: "architect-prompt"},
			RuntimeClassHint: "gvisor",
			Model:            "claude-opus-4-8",
			FallbackModel: &ksquadv1.FallbackModel{
				Model:            "claude-haiku-4-5",
				ModelEndpointRef: &ksquadv1.SecretRef{Name: "fb-endpoint", Key: "url"},
			},
			DefaultSkills:   []ksquadv1.ObjectRef{{Name: "git"}},
			ActivePhases:    []string{"design", "implementation"},
			Coordinator:     true,
			CoordinatorMode: "auto",
		},
	}

	// 1. project to the read wire the UI GETs.
	detail := roleDetail(orig)
	// 2. serialize exactly as the HTTP layer would, then feed it back as the edit body.
	wire, err := json.Marshal(detail)
	if err != nil {
		t.Fatalf("marshal RoleDetail: %v", err)
	}
	var req roleRequest
	dec := json.NewDecoder(bytes.NewReader(wire))
	dec.DisallowUnknownFields() // the strict role decode path must accept its own read shape.
	if err := dec.Decode(&req); err != nil {
		t.Fatalf("RoleDetail JSON is not a valid roleRequest (strict decode): %v\nwire=%s", err, wire)
	}
	// 3. re-plan and compare specs.
	svc, _ := newComposeFixture(t, grant("bob", "widget", auth.ProjectRoleContributor))
	plan := svc.planRole(req)
	if len(plan.errs) != 0 {
		t.Fatalf("round-trip plan had validation errors: %+v", plan.errs)
	}
	rewritten := plan.desired.(*ksquadv1.Role)
	if !reflect.DeepEqual(rewritten.Spec, orig.Spec) {
		t.Fatalf("round-trip lost data:\n orig=%+v\n got =%+v", orig.Spec, rewritten.Spec)
	}
}

// ── the strict role decode rejects unknown fields (ISI-5358 Gap 2) ──────────────────────────────
//
// DisallowUnknownFields turns a would-be silent drop into a loud 400 so a client that sends a
// field the wire does not model learns of it instead of losing config on a full-spec replace.
func TestHandleRoleRejectsUnknownField(t *testing.T) {
	svc, _ := newComposeFixture(t, grant("bob", "widget", auth.ProjectRoleContributor))
	body := map[string]any{
		"project":       "widget",
		"name":          "engineer",
		"promptRef":     map[string]any{"name": "engineer-prompt"},
		"notARealField": "boom",
	}
	w := do(svc.handleRole(true), http.MethodPost, "/api/roles",
		caller("bob", teamUID, false), body, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field want 400, got %d: %s", w.Code, w.Body.String())
	}
}

// ── a Role composed with a blank model inherits the org default (no phantom persist) ───────────
//
// A blank role-tier model is valid — it means "inherit the system-default ModelConfig". The empty
// string must round-trip as an unset spec.Model, and no fallback should be persisted when unset.
func TestComposeRoleBlankModelInherits(t *testing.T) {
	svc, _ := newComposeFixture(t, grant("bob", "widget", auth.ProjectRoleContributor))
	req := roleRequest{
		Project:   "widget",
		Name:      "reviewer",
		PromptRef: objectRefWire{Name: "reviewer-prompt"},
	}
	w := do(svc.handleRole(true), http.MethodPost, "/api/roles",
		caller("bob", teamUID, false), req, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("compose want 201, got %d: %s", w.Code, w.Body.String())
	}
	var got ksquadv1.Role
	if err := svc.applier.Get(context.Background(), client.ObjectKey{Namespace: teamNS, Name: "reviewer"}, &got); err != nil {
		t.Fatalf("role not applied: %v", err)
	}
	if got.Spec.Model != "" {
		t.Fatalf("blank role model must stay unset, got %q", got.Spec.Model)
	}
	if got.Spec.FallbackModel != nil {
		t.Fatalf("unset fallback must not persist: %+v", got.Spec.FallbackModel)
	}
}

// ── an Agent composed without the optional fields leaves them unset (no phantom persist) ────────
func TestComposeAgentOmitsUnsetOptionalFields(t *testing.T) {
	svc, _ := newComposeFixture(t, grant("bob", "widget", auth.ProjectRoleContributor))
	req := agentRequest{Project: "widget", Name: "plain-dev", Model: "claude-opus-4-8"}
	req.RuntimeRef = objectRefWire{Name: "claude-code"}
	req.RoleRef = objectRefWire{Name: "engineer"}
	req.CredentialSecretRef = secretRefWire{Name: "bob-claude"}
	w := do(svc.handleAgent(true), http.MethodPost, "/api/agents",
		caller("bob", teamUID, false), req, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("compose want 201, got %d: %s", w.Code, w.Body.String())
	}
	var got ksquadv1.Agent
	if err := svc.applier.Get(context.Background(), client.ObjectKey{Namespace: teamNS, Name: "plain-dev"}, &got); err != nil {
		t.Fatalf("agent not applied: %v", err)
	}
	if got.Spec.CredentialClass != "" || got.Spec.FallbackModel != nil {
		t.Fatalf("unset optionals must stay empty: class=%q fallback=%+v", got.Spec.CredentialClass, got.Spec.FallbackModel)
	}
}

// ── an Agent with no write grant on its project → 404 (existence-hiding) ─────

func TestComposeAgentNoMembershipNotFound(t *testing.T) {
	svc, _ := newComposeFixture(t, nil) // no grants
	req := agentRequest{Project: "widget", Name: "x", Model: "m"}
	req.RuntimeRef = objectRefWire{Name: "rt"}
	req.RoleRef = objectRefWire{Name: "role"}
	req.CredentialSecretRef = secretRefWire{Name: "sec"}
	w := do(svc.handleAgent(true), http.MethodPost, "/api/agents",
		caller("nobody", teamUID, false), req, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("no-membership must be 404, got %d: %s", w.Code, w.Body.String())
	}
}

// ── unauthenticated → 401 ────────────────────────────────────────────────────

func TestComposeUnauthenticated(t *testing.T) {
	svc, _ := newComposeFixture(t, nil)
	// No AuthorContext on the request context.
	r := httptest.NewRequest(http.MethodPost, "/api/projects", strings.NewReader(`{"name":"x"}`))
	w := httptest.NewRecorder()
	svc.handleProject(true)(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

// ── Skill inline/git discriminator validation ────────────────────────────────

func TestComposeSkillSourceValidation(t *testing.T) {
	svc, _ := newComposeFixture(t, grant("alice", "widget", auth.ProjectRoleMaintainer))
	tests := []struct {
		name string
		mut  func(*skillRequest)
		want int
	}{
		{"inline-ok", func(s *skillRequest) { s.Source.Type = "inline"; s.Source.Inline = "do things" }, http.StatusCreated},
		{"inline-missing-body", func(s *skillRequest) { s.Source.Type = "inline" }, http.StatusUnprocessableEntity},
		{"git-ok", func(s *skillRequest) {
			s.Source.Type = "git"
			s.Source.Git = &struct {
				RepoRef string `json:"repoRef"`
				Ref     string `json:"ref"`
				Path    string `json:"path,omitempty"`
			}{RepoRef: "github.com/acme/skills", Ref: "abc123"}
		}, http.StatusCreated},
		{"git-missing-ref", func(s *skillRequest) {
			s.Source.Type = "git"
			s.Source.Git = &struct {
				RepoRef string `json:"repoRef"`
				Ref     string `json:"ref"`
				Path    string `json:"path,omitempty"`
			}{RepoRef: "github.com/acme/skills"}
		}, http.StatusUnprocessableEntity},
		{"unknown-type", func(s *skillRequest) { s.Source.Type = "svn" }, http.StatusUnprocessableEntity},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := skillRequest{Project: "widget", Name: "skill-" + tc.name}
			tc.mut(&req)
			w := do(svc.handleSkill(true), http.MethodPost, "/api/skills",
				caller("alice", teamUID, false), req, nil)
			if w.Code != tc.want {
				t.Fatalf("%s: want %d, got %d: %s", tc.name, tc.want, w.Code, w.Body.String())
			}
		})
	}
}

// ── Skill capability-envelope round-trip (ISI-5360 Gap 3) ────────────────────

// TestComposeSkillRoundTripsCapabilityEnvelope proves mcpToolRefs + requires
// (toolchains/sidecars) are WRITTEN through compose. SkillView already projects
// them read-side, so before this fix an edit-save silently dropped them on every
// PUT (the full-spec upsert replaces the spec). Writing a skill with every field
// set and reading the applied CR back is the round-trip AC.
func TestComposeSkillRoundTripsCapabilityEnvelope(t *testing.T) {
	svc, _ := newComposeFixture(t, grant("alice", "widget", auth.ProjectRoleMaintainer))
	req := skillRequest{Project: "widget", Name: "pg-migrate"}
	req.Source.Type = "inline"
	req.Source.Inline = "run the migration"
	req.McpToolRefs = []string{"pg-mcp", "schema-mcp"}
	req.Permissions = []string{"db.write"}
	req.Toolchains = []string{"go@1.23", "node@22"}
	req.Sidecars = []string{"dockerd"}

	w := do(svc.handleSkill(true), http.MethodPost, "/api/skills",
		caller("alice", teamUID, false), req, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body.String())
	}

	var got ksquadv1.Skill
	if err := svc.applier.Get(context.Background(), client.ObjectKey{Namespace: teamNS, Name: "pg-migrate"}, &got); err != nil {
		t.Fatalf("skill not applied: %v", err)
	}
	// objectRefNames sorts: pg-mcp < schema-mcp.
	if mcp := objectRefNames(got.Spec.McpToolRefs); len(mcp) != 2 || mcp[0] != "pg-mcp" || mcp[1] != "schema-mcp" {
		t.Fatalf("mcpToolRefs dropped/garbled: %+v", got.Spec.McpToolRefs)
	}
	if tc := got.Spec.Requires.Toolchains; len(tc) != 2 || tc[0] != "go@1.23" || tc[1] != "node@22" {
		t.Fatalf("requires.toolchains dropped: %+v", tc)
	}
	if sc := got.Spec.Requires.Sidecars; len(sc) != 1 || sc[0] != "dockerd" {
		t.Fatalf("requires.sidecars dropped: %+v", sc)
	}
	if p := got.Spec.Permissions; len(p) != 1 || p[0] != "db.write" {
		t.Fatalf("permissions garbled: %+v", p)
	}
}

// ── Team grants + member-refs round-trip (ISI-5360 Gap 4) ────────────────────

// TestComposeTeamRoundTripsGrantsAndMembers proves grants + Agents/Projects
// member-refs are WRITTEN through compose (admin-only). Before this fix planTeam
// mapped only namespaceStrategy, so a Team edit silently blew away the whole
// composition and every grant on each save.
func TestComposeTeamRoundTripsGrantsAndMembers(t *testing.T) {
	svc, _ := newComposeFixture(t, nil)
	req := teamRequest{
		Name:              "acme-squad",
		NamespaceStrategy: "perTeam",
		Agents:            []string{"cade", "pam"},
		Projects:          []string{"widget"},
		Grants: []capabilityGrantWire{
			{Role: "role-manager", Capabilities: []string{capability.CapabilityWorkItemAuthor}},
		},
	}
	w := do(svc.handleTeam(true), http.MethodPost, "/api/teams",
		caller("root", teamUID, true), req, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body.String())
	}

	// Team is the tenancy root → lands in the control-plane namespace, not a squad ns.
	var got ksquadv1.Team
	if err := svc.applier.Get(context.Background(), client.ObjectKey{Namespace: defaultSystemNamespace, Name: "acme-squad"}, &got); err != nil {
		t.Fatalf("team not applied: %v", err)
	}
	if g := got.Spec.Grants; len(g) != 1 || g[0].Role != "role-manager" ||
		len(g[0].Capabilities) != 1 || g[0].Capabilities[0] != capability.CapabilityWorkItemAuthor {
		t.Fatalf("grants dropped/garbled: %+v", g)
	}
	if names := objectRefNames(got.Spec.Agents); len(names) != 2 || names[0] != "cade" || names[1] != "pam" {
		t.Fatalf("agent member-refs dropped: %+v", got.Spec.Agents)
	}
	if names := objectRefNames(got.Spec.Projects); len(names) != 1 || names[0] != "widget" {
		t.Fatalf("project member-refs dropped: %+v", got.Spec.Projects)
	}
}

// TestComposeTeamGrantValidation fails closed on a malformed grant BEFORE any
// apply (invariant 1), mirroring the CRD's role (MinLength=1) + capabilities
// (MinItems=1) constraints as clean field-level 422s.
func TestComposeTeamGrantValidation(t *testing.T) {
	svc, _ := newComposeFixture(t, nil)
	for _, tc := range []struct {
		name  string
		grant capabilityGrantWire
	}{
		{"empty-role", capabilityGrantWire{Role: "", Capabilities: []string{capability.CapabilityWorkItemAuthor}}},
		{"no-capabilities", capabilityGrantWire{Role: "role-manager"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := teamRequest{Name: "bad-" + tc.name, Grants: []capabilityGrantWire{tc.grant}}
			w := do(svc.handleTeam(true), http.MethodPost, "/api/teams",
				caller("root", teamUID, true), req, nil)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("%s: want 422, got %d: %s", tc.name, w.Code, w.Body.String())
			}
		})
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func mustJSON(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("decode response %q: %v", w.Body.String(), err)
	}
}

// ── squad materialize (ISI-3677, AD-3) ───────────────────────────────────────

// squadReq builds a minimal valid squadRequest for a template.
func squadReq(template, teamName string) squadRequest {
	req := squadRequest{Template: template, Project: "widget"}
	if teamName != "" {
		req.Team = &teamRequest{Name: teamName}
	}
	return req
}

func TestComposeSquadMaterialize_MinimalTrioHappyPath(t *testing.T) {
	// Admin caller: Team compose is admin-only (invariant 2), and onboarding's
	// first-run materialize creates the Team.
	svc, prov := newComposeFixture(t, grant("alice", "widget", auth.ProjectRoleMaintainer))
	w := do(svc.handleComposeSquad, http.MethodPost, "/api/compose/squad",
		caller("root", teamUID, true), squadReq("minimal-trio", "acme-squad"), nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body.String())
	}
	var res squadResponse
	mustJSON(t, w, &res)
	if res.Team == nil || res.Team.Kind != "Team" || res.Team.Name != "acme-squad" || res.Team.Operation != "created" {
		t.Fatalf("unexpected team result: %+v", res.Team)
	}
	if len(res.Agents) != 3 {
		t.Fatalf("minimal-trio must create 3 agents, got %d", len(res.Agents))
	}
	if len(res.Errors) != 0 {
		t.Fatalf("want no errors, got %+v", res.Errors)
	}
	// Every agent references its seeded Role preset + the shared credential (AD-5),
	// with the per-preset default model (FR-6.1).
	wantRole := map[string]string{"boss": "role-boss", "implementer": "role-implementer", "manager": "role-manager"}
	wantModel := map[string]string{"boss": "claude-opus-5", "implementer": "claude-sonnet-5", "manager": "claude-sonnet-5"}
	for _, a := range res.Agents {
		var got ksquadv1.Agent
		if err := svc.applier.Get(context.Background(), client.ObjectKey{Namespace: teamNS, Name: a.Name}, &got); err != nil {
			t.Fatalf("agent %s not applied: %v", a.Name, err)
		}
		if got.Spec.RoleRef.Name != wantRole[a.Name] {
			t.Fatalf("agent %s: want roleRef %s, got %s", a.Name, wantRole[a.Name], got.Spec.RoleRef.Name)
		}
		if got.Spec.CredentialSecretRef.Name != "model-credentials" || got.Spec.CredentialSecretRef.Key != "token" {
			t.Fatalf("agent %s: shared credential not set: %+v", a.Name, got.Spec.CredentialSecretRef)
		}
		if got.Spec.RuntimeRef.Name != "claude-code" {
			t.Fatalf("agent %s: default runtime not set: %+v", a.Name, got.Spec.RuntimeRef)
		}
		if got.Spec.Model != wantModel[a.Name] {
			t.Fatalf("agent %s: want model %s, got %s", a.Name, wantModel[a.Name], got.Spec.Model)
		}
	}
	// Provenance: 1 Team + 3 Agents, all server-stamped.
	if len(*prov) != 4 {
		t.Fatalf("want 4 provenance rows, got %d: %+v", len(*prov), *prov)
	}
}

// ISI-5223: the Team compose default-grants work_item.author to the template's
// coordinator role (manager → role-manager), so a PM/coordinator agent can
// orchestrate (decompose + delegate) out of the box. Non-coordinator roles are
// omitted — deny-by-default preserved.
func TestComposeSquadMaterialize_CoordinatorGrant(t *testing.T) {
	svc, _ := newComposeFixture(t, grant("alice", "widget", auth.ProjectRoleMaintainer))
	w := do(svc.handleComposeSquad, http.MethodPost, "/api/compose/squad",
		caller("root", teamUID, true), squadReq("minimal-trio", "acme-squad"), nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body.String())
	}
	var teams ksquadv1.TeamList
	if err := svc.applier.List(context.Background(), &teams); err != nil {
		t.Fatalf("list teams: %v", err)
	}
	var team *ksquadv1.Team
	for i := range teams.Items {
		if teams.Items[i].Name == "acme-squad" {
			team = &teams.Items[i]
			break
		}
	}
	if team == nil {
		t.Fatalf("acme-squad team not applied")
	}
	if len(team.Spec.Grants) != 1 {
		t.Fatalf("want 1 coordinator grant, got %+v", team.Spec.Grants)
	}
	g := team.Spec.Grants[0]
	if g.Role != "role-manager" {
		t.Fatalf("want grant on role-manager, got %q", g.Role)
	}
	if len(g.Capabilities) != 1 || g.Capabilities[0] != capability.CapabilityWorkItemAuthor {
		t.Fatalf("want [work_item.author], got %+v", g.Capabilities)
	}
}

// coordinatorGrants: only coordinator presets are granted; dedup + stable order.
func TestCoordinatorGrantsHelper(t *testing.T) {
	// No coordinator in the roster ⇒ no grants (solo = boss+impl).
	if g := coordinatorGrants(squadTemplates["solo"]); g != nil {
		t.Fatalf("solo has no coordinator, want nil grants, got %+v", g)
	}
	// bmad has multiple manager-preset agents ⇒ ONE deduped role-manager grant.
	g := coordinatorGrants(squadTemplates["bmad"])
	if len(g) != 1 || g[0].Role != "role-manager" {
		t.Fatalf("bmad: want one deduped role-manager grant, got %+v", g)
	}
	if len(g[0].Capabilities) != 1 || g[0].Capabilities[0] != capability.CapabilityWorkItemAuthor {
		t.Fatalf("want [work_item.author], got %+v", g[0].Capabilities)
	}
}

func TestComposeSquadMaterialize_SoloTemplate(t *testing.T) {
	svc, _ := newComposeFixture(t, grant("alice", "widget", auth.ProjectRoleMaintainer))
	w := do(svc.handleComposeSquad, http.MethodPost, "/api/compose/squad",
		caller("alice", teamUID, false), squadReq("solo", ""), nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body.String())
	}
	var res squadResponse
	mustJSON(t, w, &res)
	if res.Team != nil {
		t.Fatalf("no team requested ⇒ no team result, got %+v", res.Team)
	}
	if len(res.Agents) != 2 {
		t.Fatalf("solo must create Boss+Impl (2 agents), got %d", len(res.Agents))
	}
}

func TestComposeSquadMaterialize_BMADTemplate(t *testing.T) {
	svc, _ := newComposeFixture(t, grant("alice", "widget", auth.ProjectRoleMaintainer))
	w := do(svc.handleComposeSquad, http.MethodPost, "/api/compose/squad",
		caller("alice", teamUID, false), squadReq("bmad", ""), nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body.String())
	}
	var res squadResponse
	mustJSON(t, w, &res)
	if len(res.Agents) != 10 {
		t.Fatalf("bmad must create the examples/bmad-team set (10 agents), got %d", len(res.Agents))
	}
	for _, a := range res.Agents {
		var got ksquadv1.Agent
		if err := svc.applier.Get(context.Background(), client.ObjectKey{Namespace: teamNS, Name: a.Name}, &got); err != nil {
			t.Fatalf("agent %s not applied: %v", a.Name, err)
		}
		switch got.Spec.RoleRef.Name {
		case "role-boss", "role-implementer", "role-manager":
		default:
			t.Fatalf("agent %s references non-seeded role %s", a.Name, got.Spec.RoleRef.Name)
		}
	}
}

func TestComposeSquadMaterialize_InvalidTemplate422(t *testing.T) {
	svc, _ := newComposeFixture(t, grant("alice", "widget", auth.ProjectRoleMaintainer))
	w := do(svc.handleComposeSquad, http.MethodPost, "/api/compose/squad",
		caller("alice", teamUID, false), squadReq("nope", ""), nil)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d: %s", w.Code, w.Body.String())
	}
	var res struct {
		Error  string       `json:"error"`
		Fields []fieldError `json:"fields"`
	}
	mustJSON(t, w, &res)
	if res.Error != "validation failed" || len(res.Fields) != 1 || res.Fields[0].Field != "template" {
		t.Fatalf("want template field error, got %+v", res)
	}
}

func TestComposeSquadMaterialize_Unauthenticated401(t *testing.T) {
	svc, _ := newComposeFixture(t, nil)
	r := httptest.NewRequest(http.MethodPost, "/api/compose/squad", strings.NewReader(`{"template":"solo"}`))
	w := httptest.NewRecorder()
	svc.handleComposeSquad(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

func TestComposeSquadMaterialize_NoTeamNamespace404(t *testing.T) {
	svc, _ := newComposeFixture(t, nil)
	// A caller whose Team UID resolves to no Team → cross-tenant 404.
	w := do(svc.handleComposeSquad, http.MethodPost, "/api/compose/squad",
		caller("alice", "33333333-3333-3333-3333-333333333333", true), squadReq("solo", ""), nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestComposeSquadMaterialize_TeamAlreadyExisting(t *testing.T) {
	// The pre-existing Team lives in the control-plane namespace (systemNS), where
	// Team CRs are applied (ISI-3919) — NOT in a per-team namespace.
	svc, _ := newComposeFixture(t, grant("alice", "widget", auth.ProjectRoleMaintainer),
		&ksquadv1.Team{ObjectMeta: metav1.ObjectMeta{Name: "acme-squad", Namespace: defaultSystemNamespace}})
	// Admin so the (admin-only) Team plan authorizes; the existing Team is
	// reported as "existing", not a 409 failure (AC1: created if absent).
	w := do(svc.handleComposeSquad, http.MethodPost, "/api/compose/squad",
		caller("root", teamUID, true), squadReq("solo", "acme-squad"), nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body.String())
	}
	var res squadResponse
	mustJSON(t, w, &res)
	if res.Team == nil || res.Team.Operation != "existing" {
		t.Fatalf("want team operation existing, got %+v", res.Team)
	}
	if len(res.Agents) != 2 || len(res.Errors) != 0 {
		t.Fatalf("want 2 agents and no errors, got %+v", res)
	}
}

func TestComposeSquadMaterialize_PartialFailure207Verbatim(t *testing.T) {
	// The caller has no membership on the agents' project scope → every Agent
	// fails with 404 (existence-hiding), surfaced verbatim (AC4, NFR-5).
	svc, _ := newComposeFixture(t, nil)
	req := squadReq("minimal-trio", "")
	req.Project = "widget"
	w := do(svc.handleComposeSquad, http.MethodPost, "/api/compose/squad",
		caller("alice", teamUID, false), req, nil)
	if w.Code != http.StatusMultiStatus {
		t.Fatalf("want 207, got %d: %s", w.Code, w.Body.String())
	}
	var res squadResponse
	mustJSON(t, w, &res)
	if len(res.Agents) != 0 || len(res.Errors) != 3 {
		t.Fatalf("want 0 created + 3 verbatim errors, got %+v", res)
	}
	for _, e := range res.Errors {
		if e.Kind != "Agent" || e.Status != http.StatusNotFound || e.Error == "" {
			t.Fatalf("error not verbatim: %+v", e)
		}
	}
}

func TestComposeSquadMaterialize_ModelOverride(t *testing.T) {
	svc, _ := newComposeFixture(t, grant("alice", "widget", auth.ProjectRoleMaintainer))
	req := squadReq("solo", "")
	req.Models = map[string]string{"boss": "claude-opus-4-8"}
	w := do(svc.handleComposeSquad, http.MethodPost, "/api/compose/squad",
		caller("alice", teamUID, false), req, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body.String())
	}
	var got ksquadv1.Agent
	if err := svc.applier.Get(context.Background(), client.ObjectKey{Namespace: teamNS, Name: "boss"}, &got); err != nil {
		t.Fatalf("boss agent not applied: %v", err)
	}
	if got.Spec.Model != "claude-opus-4-8" {
		t.Fatalf("console-supplied model must win (presets.ts), got %s", got.Spec.Model)
	}
}
