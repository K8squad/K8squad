package apiserver

// projectsettings_test.go — S1 (ISI-3994 / ISI-3999): the per-Project settings
// read model behind GET /api/projects/{projectId}/settings.
//
// Covers the 7 ACs:
//   AC1  repo config projection (sync set + sync nil → provider default);
//   AC2  auth.connected honest + no token/Secret data ever in the body;
//   AC3  lastTest tri-state (passed | failed | untested) from the Team annotation;
//   AC4  cross-tenant read hidden (404), matching the dashboard;
//   AC5  global-admin fleet-wide read of a foreign-namespace Project;
//   AC6  canEdit agrees with the compose write-tier gate (editor true / viewer false);
//   AC7  cluster-less nil service keeps the documented 501.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/auth"
)

// --- fixtures -----------------------------------------------------------------------------------

// settingsProject builds a Project with a fully-specified repo (ref, auth ref,
// and sync) so the projection can be asserted field-by-field.
func settingsProject(ns, name, url, ref, credRef string, sync *ksquadv1.RepoSyncSpec) *ksquadv1.Project {
	p := &ksquadv1.Project{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       ksquadv1.ProjectSpec{Repo: ksquadv1.RepoSpec{URL: url, Ref: ref, Sync: sync}},
	}
	if credRef != "" {
		p.Spec.Repo.Auth = &ksquadv1.RepoAuth{CredentialSecretRef: ksquadv1.SecretRef{Name: credRef}}
	}
	return p
}

// teamWithRepoTest builds a Team CR carrying the AD-7 repo test-connection
// annotation (passed | failed), or none when result is "".
func teamWithRepoTest(ns, name, uid, result string) *ksquadv1.Team {
	tm := team(ns, name, uid)
	if result != "" {
		tm.Annotations = map[string]string{RepoTestConnectionAnnotation: result}
	}
	return tm
}

// testSettingsServer mounts the settings route for the non-admin devToken caller
// scoped to teamID. roles is wired to BOTH the service (canEdit) and the route
// gate (requireProjectRole) — nil ⇒ no membership gate and canEdit false, the
// posture the AC1–AC4 projection/tenancy tests use.
func testSettingsServer(t *testing.T, teamID uuid.UUID, reader client.Reader, roles ProjectRoleResolver) http.Handler {
	t.Helper()
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken: {Principal: "user:alice", TeamID: teamID},
	}}
	srv := NewServer(Options{
		Authenticator:   NewCookieAuthenticator(resolver),
		Discussion:      discussion.NewHandler(nil),
		ProjectSettings: NewProjectSettingsService(reader, roles),
		ProjectRoles:    roles,
	})
	return srv.Handler()
}

// getSettings GETs the settings route under a token, returning the raw recorder
// (caller asserts status) and the raw body, decoding into ProjectSettings only
// on 200.
func getSettings(t *testing.T, h http.Handler, token, projectID string) (*httptest.ResponseRecorder, string, *ProjectSettings) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/settings", nil), token))
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		return rec, body, nil
	}
	var out ProjectSettings
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode settings: %v (body %s)", err, body)
	}
	return rec, body, &out
}

// --- AC1: repo config projection ----------------------------------------------------------------

// TestSettingsProjectionSyncSet — a Project with repo.sync set surfaces the
// provider/poll/reflect fields and syncEnabled true.
func TestSettingsProjectionSyncSet(t *testing.T) {
	teamID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	sync := &ksquadv1.RepoSyncSpec{Provider: "gitlab", PollIntervalSeconds: 120, ReflectOutbound: true}
	reader := newDashboardClient(t,
		team("squad-a", "alpha", teamID.String()),
		settingsProject("squad-a", "web", "https://gitlab.com/acme/web", "release", "web-pat", sync),
	)
	h := testSettingsServer(t, teamID, reader, nil)

	_, _, s := getSettings(t, h, devToken, "web")
	if s == nil {
		t.Fatal("want 200 projection")
	}
	if s.Project.Name != "web" || s.Project.Namespace != "squad-a" {
		t.Fatalf("project ref: %+v", s.Project)
	}
	if s.Repo.URL != "https://gitlab.com/acme/web" || s.Repo.Ref != "release" {
		t.Fatalf("repo url/ref: %+v", s.Repo)
	}
	if !s.Repo.SyncEnabled || s.Repo.Provider != "gitlab" || s.Repo.PollIntervalSeconds != 120 || !s.Repo.ReflectOutbound {
		t.Fatalf("sync projection: %+v", s.Repo)
	}
}

