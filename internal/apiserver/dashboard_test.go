package apiserver

// dashboard_test.go — story 8.8a/ISI-2906: the per-Project dashboard read model.
//
// Covers the three AC families of 8.8a:
//   AC1  one composed payload from real sources (coord seam, scm seam, metrics
//        seam, Run/claim state),
//   AC2  server-side scoping to the caller's Team (choke-point 401/404 — no
//        dashboard-specific authz path),
//   AC3  per-tile degradation: a nil OR failing source degrades ONLY its tile,
//        never the whole dashboard (and never a fake number).

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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
)

// --- fake seams ---------------------------------------------------------------------------------

type fakeTicketSource struct {
	facts TicketFacts
	err   error
}

func (f *fakeTicketSource) TicketFacts(_ context.Context, _, _, _ string) (TicketFacts, error) {
	return f.facts, f.err
}

type fakePRSource struct {
	prs []PullRequest
	err error
}

func (f *fakePRSource) PullRequests(_ context.Context, _, _ string) ([]PullRequest, error) {
	return f.prs, f.err
}

type fakeMetricsSource struct {
	total    int64
	cost     *float64
	currency string
	trend    []TokenTrendPoint
	err      error
}

func (f *fakeMetricsSource) TokenConsumption(_ context.Context, _, _ string) (int64, *float64, string, []TokenTrendPoint, error) {
	return f.total, f.cost, f.currency, f.trend, f.err
}

// --- fixtures -----------------------------------------------------------------------------------

func newDashboardClient(t *testing.T, objs ...client.Object) client.Reader {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(overviewScheme(t)).WithObjects(objs...).Build()
}

func dashboardRun(ns, name, projectName, agent, workItem string, phase ksquadv1.RunPhase) *ksquadv1.Run {
	r := run(ns, name, projectName, workItem, phase, nil)
	if agent != "" {
		r.Spec.Agents = []ksquadv1.ObjectRef{{Name: agent}}
	}
	return r
}

func testDashboardServer(t *testing.T, teamID uuid.UUID, reader client.Reader, tickets TicketSource, prs PRSource, metrics MetricsSource) http.Handler {
	t.Helper()
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken: {Principal: "user:alice", TeamID: teamID},
	}}
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		Dashboard:     NewDashboardService(reader, tickets, prs, metrics),
	})
	return srv.Handler()
}

func getDashboard(t *testing.T, h http.Handler, projectID string) (*httptest.ResponseRecorder, *ProjectDashboard) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/dashboard", nil), devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var dash ProjectDashboard
	if err := json.Unmarshal(rec.Body.Bytes(), &dash); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return rec, &dash
}

// --- AC1: composition ---------------------------------------------------------------------------

