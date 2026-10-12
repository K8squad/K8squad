package apiserver

// githubsummary_test.go — ISI-5705 (WS-1b): the fleet-wide GitHub summary read
// model over the scm mirror (GET /api/squad/github). Covers:
//   - totals + per-repo breakdown aggregated purely from seeded mirror rows,
//   - the 7-day merge sparkline bucketing (UpdatedAt proxy; out-of-window merges
//     count in the total but are not bucketed),
//   - red check-run classification,
//   - the reviewed-PR join via the optional review-bridge seam (and 0 when nil),
//   - tenancy: admin spans every squad; a tenant is fenced to its own Team,
//   - route wiring: nil service ⇒ documented 501.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/controller/reviewtrigger"
	"github.com/K8squad/K8squad/pkg/coord"
	"github.com/K8squad/K8squad/pkg/scm"
)

// projectWithUID (project() + CR object UID) is shared from dashboard_test.go —
// the reviewed-PR join reads the UID to key the coord work-item lookup.

func prRow(number int, state string, merged bool, updated time.Time) scm.MirrorRow {
	return mirrorRow(scm.RecordTypePR, "pr-"+strconv.Itoa(number), state, "PR "+strconv.Itoa(number), "dev",
		scm.MirrorPayload{Number: number, Merged: merged, UpdatedAt: updated, URL: "https://github.com/acme/web/pull/" + strconv.Itoa(number)})
}

func issueRow(number int, state string, assignees []string) scm.MirrorRow {
	return mirrorRow(scm.RecordTypeIssue, "issue-"+strconv.Itoa(number), state, "issue "+strconv.Itoa(number), "dev",
		scm.MirrorPayload{Number: number, Assignees: assignees, URL: "https://github.com/acme/web/issues/" + strconv.Itoa(number)})
}

func checkRow(name, conclusion string) scm.MirrorRow {
	return mirrorRow(scm.RecordTypeCheckRun, "check-"+name, "completed", name, "ci",
		scm.MirrorPayload{Conclusion: conclusion})
}

// TestGithubSummary_Aggregate pins the core roll-up: totals, per-repo breakdown,
// the merge sparkline bucketing, and red-check classification — all from the
// seeded mirror, as a fleet admin spanning every squad.
func TestGithubSummary_Aggregate(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	teamA := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	reader := newDashboardClient(t,
		team("squad-a", "alpha", teamA.String()),
		projectWithUID("squad-a", "web", "https://github.com/acme/web", "proj-web"))

	store := scm.NewInMemoryMirrorStore()
	seedMirror(t, store, "squad-a", "web",
		issueRow(1, "open", []string{"alice"}),            // open + assigned
		issueRow(2, "open", nil),                          // open, unassigned
		issueRow(3, "closed", []string{"bob"}),            // closed → not counted
		prRow(10, "open", false, now),                     // open PR
		prRow(11, "closed", true, now),                    // merged today → sparkline[6]
		prRow(12, "closed", true, now.AddDate(0, 0, -3)),  // merged 3d ago → sparkline[3]
		prRow(13, "closed", true, now.AddDate(0, 0, -30)), // merged 30d ago → total only
		checkRow("build", "failure"),                      // red
		checkRow("lint", "timed_out"),                     // red
		checkRow("test", "success"),                       // green
	)

	svc := NewGithubSummaryService(reader, store, NewClientOverviewReader(reader), nil)
	svc.now = func() time.Time { return now }

	admin := discussion.AuthorContext{Principal: "user:root", TeamID: uuid.Nil, IsAdmin: true}
	got, err := svc.Summary(context.Background(), admin)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}

	want := GithubTotals{IssuesAssigned: 1, IssuesOpen: 2, PRsOpened: 1, PRsReviewed: 0, PRsMerged: 3, ChecksRed: 2}
	if got.Totals != want {
		t.Errorf("totals: got %+v want %+v", got.Totals, want)
	}

	if len(got.MergeSparkline7d) != 7 {
		t.Fatalf("sparkline length: got %d want 7", len(got.MergeSparkline7d))
	}
	if got.MergeSparkline7d[6] != 1 || got.MergeSparkline7d[3] != 1 {
		t.Errorf("sparkline buckets: got %v want [.. ,3]=1 & [6]=1", got.MergeSparkline7d)
	}
	var sparkSum int
	for _, v := range got.MergeSparkline7d {
		sparkSum += v
	}
	if sparkSum != 2 {
		t.Errorf("sparkline should bucket exactly the 2 in-window merges, got sum %d (%v)", sparkSum, got.MergeSparkline7d)
	}

	if len(got.ByRepo) != 1 {
		t.Fatalf("byRepo: got %d repos want 1 (%+v)", len(got.ByRepo), got.ByRepo)
	}
	repo := got.ByRepo[0]
	wantRepo := GithubRepoStat{Repo: "acme/web", Assigned: 1, Open: 2, PRs: 1, Reviewed: 0, ChecksRed: 2}
	if repo != wantRepo {
		t.Errorf("byRepo[0]: got %+v want %+v", repo, wantRepo)
	}
}

