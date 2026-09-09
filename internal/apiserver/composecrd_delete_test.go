package apiserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/auth"
)

// ============================================================================
// ISI-4107 — DELETE /api/{kind}/{name} for Agent + Project (gap 2 of ISI-4106).
//
// Delete reuses the edit seams: write-tier RBAC, team-namespace scoping
// (cross-tenant ⇒ existence-hiding 404), and a durable provenance row. Team
// delete is intentionally NOT wired (board teardown decision); these tests pin
// the Agent + Project behavior.
// ============================================================================

// deletedProvenance reports whether a "deleted" provenance row was captured for
// the given kind/name.
func deletedProvenance(rows []map[string]any, kind, name string) bool {
	for _, r := range rows {
		if r["operation"] == "deleted" && r["kind"] == kind && r["name"] == name && r["eventType"] == "crd_applied" {
			return true
		}
	}
	return false
}

func TestComposeDeleteProject_ContributorAllowed(t *testing.T) {
	// A Project's membership scope IS its own name (scopeIsName): a contributor on
	// "widget" may delete the "widget" Project.
	svc, prov := newComposeFixture(t, grant("bob", "widget", auth.ProjectRoleContributor),
		&ksquadv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "widget", Namespace: teamNS}})

	w := do(svc.handleProjectDelete(), http.MethodDelete, "/api/projects/widget",
		caller("bob", teamUID, false), nil, map[string]string{"name": "widget"})
	if w.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", w.Code, w.Body.String())
	}
	var got ksquadv1.Project
	err := svc.applier.Get(context.Background(), client.ObjectKey{Namespace: teamNS, Name: "widget"}, &got)
	if err == nil {
		t.Fatalf("project still present after delete")
	}
	if !deletedProvenance(*prov, "Project", "widget") {
		t.Fatalf("no delete provenance row captured: %+v", *prov)
	}
}

func TestComposeDeleteAgent_AdminAllowed(t *testing.T) {
	// Admins compose fleet-wide: no ?project= scope required.
	svc, _ := newComposeFixture(t, nil,
		&ksquadv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "backend-dev", Namespace: teamNS}})

	w := do(svc.handleAgentDelete(), http.MethodDelete, "/api/agents/backend-dev",
		caller("root", teamUID, true), nil, map[string]string{"name": "backend-dev"})
	if w.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", w.Code, w.Body.String())
	}
	var got ksquadv1.Agent
	if err := svc.applier.Get(context.Background(), client.ObjectKey{Namespace: teamNS, Name: "backend-dev"}, &got); err == nil {
		t.Fatalf("agent still present after delete")
	}
}

func TestComposeDeleteAgent_ContributorScopedByProjectQuery(t *testing.T) {
	// Agent RBAC is project-scoped; the delete carries the project via ?project=.
	svc, _ := newComposeFixture(t, grant("bob", "widget", auth.ProjectRoleContributor),
		&ksquadv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "backend-dev", Namespace: teamNS}})

	w := do(svc.handleAgentDelete(), http.MethodDelete, "/api/agents/backend-dev?project=widget",
		caller("bob", teamUID, false), nil, map[string]string{"name": "backend-dev"})
	if w.Code != http.StatusNoContent {
		t.Fatalf("want 204 with ?project= scope, got %d: %s", w.Code, w.Body.String())
	}
}

func TestComposeDeleteAgent_ContributorMissingProjectFailsClosed(t *testing.T) {
	// A project-scoped kind with no scope is a request bug — fail closed (400),
	// never delete unscoped.
	svc, _ := newComposeFixture(t, grant("bob", "widget", auth.ProjectRoleContributor),
		&ksquadv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "backend-dev", Namespace: teamNS}})

	w := do(svc.handleAgentDelete(), http.MethodDelete, "/api/agents/backend-dev",
		caller("bob", teamUID, false), nil, map[string]string{"name": "backend-dev"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 (project scope required), got %d: %s", w.Code, w.Body.String())
	}
	// The object must survive a fail-closed authz check.
	var got ksquadv1.Agent
	if err := svc.applier.Get(context.Background(), client.ObjectKey{Namespace: teamNS, Name: "backend-dev"}, &got); err != nil {
		t.Fatalf("agent wrongly deleted on failed authz: %v", err)
	}
}

func TestComposeDeleteMissing404(t *testing.T) {
	svc, _ := newComposeFixture(t, nil)
	w := do(svc.handleProjectDelete(), http.MethodDelete, "/api/projects/ghost",
		caller("root", teamUID, true), nil, map[string]string{"name": "ghost"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404 for missing object, got %d: %s", w.Code, w.Body.String())
	}
}

func TestComposeDeleteCrossTenant404(t *testing.T) {
	// A Project owned by the FOREIGN tenant is invisible to the caller's namespace
	// scope → 404 existence-hiding, and the foreign object is untouched.
	svc, _ := newComposeFixture(t, nil,
		&ksquadv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "secret", Namespace: otherNS}})
	// Admin so RBAC passes; the 404 must come from namespace scoping alone.
	w := do(svc.handleProjectDelete(), http.MethodDelete, "/api/projects/secret",
		caller("root", teamUID, true), nil, map[string]string{"name": "secret"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404 (cross-tenant existence-hiding), got %d: %s", w.Code, w.Body.String())
	}
	var foreign ksquadv1.Project
	if err := svc.applier.Get(context.Background(), client.ObjectKey{Namespace: otherNS, Name: "secret"}, &foreign); err != nil {
		t.Fatalf("cross-tenant object wrongly deleted: %v", err)
	}
}

func TestComposeDeleteViewerForbidden(t *testing.T) {
	svc, _ := newComposeFixture(t, grant("alice", "widget", auth.ProjectRoleViewer),
		&ksquadv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "widget", Namespace: teamNS}})
	w := do(svc.handleProjectDelete(), http.MethodDelete, "/api/projects/widget",
		caller("alice", teamUID, false), nil, map[string]string{"name": "widget"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403 for viewer, got %d: %s", w.Code, w.Body.String())
	}
	var got ksquadv1.Project
	if err := svc.applier.Get(context.Background(), client.ObjectKey{Namespace: teamNS, Name: "widget"}, &got); err != nil {
		t.Fatalf("project wrongly deleted for viewer: %v", err)
	}
}

func TestComposeDeleteUnauthenticated(t *testing.T) {
	svc, _ := newComposeFixture(t, nil)
	// No AuthorContext on the request context (BFFAuthz is upstream).
	r := httptest.NewRequest(http.MethodDelete, "/api/projects/widget", nil)
	r = mux.SetURLVars(r, map[string]string{"name": "widget"})
	w := httptest.NewRecorder()
	svc.handleProjectDelete()(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 unauthenticated, got %d: %s", w.Code, w.Body.String())
	}
}
