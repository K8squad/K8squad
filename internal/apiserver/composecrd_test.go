package apiserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	svc, _ := newComposeFixture(t, grant("alice", "widget", auth.ProjectRoleMaintainer),
		&ksquadv1.Team{ObjectMeta: metav1.ObjectMeta{Name: "acme-squad", Namespace: teamNS}})
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