// TestGithubSummary_ReviewedJoin pins the one count that depends on the local
// review bridge: a PR whose review work item reached "done" is counted reviewed;
// a miss is honestly not reviewed; a nil seam yields 0 without fabricating.
func TestGithubSummary_ReviewedJoin(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	reader := newDashboardClient(t,
		team("squad-a", "alpha", "11111111-1111-1111-1111-111111111111"),
		projectWithUID("squad-a", "web", "https://github.com/acme/web", "proj-web"))
	store := scm.NewInMemoryMirrorStore()
	seedMirror(t, store, "squad-a", "web",
		prRow(10, "open", false, now),
		prRow(11, "open", false, now))

	// PR #10 has a review work item in "done"; PR #11 has none (honest miss).
	admin := discussion.AuthorContext{Principal: "user:root", TeamID: uuid.Nil, IsAdmin: true}
	anchor := reviewtrigger.PRAnchorLabel("https://github.com/acme/web", "10")
	finder := &fakeReviewFinder{byLabel: map[string]coord.WorkItemRecord{
		// admin ⇒ reviewTeamID "" ; projectID is the Project CR UID.
		"|proj-web|" + anchor: {ID: "wi-1", State: "done"},
	}}

	svc := NewGithubSummaryService(reader, store, NewClientOverviewReader(reader), finder)
	svc.now = func() time.Time { return now }
	got, err := svc.Summary(context.Background(), admin)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if got.Totals.PRsReviewed != 1 {
		t.Errorf("reviewed total: got %d want 1", got.Totals.PRsReviewed)
	}
	if len(got.ByRepo) != 1 || got.ByRepo[0].Reviewed != 1 {
		t.Errorf("byRepo reviewed: got %+v want reviewed=1", got.ByRepo)
	}

	// Nil seam ⇒ 0 reviewed, no fabrication, every other count intact.
	svcNoReviews := NewGithubSummaryService(reader, store, NewClientOverviewReader(reader), nil)
	svcNoReviews.now = func() time.Time { return now }
	bare, err := svcNoReviews.Summary(context.Background(), admin)
	if err != nil {
		t.Fatalf("Summary (no reviews): %v", err)
	}
	if bare.Totals.PRsReviewed != 0 {
		t.Errorf("nil review seam should yield 0 reviewed, got %d", bare.Totals.PRsReviewed)
	}
	if bare.Totals.PRsOpened != 2 {
		t.Errorf("nil review seam must not disturb other counts; prsOpened got %d want 2", bare.Totals.PRsOpened)
	}
}

// TestGithubSummary_Tenancy pins the scoping: a fleet admin spans both squads'
// repos; a tenant scoped to squad-a sees only its own repo, never squad-b's.
func TestGithubSummary_Tenancy(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	teamA := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	teamB := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	reader := newDashboardClient(t,
		team("squad-a", "alpha", teamA.String()),
		team("squad-b", "beta", teamB.String()),
		projectWithUID("squad-a", "web", "https://github.com/acme/web", "proj-web"),
		projectWithUID("squad-b", "api", "https://github.com/acme/api", "proj-api"))
	store := scm.NewInMemoryMirrorStore()
	seedMirror(t, store, "squad-a", "web", issueRow(1, "open", nil))
	seedMirror(t, store, "squad-b", "api", issueRow(1, "open", nil), issueRow(2, "open", nil))

	svc := NewGithubSummaryService(reader, store, NewClientOverviewReader(reader), nil)
	svc.now = func() time.Time { return now }

	admin := discussion.AuthorContext{Principal: "user:root", TeamID: uuid.Nil, IsAdmin: true}
	fleet, err := svc.Summary(context.Background(), admin)
	if err != nil {
		t.Fatalf("admin Summary: %v", err)
	}
	if fleet.Totals.IssuesOpen != 3 || len(fleet.ByRepo) != 2 {
		t.Errorf("admin should span both squads: got %d open issues across %d repos", fleet.Totals.IssuesOpen, len(fleet.ByRepo))
	}

	tenant := discussion.AuthorContext{Principal: "user:alice", TeamID: teamA}
	scoped, err := svc.Summary(context.Background(), tenant)
	if err != nil {
		t.Fatalf("tenant Summary: %v", err)
	}
	if scoped.Totals.IssuesOpen != 1 || len(scoped.ByRepo) != 1 || scoped.ByRepo[0].Repo != "acme/web" {
		t.Errorf("tenant must be fenced to squad-a: got %+v", scoped)
	}
}

// TestGithubSummary_Route pins the HTTP wiring: a nil service answers the
// documented 501; a wired service returns 200 with the JSON payload.
func TestGithubSummary_Route(t *testing.T) {
	teamID := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	// nil service ⇒ 501.
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken: {Principal: "user:alice", TeamID: teamID},
	}}
	nilSrv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
	})
	rec := httptest.NewRecorder()
	nilSrv.Handler().ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/squad/github", nil), devToken))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("nil service: got %d want 501", rec.Code)
	}

	// wired ⇒ 200.
	reader := newDashboardClient(t,
		team("squad-a", "alpha", teamID.String()),
		projectWithUID("squad-a", "web", "https://github.com/acme/web", "proj-web"))
	store := scm.NewInMemoryMirrorStore()
	seedMirror(t, store, "squad-a", "web", issueRow(1, "open", []string{"alice"}))
	svc := NewGithubSummaryService(reader, store, NewClientOverviewReader(reader), nil)

	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		GithubSummary: svc,
	})
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/squad/github", nil), devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("wired service: got %d want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var out GithubSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Totals.IssuesAssigned != 1 || out.Totals.IssuesOpen != 1 {
		t.Errorf("payload totals: got %+v", out.Totals)
	}
	if out.ByRepo == nil || out.MergeSparkline7d == nil {
		t.Errorf("slices must marshal as [] not null: byRepo=%v spark=%v", out.ByRepo, out.MergeSparkline7d)
	}
}