// TestDashboardComposesAllSources — every wired source lands in ONE payload with its tile
// available, and the live-Runs tile projects Run/claim state (agent mapping, phase coalescing,
// 13.9 indicators from conditions).
func TestDashboardComposesAllSources(t *testing.T) {
	teamID := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	claimed := time.Now().Add(-10 * time.Minute).UTC().Truncate(time.Second)
	resume := time.Now().Add(3 * time.Minute).UTC().Truncate(time.Second)

	paused := dashboardRun("squad-a", "run-paused", "web", "agent-b", "T2", ksquadv1.RunPhasePaused)
	paused.Status.ClaimedAt = &metav1.Time{Time: claimed}
	paused.Status.Conditions = []metav1.Condition{
		{Type: "Paused", Reason: "rate_limited", Message: "429 from provider; resume_at=" + resume.Format(time.RFC3339)},
		{Type: "Fallback", Reason: "gpt-4o-mini", Message: "fallback active"},
	}
	blank := dashboardRun("squad-a", "run-blank", "web", "", "T3", "") // reconciler has not observed it
	foreign := dashboardRun("squad-a", "run-foreign", "other-proj", "agent-c", "T9", ksquadv1.RunPhaseRunning)

	reader := newDashboardClient(t,
		team("squad-a", "alpha", teamID.String()),
		project("squad-a", "web", "https://github.com/acme/web"),
		dashboardRun("squad-a", "run-live", "web", "agent-a", "T1", ksquadv1.RunPhaseRunning),
		paused, blank, foreign,
	)

	upd := time.Now().UTC().Truncate(time.Second)
	h := testDashboardServer(t, teamID, reader,
		&fakeTicketSource{facts: TicketFacts{
			ByStatus: map[string]int{"open": 3, "done": 5},
			Recent:   []TicketSummary{{ID: "T1", Title: "Fix login", Status: "open", UpdatedAt: &upd}},
			PendingApprovals: []PendingApproval{{TicketID: "T4", Title: "Deploy to prod", RequestingAgent: "agent-a", RunID: "run-live", RaisedAt: &upd}},
			CanAct:  true,
		}},
		&fakePRSource{prs: []PullRequest{
			{Number: 12, Title: "Fix cache", ReviewState: PRReadyForReview, Branch: "fix/cache"},
			{Number: 11, Title: "WIP spike", ReviewState: PRDraft},
			{Number: 10, Title: "Broken CI", ReviewState: PRBlocked},
			{Number: 9, Title: "Landed", ReviewState: PRMerged},
			{Number: 8, Title: "Unknown state", ReviewState: "something-new"},
		}},
		&fakeMetricsSource{total: 1234567, currency: "USD",
			trend: []TokenTrendPoint{{Date: "2026-08-19", Tokens: 40000}, {Date: "2026-08-20", Tokens: 60000}}},
	)

	_, dash := getDashboard(t, h, "web")

	// Tickets tile (8.8b/8.8c).
	if !dash.Tickets.Available || dash.Tickets.ByStatus["open"] != 3 || len(dash.Tickets.PendingApprovals) != 1 || !dash.Tickets.CanAct {
		t.Fatalf("tickets tile: %+v", dash.Tickets)
	}
	// PR tile (8.8d): four columns; unknown review_state dropped, not mis-bucketed.
	if !dash.PullRequests.Available ||
		len(dash.PullRequests.ReadyForReview) != 1 || len(dash.PullRequests.Draft) != 1 ||
		len(dash.PullRequests.Blocked) != 1 || len(dash.PullRequests.Merged) != 1 {
		t.Fatalf("pr tile: %+v", dash.PullRequests)
	}
	// Consumption tile (8.8e).
	if !dash.Consumption.Available || dash.Consumption.TotalTokens != 1234567 || len(dash.Consumption.Trend) != 2 {
		t.Fatalf("consumption tile: %+v", dash.Consumption)
	}
	// Live runs tile (8.8f): only the Project's Runs; agent mapping; phase coalescing;
	// rate-limit resume clock + fallback model from conditions.
	if !dash.LiveRuns.Available || len(dash.LiveRuns.Runs) != 3 {
		t.Fatalf("liveRuns tile: %+v", dash.LiveRuns)
	}
	byName := map[string]LiveRun{}
	for _, r := range dash.LiveRuns.Runs {
		byName[r.Name] = r
	}
	if r := byName["run-live"]; r.Agent != "agent-a" || r.Phase != "Running" || r.WorkItem != "T1" {
		t.Fatalf("run-live: %+v", r)
	}
	if r := byName["run-blank"]; r.Phase != "Pending" {
		t.Fatalf("run-blank phase coalescing: %+v", r)
	}
	if r := byName["run-paused"]; r.PausedReason != "rate_limited" || r.ResumeAt == nil || !r.ResumeAt.Equal(resume) || r.FallbackModel != "gpt-4o-mini" {
		t.Fatalf("run-paused indicators: %+v", r)
	}
	if _, ok := byName["run-foreign"]; ok {
		t.Fatalf("foreign-project run leaked into dashboard: %+v", byName["run-foreign"])
	}
}

// --- AC3: per-tile degradation ------------------------------------------------------------------

