package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
)

func overviewScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := ksquadv1.AddToScheme(s); err != nil {
		t.Fatalf("register scheme: %v", err)
	}
	return s
}

func team(ns, name, uid string) *ksquadv1.Team {
	return &ksquadv1.Team{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(uid)},
		Spec:       ksquadv1.TeamSpec{NamespaceStrategy: "isolated"},
	}
}

func project(ns, name, repo string) *ksquadv1.Project {
	return &ksquadv1.Project{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       ksquadv1.ProjectSpec{Repo: ksquadv1.RepoSpec{URL: repo}},
	}
}

func run(ns, name, projectName, workItem string, phase ksquadv1.RunPhase, claimed *time.Time) *ksquadv1.Run {
	r := &ksquadv1.Run{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: ksquadv1.RunSpec{
			ProjectRef:  ksquadv1.ObjectRef{Name: projectName},
			WorkItemRef: workItem,
		},
		Status: ksquadv1.RunStatus{Phase: phase},
	}
	if claimed != nil {
		r.Status.ClaimedAt = &metav1.Time{Time: *claimed}
	}
	return r
}

func newReader(t *testing.T, objs ...client.Object) *ClientOverviewReader {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(overviewScheme(t)).WithObjects(objs...).Build()
	return NewClientOverviewReader(c)
}

// TestOverviewProjection — the happy path: Teams→Projects→Runs projected, grouped, phase-rolled-up,
// deterministically sorted, with the Pending coalesce and ClaimedAt carried through.
func TestOverviewProjection(t *testing.T) {
	const teamUID = "11111111-1111-1111-1111-111111111111"
	claimed := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	r := newReader(t,
		team("squad-a", "alpha", teamUID),
		project("squad-a", "web", "https://github.com/acme/web"),
		project("squad-a", "api", "https://github.com/acme/api"),
		run("squad-a", "run-2", "web", "ISI-2", ksquadv1.RunPhaseRunning, &claimed),
		run("squad-a", "run-1", "web", "ISI-1", ksquadv1.RunPhaseSucceeded, nil),
		run("squad-a", "run-3", "api", "ISI-3", "", nil), // empty phase ⇒ coalesced to Pending
	)

	ov, err := r.Overview(context.Background(), teamUID)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if ov.Team.Name != "alpha" || ov.Team.Namespace != "squad-a" || ov.Team.UID != teamUID {
		t.Fatalf("team ref: %+v", ov.Team)
	}
	if len(ov.Projects) != 2 {
		t.Fatalf("projects: got %d, want 2", len(ov.Projects))
	}
	// Sorted by name: api before web.
	if ov.Projects[0].Name != "api" || ov.Projects[1].Name != "web" {
		t.Fatalf("project order: %s, %s", ov.Projects[0].Name, ov.Projects[1].Name)
	}

	api := ov.Projects[0]
	if len(api.Runs) != 1 || api.Runs[0].Phase != string(ksquadv1.RunPhasePending) {
		t.Fatalf("api runs: %+v (empty phase must coalesce to Pending)", api.Runs)
	}
	if api.PhaseCounts["Pending"] != 1 {
		t.Fatalf("api phaseCounts: %+v", api.PhaseCounts)
	}

	web := ov.Projects[1]
	if len(web.Runs) != 2 {
		t.Fatalf("web runs: got %d, want 2", len(web.Runs))
	}
	// Runs sorted by name: run-1 before run-2.
	if web.Runs[0].Name != "run-1" || web.Runs[1].Name != "run-2" {
		t.Fatalf("run order: %s, %s", web.Runs[0].Name, web.Runs[1].Name)
	}
	if web.Runs[0].Phase != "Succeeded" || web.Runs[1].Phase != "Running" {
		t.Fatalf("run phases: %+v", web.Runs)
	}
	if web.Runs[1].ClaimedAt == nil || !web.Runs[1].ClaimedAt.Equal(claimed) {
		t.Fatalf("run-2 claimedAt: %+v", web.Runs[1].ClaimedAt)
	}
	if web.Runs[0].ClaimedAt != nil {
		t.Fatalf("run-1 must have nil claimedAt: %+v", web.Runs[0].ClaimedAt)
	}
	if web.PhaseCounts["Running"] != 1 || web.PhaseCounts["Succeeded"] != 1 {
		t.Fatalf("web phaseCounts: %+v", web.PhaseCounts)
	}
}