// TestSettingsProjectionSyncNil — a connected repo with sync nil surfaces
// provider "github" (the v1 default, never blank) and syncEnabled false, and an
// unset ref projects as the empty string.
func TestSettingsProjectionSyncNil(t *testing.T) {
	teamID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	reader := newDashboardClient(t,
		team("squad-a", "alpha", teamID.String()),
		settingsProject("squad-a", "web", "https://github.com/acme/web", "", "", nil),
	)
	h := testSettingsServer(t, teamID, reader, nil)

	_, _, s := getSettings(t, h, devToken, "web")
	if s == nil {
		t.Fatal("want 200 projection")
	}
	if s.Repo.SyncEnabled {
		t.Fatalf("syncEnabled must be false when sync is nil: %+v", s.Repo)
	}
	if s.Repo.Provider != "github" {
		t.Fatalf("provider must default to github when sync nil, got %q", s.Repo.Provider)
	}
	if s.Repo.Ref != "" {
		t.Fatalf("unset ref must project as empty string, got %q", s.Repo.Ref)
	}
	if s.Repo.PollIntervalSeconds != 0 || s.Repo.ReflectOutbound {
		t.Fatalf("nil-sync must zero poll/reflect: %+v", s.Repo)
	}
}

// --- AC2: auth honest, token never leaves -------------------------------------------------------

// TestSettingsAuthConnected — a project with a credential ref reports connected
// with the ref NAME, and the marshalled body carries NO Secret data/token field.
func TestSettingsAuthConnected(t *testing.T) {
	teamID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	reader := newDashboardClient(t,
		team("squad-a", "alpha", teamID.String()),
		settingsProject("squad-a", "web", "https://github.com/acme/web", "", "web-scm-cred", nil),
	)
	h := testSettingsServer(t, teamID, reader, nil)

	_, body, s := getSettings(t, h, devToken, "web")
	if s == nil {
		t.Fatal("want 200 projection")
	}
	if !s.Auth.Connected || s.Auth.CredentialSecretRefName != "web-scm-cred" {
		t.Fatalf("auth projection: %+v", s.Auth)
	}
	// No token material ever crosses the boundary (AC2 / NFR-SEC8): the body must
	// carry neither a Secret "data" field nor any "token" key.
	if strings.Contains(body, `"data"`) {
		t.Fatalf("body must not contain a Secret data field: %s", body)
	}
	if strings.Contains(strings.ToLower(body), "token") {
		t.Fatalf("body must not contain a token field/value: %s", body)
	}
}

// TestSettingsAuthDisconnected — spec.repo.auth nil reports not-connected with an
// empty ref name.
func TestSettingsAuthDisconnected(t *testing.T) {
	teamID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	reader := newDashboardClient(t,
		team("squad-a", "alpha", teamID.String()),
		settingsProject("squad-a", "web", "https://github.com/acme/web", "", "", nil),
	)
	h := testSettingsServer(t, teamID, reader, nil)

	_, _, s := getSettings(t, h, devToken, "web")
	if s == nil {
		t.Fatal("want 200 projection")
	}
	if s.Auth.Connected || s.Auth.CredentialSecretRefName != "" {
		t.Fatalf("auth must be disconnected with empty ref: %+v", s.Auth)
	}
}

// --- AC3: lastTest tri-state --------------------------------------------------------------------

func TestSettingsLastTestTriState(t *testing.T) {
	cases := []struct {
		annotation string
		want       string
	}{
		{"passed", RepoTestPassed},
		{"failed", RepoTestFailed},
		{"", RepoTestUntested}, // annotation absent ⇒ never fabricate "passed"
	}
	for _, tc := range cases {
		t.Run("annotation="+tc.annotation, func(t *testing.T) {
			teamID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
			reader := newDashboardClient(t,
				teamWithRepoTest("squad-a", "alpha", teamID.String(), tc.annotation),
				// A connected credential ref must NOT by itself imply "passed" (AC3).
				settingsProject("squad-a", "web", "https://github.com/acme/web", "", "web-pat", nil),
			)
			h := testSettingsServer(t, teamID, reader, nil)

			_, _, s := getSettings(t, h, devToken, "web")
			if s == nil {
				t.Fatal("want 200 projection")
			}
			if s.Auth.LastTest != tc.want {
				t.Fatalf("lastTest: got %q, want %q", s.Auth.LastTest, tc.want)
			}
		})
	}
}

// --- AC4: cross-tenant read hidden --------------------------------------------------------------