// TestDashboardPerTileDegradation — nil seams degrade exactly their tiles ("source not wired",
// FR-I3: never a fake number) while the live-Runs tile still serves from the cache; a FAILING
// seam degrades only its own tile with the error as reason, never the whole payload.
func TestDashboardPerTileDegradation(t *testing.T) {
	teamID := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	reader := newDashboardClient(t,
		team("squad-a", "alpha", teamID.String()),
		project("squad-a", "web", "https://github.com/acme/web"),
		dashboardRun("squad-a", "run-live", "web", "agent-a", "T1", ksquadv1.RunPhaseRunning),
	)

	t.Run("nil seams", func(t *testing.T) {
		h := testDashboardServer(t, teamID, reader, nil, nil, nil)
		_, dash := getDashboard(t, h, "web")
		if dash.Tickets.Available || dash.PullRequests.Available || dash.Consumption.Available {
			t.Fatalf("nil seams must degrade their tiles: %+v", dash)
		}
		if dash.Tickets.Reason == "" || dash.PullRequests.Reason == "" || dash.Consumption.Reason == "" {
			t.Fatalf("degraded tiles must carry a reason: %+v", dash)
		}
		if !dash.LiveRuns.Available || len(dash.LiveRuns.Runs) != 1 {
			t.Fatalf("live runs must still serve from the cache: %+v", dash.LiveRuns)
		}
	})

	t.Run("failing metrics seam", func(t *testing.T) {
		h := testDashboardServer(t, teamID, reader,
			&fakeTicketSource{facts: TicketFacts{ByStatus: map[string]int{"open": 1}}},
			nil,
			&fakeMetricsSource{err: errors.New("prometheus: connection refused")},
		)
		_, dash := getDashboard(t, h, "web")
		if !dash.Tickets.Available {
			t.Fatalf("tickets tile must survive a metrics failure: %+v", dash.Tickets)
		}
		if dash.Consumption.Available || dash.Consumption.Reason != "prometheus: connection refused" {
			t.Fatalf("consumption tile must degrade with the source error: %+v", dash.Consumption)
		}
		if dash.Consumption.TotalTokens != 0 {
			t.Fatalf("degraded consumption must not carry a fake number: %+v", dash.Consumption)
		}
	})
}

// --- AC2: scoping / authz (same choke point, no dashboard path) ---------------------------------

// TestDashboardForeignProjectIs404 — a Project outside the caller's Team namespace does not
// exist: 404, existence-hiding (NFR-SEC5), indistinguishable from an unknown Project.
func TestDashboardForeignProjectIs404(t *testing.T) {
	teamID := uuid.MustParse("99999999-9999-9999-9999-999999999999")
	reader := newDashboardClient(t,
		team("squad-a", "alpha", teamID.String()),
		project("squad-b", "web", "https://github.com/acme/web"), // different Team namespace
	)
	h := testDashboardServer(t, teamID, reader, nil, nil, nil)

	for _, projectID := range []string{"web", "no-such-project"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/dashboard", nil), devToken))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("project %q: got %d, want 404", projectID, rec.Code)
		}
	}
}

// TestDashboardUnauthenticated — no session cookie ⇒ 401 at the choke point, before the read
// model runs (defence in depth; BFFAuthz is the authoritative gate).
func TestDashboardUnauthenticated(t *testing.T) {
	teamID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	reader := newDashboardClient(t, team("squad-a", "alpha", teamID.String()))
	h := testDashboardServer(t, teamID, reader, nil, nil, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/projects/web/dashboard", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: got %d, want 401", rec.Code)
	}
}

// TestDashboardNotWired501 — a cluster-less dev run keeps the documented 501 contract (route
// exists; backing pending) rather than a bare 404.
func TestDashboardNotWired501(t *testing.T) {
	teamID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken: {Principal: "user:alice", TeamID: teamID},
	}}
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		// Dashboard nil ⇒ documented 501.
	})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/projects/web/dashboard", nil), devToken))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("nil dashboard: got %d, want 501", rec.Code)
	}
}

// --- seam contract helpers ----------------------------------------------------------------------

// TestParseResumeAt — the interim `resume_at=<RFC3339>` carriage parses when present and
// degrades to "no countdown" when absent or malformed (best-effort, never an error).
func TestParseResumeAt(t *testing.T) {
	want := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	if got, ok := parseResumeAt("429 from provider; resume_at=2026-08-20T15:00:00Z"); !ok || !got.Equal(want) {
		t.Fatalf("parse: got (%v, %v), want (%v, true)", got, ok, want)
	}
	if _, ok := parseResumeAt("no marker here"); ok {
		t.Fatalf("absent marker must not parse")
	}
	if _, ok := parseResumeAt("resume_at=not-a-time"); ok {
		t.Fatalf("malformed timestamp must not parse")
	}
}
