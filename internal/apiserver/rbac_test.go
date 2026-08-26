package apiserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/gorilla/mux"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/auth"
)

// fakeRoleResolver is a map-backed ProjectRoleResolver for the middleware tests.
type fakeRoleResolver struct {
	roles map[string]map[string]string // principal → project → role
	err   error                        // forced infrastructure error (overrides roles)
}

func (f fakeRoleResolver) RoleForPrincipal(_ context.Context, principal, project string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if projs, ok := f.roles[principal]; ok {
		if role, ok := projs[project]; ok {
			return role, nil
		}
	}
	return "", auth.ErrNoMembership
}

// serveRBAC runs requireProjectRole around a 200-OK terminal handler for the given caller and
// {projectId}, returning the recorded status. A nil author ⇒ no AuthorContext on the context.
func serveRBAC(t *testing.T, resolver ProjectRoleResolver, minRole string, author *discussion.AuthorContext, projectID string) int {
	t.Helper()
	terminal := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := requireProjectRole(resolver, minRole)(terminal)

	r := httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/dashboard", nil)
	r = mux.SetURLVars(r, map[string]string{"projectId": projectID})
	if author != nil {
		r = r.WithContext(discussion.WithAuth(r.Context(), *author))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

func user(principal string) *discussion.AuthorContext {
	return &discussion.AuthorContext{Principal: principal, TeamID: uuid.New(), IsAdmin: false}
}

func admin(principal string) *discussion.AuthorContext {
	return &discussion.AuthorContext{Principal: principal, TeamID: uuid.New(), IsAdmin: true}
}

func TestRequireProjectRole(t *testing.T) {
	resolver := fakeRoleResolver{roles: map[string]map[string]string{
		"alice": {"checkout": auth.ProjectRoleMaintainer},
		"bob":   {"checkout": auth.ProjectRoleViewer},
	}}

	tests := []struct {
		name    string
		author  *discussion.AuthorContext
		minRole string
		project string
		want    int
	}{
		{"admin bypasses membership", admin("root"), auth.ProjectRoleMaintainer, "checkout", http.StatusOK},
		{"admin allowed on unknown project", admin("root"), auth.ProjectRoleViewer, "nonexistent", http.StatusOK},
		{"maintainer meets maintainer", user("alice"), auth.ProjectRoleMaintainer, "checkout", http.StatusOK},
		{"maintainer meets viewer", user("alice"), auth.ProjectRoleViewer, "checkout", http.StatusOK},
		{"viewer meets viewer", user("bob"), auth.ProjectRoleViewer, "checkout", http.StatusOK},
		{"viewer fails maintainer", user("bob"), auth.ProjectRoleMaintainer, "checkout", http.StatusForbidden},
		{"no membership hides project", user("bob"), auth.ProjectRoleViewer, "billing", http.StatusNotFound},
		{"unknown principal hides project", user("mallory"), auth.ProjectRoleViewer, "checkout", http.StatusNotFound},
		{"unauthenticated", nil, auth.ProjectRoleViewer, "checkout", http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := serveRBAC(t, resolver, tt.minRole, tt.author, tt.project); got != tt.want {
				t.Errorf("status = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestRequireProjectRole_NilResolverFailsClosed(t *testing.T) {
	if got := serveRBAC(t, nil, auth.ProjectRoleViewer, user("alice"), "checkout"); got != http.StatusServiceUnavailable {
		t.Errorf("nil resolver status = %d, want 503", got)
	}
}

func TestRequireProjectRole_InfraErrorIs502(t *testing.T) {
	resolver := fakeRoleResolver{err: errors.New("db down")}
	if got := serveRBAC(t, resolver, auth.ProjectRoleViewer, user("alice"), "checkout"); got != http.StatusBadGateway {
		t.Errorf("infra-error status = %d, want 502", got)
	}
}

func TestRequireProjectRole_EmptyProjectIDIs404(t *testing.T) {
	resolver := fakeRoleResolver{roles: map[string]map[string]string{}}
	if got := serveRBAC(t, resolver, auth.ProjectRoleViewer, user("alice"), ""); got != http.StatusNotFound {
		t.Errorf("empty projectId status = %d, want 404", got)
	}
}