// TestOverviewTeamScopeIsolation — the read model NEVER leaks another Team's namespace. Team B's
// project/run in a different namespace must not appear in Team A's overview, and Team B's UID
// resolves only to Team B.
func TestOverviewTeamScopeIsolation(t *testing.T) {
	const uidA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const uidB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	r := newReader(t,
		team("squad-a", "alpha", uidA),
		team("squad-b", "beta", uidB),
		project("squad-a", "web", "https://github.com/acme/web"),
		project("squad-b", "secret", "https://github.com/acme/secret"),
		run("squad-a", "run-a", "web", "ISI-A", ksquadv1.RunPhaseRunning, nil),
		run("squad-b", "run-b", "secret", "ISI-B", ksquadv1.RunPhaseRunning, nil),
	)

	ovA, err := r.Overview(context.Background(), uidA)
	if err != nil {
		t.Fatalf("Overview A: %v", err)
	}
	if len(ovA.Projects) != 1 || ovA.Projects[0].Name != "web" {
		t.Fatalf("team A leaked cross-tenant projects: %+v", ovA.Projects)
	}

	ovB, err := r.Overview(context.Background(), uidB)
	if err != nil {
		t.Fatalf("Overview B: %v", err)
	}
	if ovB.Team.Name != "beta" || len(ovB.Projects) != 1 || ovB.Projects[0].Name != "secret" {
		t.Fatalf("team B scope wrong: %+v", ovB)
	}
}

// TestOverviewOrphanRunDropped — a Run referencing a Project absent from the namespace is not
// placed under any Project row (inconsistent reference, not a dashboard cell).
func TestOverviewOrphanRunDropped(t *testing.T) {
	const teamUID = "22222222-2222-2222-2222-222222222222"
	r := newReader(t,
		team("squad-a", "alpha", teamUID),
		project("squad-a", "web", "https://github.com/acme/web"),
		run("squad-a", "run-x", "ghost", "ISI-X", ksquadv1.RunPhaseRunning, nil),
	)
	ov, err := r.Overview(context.Background(), teamUID)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if len(ov.Projects) != 1 || len(ov.Projects[0].Runs) != 0 {
		t.Fatalf("orphan run must be dropped: %+v", ov.Projects)
	}
}

// TestOverviewTeamNotFound — an unknown or empty Team scope yields ErrTeamNotFound (→ 404), never a
// blank overview or another team's data.
func TestOverviewTeamNotFound(t *testing.T) {
	r := newReader(t, team("squad-a", "alpha", "33333333-3333-3333-3333-333333333333"))
	for _, uid := range []string{"", "99999999-9999-9999-9999-999999999999"} {
		if _, err := r.Overview(context.Background(), uid); !errors.Is(err, ErrTeamNotFound) {
			t.Fatalf("uid %q: got err %v, want ErrTeamNotFound", uid, err)
		}
	}
}

// --- handler / server wiring ---------------------------------------------------------------------

func testOverviewServer(t *testing.T, teamID uuid.UUID, reader SquadOverviewReader) http.Handler {
	t.Helper()
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken: {Principal: "user:alice", TeamID: teamID},
	}}
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		Overview:      reader,
	})
	return srv.Handler()
}

// TestSquadOverviewHandlerOK — a session whose Team scope resolves serves 200 + the projection.
func TestSquadOverviewHandlerOK(t *testing.T) {
	teamID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	reader := newReader(t,
		team("squad-a", "alpha", teamID.String()),
		project("squad-a", "web", "https://github.com/acme/web"),
	)
	h := testOverviewServer(t, teamID, reader)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/squad/overview", nil), devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("overview: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var ov SquadOverview
	if err := json.Unmarshal(rec.Body.Bytes(), &ov); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ov.Team.Name != "alpha" || len(ov.Projects) != 1 {
		t.Fatalf("body: %+v", ov)
	}
}

// TestSquadOverviewHandlerTeamNotFound — an authenticated caller whose Team has no projection gets
// 404, distinct from the 401 an unauthenticated caller gets.
func TestSquadOverviewHandlerTeamNotFound(t *testing.T) {
	teamID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	reader := newReader(t, team("squad-a", "alpha", "66666666-6666-6666-6666-666666666666"))
	h := testOverviewServer(t, teamID, reader)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/squad/overview", nil), devToken))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("overview(no team): got %d, want 404", rec.Code)
	}
}

// TestSquadOverviewHandlerUnauthenticated — no session cookie ⇒ 401 at the choke point, before the
// read model runs.
func TestSquadOverviewHandlerUnauthenticated(t *testing.T) {
	teamID := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	reader := newReader(t, team("squad-a", "alpha", teamID.String()))
	h := testOverviewServer(t, teamID, reader)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/squad/overview", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("overview(no session): got %d, want 401", rec.Code)
	}
}

// TestSquadOverviewNilReaderStill501 — with no read model wired the route keeps its documented 501
// (honest contract for a cluster-less run), NOT a 404 or panic.
func TestSquadOverviewNilReaderStill501(t *testing.T) {
	teamID := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	h := testOverviewServer(t, teamID, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/squad/overview", nil), devToken))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("overview(nil reader): got %d, want 501", rec.Code)
	}
}
