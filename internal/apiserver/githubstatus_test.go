package apiserver

// githubstatus_test.go — ISI-3956 S5b: the GitHub-status read model over the
// scm mirror. Covers:
//   AC1  mirror projection: PRs(review_state)/issues/check-runs(conclusion)/
//        artifacts/releases from seeded mirror rows,
//   AC2/AC6  no GitHub in the request path / no credential in the response
//        (the reader depends ONLY on the mirror store),
//   AC3  honest freshness passthrough from Project.Status.Sync,
//   AC4  tenancy: foreign-team 404 (existence-hiding), global-admin 200,
//   AC5  nil reader ⇒ documented 501.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/scm"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// --- fixtures -----------------------------------------------------------------------------------

func testGithubStatusServer(t *testing.T, teamID uuid.UUID, reader client.Reader, mirror scm.MirrorReader) http.Handler {
	t.Helper()
	return testGithubStatusServerAs(t, teamID, discussion.AuthorContext{}, reader, mirror)
}

// testGithubStatusServerAs wires both a non-admin devToken session (scoped to
// teamID) and an admin adminDashToken session, so one server exercises both.
func testGithubStatusServerAs(t *testing.T, teamID uuid.UUID, admin discussion.AuthorContext, reader client.Reader, mirror scm.MirrorReader) http.Handler {
	t.Helper()
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken:       {Principal: "user:alice", TeamID: teamID},
		adminDashToken: admin,
	}}
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		GithubStatus:  NewGithubStatusService(reader, mirror),
	})
	return srv.Handler()
}

func getGithubStatusAs(t *testing.T, h http.Handler, token, projectID string) (*httptest.ResponseRecorder, *GithubStatus) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/github", nil), token))
	if rec.Code != http.StatusOK {
		return rec, nil
	}
	var st GithubStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return rec, &st
}

// seedMirror applies rows to an in-memory mirror scoped to (ns, name).
func seedMirror(t *testing.T, store *scm.InMemoryMirrorStore, ns, name string, rows ...scm.MirrorRow) {
	t.Helper()
	if _, err := store.ApplySnapshot(context.Background(), ns, name, rows); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}
}

func mirrorRow(kind scm.RecordType, extID, state, title, actor string, p scm.MirrorPayload) scm.MirrorRow {
	raw, _ := json.Marshal(p)
	return scm.MirrorRow{Kind: kind, ExternalID: extID, State: state, Title: title, Actor: actor, Payload: raw}
}

// --- AC1 + AC3: projection + freshness ----------------------------------------------------------

