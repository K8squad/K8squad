package apiserver

// fleetlist_detail_test.go — ADR-0016 / ISI-4007 authoring-spec detail reads that
// hydrate the compose EDIT form (AgentDetail/RoleDetail/ProjectDetail + the Skill
// inline/?team= extension). Table tests per kind exercise the D2 scoping matrix:
// tenant own-ns hit / tenant cross-ns 404 / admin+?team= hit / admin missing-team
// 404 / admin foreign-team 404 (existence-hiding — deny is always 404, never 403).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// fullAgentObj builds an Agent carrying every form-owned field (BYO endpoint,
// credential class, fallback-with-endpoint, multiple skill refs) so the detail
// projection's round-trip fidelity can be asserted end to end.
func fullAgentObj(ns, name string) *ksquadv1.Agent {
	return &ksquadv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID("ag-full")},
		Spec: ksquadv1.AgentSpec{
			RuntimeRef:          ksquadv1.ObjectRef{Name: "claude-code"},
			RoleRef:             ksquadv1.ObjectRef{Name: "boss", Namespace: "shared"},
			SkillRefs:           []ksquadv1.ObjectRef{{Name: "sk-a"}, {Name: "sk-b", Namespace: "shared"}},
			Model:               "claude-opus-5",
			ModelEndpointRef:    &ksquadv1.SecretRef{Name: "byo-endpoint", Key: "url"},
			CredentialSecretRef: ksquadv1.SecretRef{Name: "model-creds", Key: "token"},
			CredentialClass:     "human-seat",
			FallbackModel: &ksquadv1.FallbackModel{
				Model:            "claude-sonnet-5",
				ModelEndpointRef: &ksquadv1.SecretRef{Name: "fb-endpoint"},
			},
			// A non-form field the read must NOT surface (ADR-0016 round-trip note).
			OwnedBy: ksquadv1.PrincipalRef("user:alice"),
		},
	}
}

// projectAuthObj builds a Project carrying every form-owned field (repo url/ref +
// BYO repo auth + goals + egress policy ref).
func projectAuthObj(ns, name string) *ksquadv1.Project {
	return &ksquadv1.Project{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: ksquadv1.ProjectSpec{
			Repo: ksquadv1.RepoSpec{
				URL:  "https://github.com/acme/widget",
				Ref:  "main",
				Auth: &ksquadv1.RepoAuth{CredentialSecretRef: ksquadv1.SecretRef{Name: "gh-pat"}},
			},
			Goals:           []string{"ship v1", "keep it green"},
			EgressPolicyRef: &ksquadv1.ObjectRef{Name: "allow-github"},
		},
	}
}

// --- AgentDetail scoping matrix ------------------------------------------------

func TestAgentDetailScoping(t *testing.T) {
	r := newFleetReader(t, twoSquadObjs(fleetUIDA, fleetUIDB)...)
	ctx := context.Background()

	// Tenant A reads their own agent by name.
	if d, err := r.AgentDetail(ctx, fleetUIDA, "agent-a", "", false); err != nil || d.Name != "agent-a" {
		t.Fatalf("tenant own agent: d=%+v err=%v", d, err)
	}
	// Tenant A cannot read squad-b's agent — existence-hiding 404.
	if _, err := r.AgentDetail(ctx, fleetUIDA, "agent-b", "", false); !errors.Is(err, ErrDetailNotFound) {
		t.Fatalf("tenant cross-ns: want ErrDetailNotFound, got %v", err)
	}
	// Admin with ?team=B reads squad-b's agent.
	if d, err := r.AgentDetail(ctx, "", "agent-b", fleetUIDB, true); err != nil || d.Name != "agent-b" {
		t.Fatalf("admin+team agent: d=%+v err=%v", d, err)
	}
	// Admin MUST name a squad: missing ?team= ⇒ 404 (ADR-0016 D2).
	if _, err := r.AgentDetail(ctx, "", "agent-a", "", true); !errors.Is(err, ErrDetailNotFound) {
		t.Fatalf("admin missing team: want ErrDetailNotFound, got %v", err)
	}
	// Admin naming a foreign/unknown squad ⇒ 404 (existence-hiding).
	if _, err := r.AgentDetail(ctx, "", "agent-a", "no-such-uid", true); !errors.Is(err, ErrDetailNotFound) {
		t.Fatalf("admin foreign team: want ErrDetailNotFound, got %v", err)
	}
	// A name that resolves in the wrong squad is still hidden: agent-a is squad-a,
	// so an admin scoping to squad-b must not see it.
	if _, err := r.AgentDetail(ctx, "", "agent-a", fleetUIDB, true); !errors.Is(err, ErrDetailNotFound) {
		t.Fatalf("admin+team miss: want ErrDetailNotFound, got %v", err)
	}
}