// TestSettingsForeignProjectIs404 — a Project in another Team's namespace, and a
// wholly unknown name, both 404 (existence-hiding), exactly like the dashboard.
func TestSettingsForeignProjectIs404(t *testing.T) {
	teamID := uuid.MustParse("66666666-6666-6666-6666-666666666666")
	reader := newDashboardClient(t,
		team("squad-a", "alpha", teamID.String()),
		project("squad-b", "web", "https://github.com/acme/web"), // different Team namespace
	)
	h := testSettingsServer(t, teamID, reader, nil)

	for _, projectID := range []string{"web", "no-such-project"} {
		rec, _, _ := getSettings(t, h, devToken, projectID)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("project %q: got %d, want 404", projectID, rec.Code)
		}
	}
}

// TestSettingsUnauthenticated — no session cookie ⇒ 401 at the choke point.
func TestSettingsUnauthenticated(t *testing.T) {
	teamID := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	reader := newDashboardClient(t, team("squad-a", "alpha", teamID.String()))
	h := testSettingsServer(t, teamID, reader, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/projects/web/settings", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: got %d, want 401", rec.Code)
	}
}

// --- AC5: global-admin fleet-wide read ----------------------------------------------------------

// TestSettingsAdminForeignProject — an admin (no home Team) reads a Project in a
// foreign namespace via the fleet-wide resolve path; canEdit is true (admin is
// supra-tenant write authority).
func TestSettingsAdminForeignProject(t *testing.T) {
	admin := discussion.AuthorContext{Principal: "user:root", TeamID: uuid.Nil, IsAdmin: true}
	const adminToken = "admin-token-xyz"
	reader := newDashboardClient(t,
		team("squad-b", "beta", "cccccccc-cccc-cccc-cccc-cccccccccccc"),
		settingsProject("squad-b", "web", "https://github.com/acme/web", "main", "web-pat", nil),
	)
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		adminToken: admin,
	}}
	srv := NewServer(Options{
		Authenticator:   NewCookieAuthenticator(resolver),
		Discussion:      discussion.NewHandler(nil),
		ProjectSettings: NewProjectSettingsService(reader, fakeRoleResolver{}),
		ProjectRoles:    fakeRoleResolver{},
	})
	h := srv.Handler()

	_, _, s := getSettings(t, h, adminToken, "web")
	if s == nil {
		t.Fatal("admin fleet read: want 200 projection")
	}
	if s.Project.Namespace != "squad-b" || s.Project.Name != "web" {
		t.Fatalf("admin must resolve foreign project: %+v", s.Project)
	}
	if !s.CanEdit {
		t.Fatalf("admin canEdit must be true (supra-tenant write authority)")
	}
}

// --- AC6: canEdit agrees with the write-tier gate -----------------------------------------------

// TestSettingsCanEditMatchesWriteTier — canEdit is true for a contributor (who
// would be authorized to PUT the Project) and false for a read-only viewer, using
// the SAME resolver the compose write path consults (agree by construction).
func TestSettingsCanEditMatchesWriteTier(t *testing.T) {
	cases := []struct {
		role string
		want bool
	}{
		{auth.ProjectRoleContributor, true},
		{auth.ProjectRoleMaintainer, true},
		{auth.ProjectRoleViewer, false},
	}
	for _, tc := range cases {
		t.Run("role="+tc.role, func(t *testing.T) {
			teamID := uuid.MustParse("88888888-8888-8888-8888-888888888888")
			roles := fakeRoleResolver{roles: map[string]map[string]string{
				"user:alice": {"web": tc.role},
			}}
			reader := newDashboardClient(t,
				team("squad-a", "alpha", teamID.String()),
				settingsProject("squad-a", "web", "https://github.com/acme/web", "", "web-pat", nil),
			)
			h := testSettingsServer(t, teamID, reader, roles)

			rec, body, s := getSettings(t, h, devToken, "web")
			if s == nil {
				t.Fatalf("want 200 (got %d, body %s)", rec.Code, body)
			}
			if s.CanEdit != tc.want {
				t.Fatalf("role %q: canEdit=%v, want %v", tc.role, s.CanEdit, tc.want)
			}
		})
	}
}

// --- AC7: cluster-less 501 ----------------------------------------------------------------------

// TestSettingsNotWired501 — a nil ProjectSettings service keeps the documented
// 501 contract (route exists; backing pending), mirroring the dashboard.
func TestSettingsNotWired501(t *testing.T) {
	teamID := uuid.MustParse("99999999-9999-9999-9999-999999999999")
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken: {Principal: "user:alice", TeamID: teamID},
	}}
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		// ProjectSettings nil ⇒ documented 501.
	})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/projects/web/settings", nil), devToken))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("nil settings: got %d, want 501", rec.Code)
	}
}