func TestGithubStatusProjection(t *testing.T) {
	teamID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	synced := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	updated := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

	proj := project("squad-a", "web", "https://github.com/acme/web")
	proj.Status.Sync = &ksquadv1.ProjectSyncStatus{
		LastMirrorTime:    &metav1.Time{Time: synced},
		LastWebhookTime:   &metav1.Time{Time: synced},
		MirrorRecordCount: 5,
	}
	reader := newDashboardClient(t, team("squad-a", "alpha", teamID.String()), proj)

	store := scm.NewInMemoryMirrorStore()
	seedMirror(t, store, "squad-a", "web",
		mirrorRow(scm.RecordTypePR, "7", "open", "add feature", "dev",
			scm.MirrorPayload{Number: 7, URL: "https://github.com/acme/web/pull/7", HeadRef: "feat/x", UpdatedAt: updated}),
		mirrorRow(scm.RecordTypePR, "8", "closed", "merged one", "dev",
			scm.MirrorPayload{Number: 8, URL: "https://github.com/acme/web/pull/8", Merged: true}),
		mirrorRow(scm.RecordTypeIssue, "3", "open", "a bug", "dev",
			scm.MirrorPayload{Number: 3, URL: "https://github.com/acme/web/issues/3"}),
		mirrorRow(scm.RecordTypeCheckRun, "101", "completed", "ci", "",
			scm.MirrorPayload{URL: "https://github.com/acme/web/runs/101", Conclusion: "success"}),
		mirrorRow(scm.RecordTypeArtifact, "55", "", "logs", "",
			scm.MirrorPayload{URL: "https://x/artifact/55", Size: 4096, CreatedAt: updated}),
		mirrorRow(scm.RecordTypeRelease, "9", "published", "v1.0.0", "dev",
			scm.MirrorPayload{URL: "https://github.com/acme/web/releases/tag/v1.0.0", HeadRef: "v1.0.0", CreatedAt: synced}),
	)

	h := testGithubStatusServer(t, teamID, reader, store)
	rec, st := getGithubStatusAs(t, h, devToken, "web")
	if st == nil {
		t.Fatalf("projection: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	if st.Project.Name != "web" || st.Project.Namespace != "squad-a" {
		t.Fatalf("scope: %+v", st.Project)
	}
	// PRs: open ⇒ ready-for-review; closed+merged ⇒ merged.
	if len(st.PullRequests) != 2 {
		t.Fatalf("want 2 PRs, got %+v", st.PullRequests)
	}
	prByNum := map[int]GithubPR{}
	for _, pr := range st.PullRequests {
		prByNum[pr.Number] = pr
	}
	if got := prByNum[7]; got.ReviewState != PRReadyForReview || got.Branch != "feat/x" || got.URL == "" || got.UpdatedAt == nil {
		t.Errorf("open PR projected wrong: %+v", got)
	}
	if got := prByNum[8]; got.ReviewState != PRMerged || !got.Merged {
		t.Errorf("merged PR projected wrong: %+v", got)
	}
	// Issue.
	if len(st.Issues) != 1 || st.Issues[0].Number != 3 || st.Issues[0].State != "open" {
		t.Errorf("issue projected wrong: %+v", st.Issues)
	}
	// Check-run conclusion.
	if len(st.CheckRuns) != 1 || st.CheckRuns[0].Conclusion != "success" || st.CheckRuns[0].Name != "ci" {
		t.Errorf("check-run projected wrong: %+v", st.CheckRuns)
	}
	// Artifact size + url.
	if len(st.Artifacts) != 1 || st.Artifacts[0].SizeBytes != 4096 || st.Artifacts[0].URL == "" {
		t.Errorf("artifact projected wrong: %+v", st.Artifacts)
	}
	// Release: tag on Tag, state published.
	if len(st.Releases) != 1 || st.Releases[0].Tag != "v1.0.0" || st.Releases[0].State != "published" {
		t.Errorf("release projected wrong: %+v", st.Releases)
	}
	// AC3 freshness passthrough.
	if st.Freshness.LastMirrorTime == nil || !st.Freshness.LastMirrorTime.Equal(synced) || st.Freshness.MirrorRecordCount != 5 {
		t.Errorf("freshness projected wrong: %+v", st.Freshness)
	}

	// AC2/AC6: the response must never carry a credential/secret/token field.
	body := rec.Body.String()
	for _, forbidden := range []string{"credential", "secret", "token", "password"} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Errorf("response leaked %q: %s", forbidden, body)
		}
	}
}

// --- AC4: tenancy ------------------------------------------------------------------------------

func TestGithubStatusForeignProjectIs404(t *testing.T) {
	teamID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	reader := newDashboardClient(t,
		team("squad-a", "alpha", teamID.String()),
		project("squad-b", "web", "https://github.com/acme/web"), // foreign namespace
	)
	store := scm.NewInMemoryMirrorStore()
	h := testGithubStatusServer(t, teamID, reader, store)

	for _, projectID := range []string{"web", "no-such-project"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/github", nil), devToken))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("project %q: got %d, want 404", projectID, rec.Code)
		}
	}
}

func TestGithubStatusAdminForeignProject200(t *testing.T) {
	admin := discussion.AuthorContext{Principal: "user:root", TeamID: uuid.Nil, IsAdmin: true}
	reader := newDashboardClient(t,
		team("squad-b", "beta", "cccccccc-cccc-cccc-cccc-cccccccccccc"),
		project("squad-b", "web", "https://github.com/acme/web"),
	)
	store := scm.NewInMemoryMirrorStore()
	seedMirror(t, store, "squad-b", "web",
		mirrorRow(scm.RecordTypeIssue, "1", "open", "issue", "dev", scm.MirrorPayload{Number: 1}))
	h := testGithubStatusServerAs(t, uuid.New(), admin, reader, store)

	rec, st := getGithubStatusAs(t, h, adminDashToken, "web")
	if st == nil {
		t.Fatalf("admin foreign project: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if st.Project.Namespace != "squad-b" || len(st.Issues) != 1 {
		t.Fatalf("admin projection wrong: %+v", st)
	}
}

// --- AC5: nil reader ⇒ 501 ----------------------------------------------------------------------

func TestGithubStatusNotWired501(t *testing.T) {
	teamID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken: {Principal: "user:alice", TeamID: teamID},
	}}
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		// GithubStatus nil ⇒ documented 501.
	})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/projects/web/github", nil), devToken))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("nil github-status: got %d, want 501", rec.Code)
	}
}

// TestGithubStatusUnauthenticated — no session cookie ⇒ 401 at the choke point.
func TestGithubStatusUnauthenticated(t *testing.T) {
	teamID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	reader := newDashboardClient(t, team("squad-a", "alpha", teamID.String()))
	h := testGithubStatusServer(t, teamID, reader, scm.NewInMemoryMirrorStore())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/projects/web/github", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: got %d, want 401", rec.Code)
	}
}