// TestAgentDetailRoundTripFidelity asserts every form-owned field survives the
// projection and non-form CRD fields are dropped (mirrors the write wire).
func TestAgentDetailRoundTripFidelity(t *testing.T) {
	r := newFleetReader(t,
		team("squad-a", "alpha", fleetUIDA),
		fullAgentObj("squad-a", "cade"),
	)
	d, err := r.AgentDetail(context.Background(), fleetUIDA, "cade", "", false)
	if err != nil {
		t.Fatalf("AgentDetail: %v", err)
	}
	if d.Name != "cade" || d.RuntimeRef.Name != "claude-code" || d.Model != "claude-opus-5" {
		t.Fatalf("scalar fields: %+v", d)
	}
	if d.RoleRef.Name != "boss" || d.RoleRef.Namespace != "shared" {
		t.Fatalf("roleRef ns not carried: %+v", d.RoleRef)
	}
	if len(d.SkillRefs) != 2 || d.SkillRefs[1].Namespace != "shared" {
		t.Fatalf("skillRefs: %+v", d.SkillRefs)
	}
	if d.ModelEndpointRef == nil || d.ModelEndpointRef.Key != "url" {
		t.Fatalf("modelEndpointRef: %+v", d.ModelEndpointRef)
	}
	if d.CredentialSecretRef.Name != "model-creds" || d.CredentialClass != "human-seat" {
		t.Fatalf("credential: %+v class=%q", d.CredentialSecretRef, d.CredentialClass)
	}
	if d.FallbackModel == nil || d.FallbackModel.Model != "claude-sonnet-5" ||
		d.FallbackModel.ModelEndpointRef == nil || d.FallbackModel.ModelEndpointRef.Name != "fb-endpoint" {
		t.Fatalf("fallbackModel: %+v", d.FallbackModel)
	}
	// Non-form CRD field (ownedBy) must not appear in the JSON body.
	b, _ := json.Marshal(d)
	if got := string(b); contains(got, "ownedBy") || contains(got, "alice") {
		t.Fatalf("non-form field leaked into detail body: %s", got)
	}
}

// --- RoleDetail scoping --------------------------------------------------------

func TestRoleDetailScoping(t *testing.T) {
	r := newFleetReader(t, twoSquadObjs(fleetUIDA, fleetUIDB)...)
	ctx := context.Background()

	d, err := r.RoleDetail(ctx, fleetUIDA, "dev", "", false)
	if err != nil || d.Name != "dev" || d.PromptRef.Name != "dev-prompt" || d.RuntimeClassHint != "gpu" {
		t.Fatalf("tenant own role: d=%+v err=%v", d, err)
	}
	if len(d.DefaultSkills) != 1 || d.DefaultSkills[0].Name != "sk-a" {
		t.Fatalf("defaultSkills: %+v", d.DefaultSkills)
	}
	if _, err := r.RoleDetail(ctx, fleetUIDA, "qa", "", false); !errors.Is(err, ErrDetailNotFound) {
		t.Fatalf("tenant cross-ns role: want ErrDetailNotFound, got %v", err)
	}
	if d, err := r.RoleDetail(ctx, "", "qa", fleetUIDB, true); err != nil || d.Name != "qa" {
		t.Fatalf("admin+team role: d=%+v err=%v", d, err)
	}
	if _, err := r.RoleDetail(ctx, "", "dev", "", true); !errors.Is(err, ErrDetailNotFound) {
		t.Fatalf("admin missing team role: want ErrDetailNotFound, got %v", err)
	}
}

// --- ProjectDetail scoping + repo auth fidelity --------------------------------

func TestProjectDetailScopingAndAuth(t *testing.T) {
	r := newFleetReader(t,
		team("squad-a", "alpha", fleetUIDA),
		team("squad-b", "beta", fleetUIDB),
		projectAuthObj("squad-a", "widget"),
		project("squad-b", "gadget", "https://github.com/acme/gadget"),
	)
	ctx := context.Background()

	d, err := r.ProjectDetail(ctx, fleetUIDA, "widget", "", false)
	if err != nil {
		t.Fatalf("tenant own project: %v", err)
	}
	if d.Repo.URL != "https://github.com/acme/widget" || d.Repo.Ref != "main" {
		t.Fatalf("repo: %+v", d.Repo)
	}
	if d.Repo.Auth == nil || d.Repo.Auth.CredentialSecretRef.Name != "gh-pat" {
		t.Fatalf("repo.auth not carried: %+v", d.Repo.Auth)
	}
	if len(d.Goals) != 2 || d.EgressPolicyRef == nil || d.EgressPolicyRef.Name != "allow-github" {
		t.Fatalf("goals/egress: %+v %+v", d.Goals, d.EgressPolicyRef)
	}
	// Cross-ns + admin scoping matrix.
	if _, err := r.ProjectDetail(ctx, fleetUIDA, "gadget", "", false); !errors.Is(err, ErrDetailNotFound) {
		t.Fatalf("tenant cross-ns project: want ErrDetailNotFound, got %v", err)
	}
	if d, err := r.ProjectDetail(ctx, "", "gadget", fleetUIDB, true); err != nil || d.Name != "gadget" {
		t.Fatalf("admin+team project: d=%+v err=%v", d, err)
	}
	if _, err := r.ProjectDetail(ctx, "", "widget", "", true); !errors.Is(err, ErrDetailNotFound) {
		t.Fatalf("admin missing team project: want ErrDetailNotFound, got %v", err)
	}
}

