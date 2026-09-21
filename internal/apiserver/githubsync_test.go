package apiserver

// githubsync_test.go — ISI-4011: POST /api/projects/{projectId}/github/sync.
// Covers:
//   - 202 Accepted: annotation patch lands on the Project
//   - 429 debounce: a second call within the window is rejected
//   - 404 existence-hiding: foreign-team project
//   - 501 nil service: writer not wired

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	fake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/controller/reposync"
)

// buildSyncClient returns a fake client.Client seeded with the given objects.
func buildSyncClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(overviewScheme(t)).WithObjects(objs...).Build()
}

func testGithubSyncServer(t *testing.T, teamID uuid.UUID, svc *GithubSyncService) http.Handler {
	t.Helper()
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken: {Principal: "user:alice", TeamID: teamID},
	}}
	return NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		GithubSync:    svc,
	}).Handler()
}

// --- tests ----------------------------------------------------------------------

func TestGithubSync_202(t *testing.T) {
	teamID := uuid.New()
	ns := teamID.String()
	// The resolver needs a Team CR whose UID == teamID so resolveTeamNamespace
	// maps teamID → ns, and a Project in that namespace.
	tm := team(ns, "my-team", teamID.String())
	proj := project(ns, "myproject", "")
	c := buildSyncClient(t, tm, proj)
	svc := NewGithubSyncService(c, c)

	srv := testGithubSyncServer(t, teamID, svc)
	req := httptest.NewRequest(http.MethodPost, "/api/projects/myproject/github/sync", nil)
	req.AddCookie(&http.Cookie{Name: "ksquad_session", Value: devToken})
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", w.Code, w.Body.String())
	}

	// Verify annotation was set on the Project.
	var got ksquadv1.Project
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "myproject"}, &got); err != nil {
		t.Fatalf("get project: %v", err)
	}
	if got.Annotations[reposync.TriggerAnnotation] == "" {
		t.Error("expected scm-sync-trigger annotation to be set")
	}
}

// TestGithubSync_CompositeProjectId is the ISI-4662 regression: the console addresses
// the project by its canonical url-encoded "namespace/name" composite id, so the route
// must be {projectId:.+}. A bare {projectId} stops at the decoded slash and 404s — which
// the S5c GitHub tab renders as the honest "No GitHub status" empty state, i.e. the screen
// looks empty even though the mirror has data.
func TestGithubSync_CompositeProjectId(t *testing.T) {
	teamID := uuid.New()
	ns := teamID.String()
	tm := team(ns, "my-team", teamID.String())
	proj := project(ns, "myproject", "")
	c := buildSyncClient(t, tm, proj)
	svc := NewGithubSyncService(c, c)

	srv := testGithubSyncServer(t, teamID, svc)
	// "<ns>/myproject", url-encoded exactly as the console sends it (slash → %2F).
	target := "/api/projects/" + url.PathEscape(ns+"/myproject") + "/github/sync"
	req := httptest.NewRequest(http.MethodPost, target, nil)
	req.AddCookie(&http.Cookie{Name: "ksquad_session", Value: devToken})
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("composite projectId: want 202, got %d: %s", w.Code, w.Body.String())
	}
	var got ksquadv1.Project
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "myproject"}, &got); err != nil {
		t.Fatalf("get project: %v", err)
	}
	if got.Annotations[reposync.TriggerAnnotation] == "" {
		t.Error("expected scm-sync-trigger annotation set via composite id")
	}
}

// TestGithubSync_CrossOriginRejected pins the sameOriginGuard on this mutation
// (Cursor review, ISI-4662 finding 4): widening the route to the composite id is
// what makes the console's path here reachable, so a top-level cross-site POST —
// which rides SameSite=Lax — must be rejected before the annotation patch.
func TestGithubSync_CrossOriginRejected(t *testing.T) {
	teamID := uuid.New()
	ns := teamID.String()
	tm := team(ns, "my-team", teamID.String())
	proj := project(ns, "myproject", "")
	c := buildSyncClient(t, tm, proj)
	svc := NewGithubSyncService(c, c)

	srv := testGithubSyncServer(t, teamID, svc)
	req := httptest.NewRequest(http.MethodPost, "/api/projects/myproject/github/sync", nil)
	req.AddCookie(&http.Cookie{Name: "ksquad_session", Value: devToken})
	req.Header.Set("Origin", "https://evil.example.com")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST: want 403, got %d: %s", w.Code, w.Body.String())
	}
	var got ksquadv1.Project
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "myproject"}, &got); err != nil {
		t.Fatalf("get project: %v", err)
	}
	if got.Annotations[reposync.TriggerAnnotation] != "" {
		t.Error("cross-origin POST must not set the scm-sync-trigger annotation")
	}
}

func TestGithubSync_Debounce(t *testing.T) {
	teamID := uuid.New()
	ns := teamID.String()
	tm := team(ns, "my-team", teamID.String())
	proj := project(ns, "myproject", "")
	c := buildSyncClient(t, tm, proj)
	svc := NewGithubSyncService(c, c)

	srv := testGithubSyncServer(t, teamID, svc)

	do := func() int {
		req := httptest.NewRequest(http.MethodPost, "/api/projects/myproject/github/sync", nil)
		req.AddCookie(&http.Cookie{Name: "ksquad_session", Value: devToken})
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		return w.Code
	}

	if s := do(); s != http.StatusAccepted {
		t.Fatalf("first call: want 202, got %d", s)
	}
	if s := do(); s != http.StatusTooManyRequests {
		t.Fatalf("second call: want 429 (debounce), got %d", s)
	}
}

func TestGithubSync_ForeignProject404(t *testing.T) {
	teamID := uuid.New()
	otherNS := uuid.New().String()
	// Team in caller's namespace, Project in a different namespace.
	tm := team(teamID.String(), "my-team", teamID.String())
	proj := project(otherNS, "foreign", "")
	c := buildSyncClient(t, tm, proj)
	svc := NewGithubSyncService(c, c)

	srv := testGithubSyncServer(t, teamID, svc)
	req := httptest.NewRequest(http.MethodPost, "/api/projects/foreign/github/sync", nil)
	req.AddCookie(&http.Cookie{Name: "ksquad_session", Value: devToken})
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGithubSync_NilService501(t *testing.T) {
	teamID := uuid.New()
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken: {Principal: "user:alice", TeamID: teamID},
	}}
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		GithubSync:    nil, // not wired
	}).Handler()

	req := httptest.NewRequest(http.MethodPost, "/api/projects/x/github/sync", nil)
	req.AddCookie(&http.Cookie{Name: "ksquad_session", Value: devToken})
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("want 501, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "github-sync trigger") {
		t.Errorf("want not-implemented body, got %s", w.Body.String())
	}
}