// --- Skill inline body + ?team= disambiguation (ADR-0016 extension) ------------

func TestSkillDetailInlineBody(t *testing.T) {
	inline := skillObj("squad-a", "greet", "s-inline", ksquadv1.SkillSourceInline, "fs:read")
	inline.Spec.Source.Inline = "# greet\nsay hi"
	r := newFleetReader(t, team("squad-a", "alpha", fleetUIDA), inline)

	v, err := r.Skill(context.Background(), fleetUIDA, "greet", "", false)
	if err != nil {
		t.Fatalf("Skill(greet): %v", err)
	}
	if v.Inline != "# greet\nsay hi" || v.SourceType != string(ksquadv1.SkillSourceInline) {
		t.Fatalf("inline body not projected: %+v", v)
	}
}

func TestSkillDetailTeamSelector(t *testing.T) {
	// Two squads each own a skill named "shared"; the admin ?team= selector must
	// resolve the one in the named squad, not the first by namespace order.
	skA := skillObj("squad-a", "shared", "sh-a", ksquadv1.SkillSourceInline, "fs:read")
	skB := skillObj("squad-b", "shared", "sh-b", ksquadv1.SkillSourceInline, "net:egress")
	r := newFleetReader(t,
		team("squad-a", "alpha", fleetUIDA),
		team("squad-b", "beta", fleetUIDB),
		skA, skB,
	)
	ctx := context.Background()

	vB, err := r.Skill(ctx, "", "shared", fleetUIDB, true)
	if err != nil || vB.Namespace != "squad-b" || vB.UID != "sh-b" {
		t.Fatalf("admin ?team=B: v=%+v err=%v", vB, err)
	}
	vA, err := r.Skill(ctx, "", "shared", fleetUIDA, true)
	if err != nil || vA.Namespace != "squad-a" || vA.UID != "sh-a" {
		t.Fatalf("admin ?team=A: v=%+v err=%v", vA, err)
	}
	// Admin ?team= naming a foreign/unknown squad ⇒ existence-hiding 404.
	if _, err := r.Skill(ctx, "", "shared", "no-such-uid", true); !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("admin foreign team skill: want ErrSkillNotFound, got %v", err)
	}
	// Back-compat: admin with NO ?team= still resolves fleet-wide (first by ns).
	if v, err := r.Skill(ctx, "", "shared", "", true); err != nil || v.Namespace != "squad-a" {
		t.Fatalf("admin no-team back-compat: v=%+v err=%v", v, err)
	}
}

// --- handler wiring for the new detail routes ----------------------------------

func TestFleetDetailHandlers(t *testing.T) {
	adminID := uuid.MustParse("66666666-6666-6666-6666-666666666666")
	objs := append(twoSquadObjs(fleetUIDA, fleetUIDB), projectAuthObj("squad-a", "widget"))
	reader := newFleetReader(t, objs...)
	h := testFleetServer(t, adminID, true, reader)

	// Admin reads squad-b's agent via ?team=B.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/squad/agents/agent-b?team="+fleetUIDB, nil), devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("agent detail: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var ad AgentDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &ad); err != nil || ad.Name != "agent-b" {
		t.Fatalf("agent detail body: %+v err=%v", ad, err)
	}

	// Admin missing ?team= ⇒ 404 (must name a squad).
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/squad/agents/agent-b", nil), devToken))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("agent detail no team: got %d, want 404", rec.Code)
	}

	// Role + project detail routes are wired.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/squad/roles/dev?team="+fleetUIDA, nil), devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("role detail: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/squad/projects/widget?team="+fleetUIDA, nil), devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("project detail: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var pd ProjectDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &pd); err != nil || pd.Repo.Auth == nil {
		t.Fatalf("project detail body: %+v err=%v", pd, err)
	}
}

func TestFleetDetailHandlersNilReader501(t *testing.T) {
	adminID := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	h := testFleetServer(t, adminID, true, nil)
	for _, path := range []string{
		"/api/squad/agents/agent-a", "/api/squad/roles/dev", "/api/squad/projects/widget",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, path, nil), devToken))
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("%s (nil reader): got %d, want 501", path, rec.Code)
		}
	}
}

// contains is a tiny substring helper (avoid importing strings for one call site).
func contains(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
