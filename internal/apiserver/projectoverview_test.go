package apiserver

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/coord"
)

// overviewRun builds a Run in a Project with a phase, claim time, optional
// terminal condition time, and optional total token usage.
func overviewRun(ns, name, projectName string, phase ksquadv1.RunPhase, claimed *time.Time, terminalAt *time.Time, tokens *ksquadv1.TokenUsage) *ksquadv1.Run {
	r := &ksquadv1.Run{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       ksquadv1.RunSpec{ProjectRef: ksquadv1.ObjectRef{Name: projectName}},
		Status:     ksquadv1.RunStatus{Phase: phase, TotalTokenUsage: tokens},
	}
	if claimed != nil {
		r.Status.ClaimedAt = &metav1.Time{Time: *claimed}
	}
	if terminalAt != nil {
		r.Status.Conditions = []metav1.Condition{{
			Type:               "Succeeded",
			Status:             metav1.ConditionTrue,
			Reason:             "Done",
			LastTransitionTime: metav1.Time{Time: *terminalAt},
		}}
	}
	return r
}

// fakeStatusSource is a canned coord seam for the service test — the coord SQL
// reconstruction is unit-tested in pkg/coord; here we only prove the seam wires
// through, degrades on nil, and is scoped by the resolved UID/team.
type fakeStatusSource struct {
	snaps      []coord.StatusSnapshot
	err        error
	gotTeam    string
	gotProject string
}

func (f *fakeStatusSource) ProjectStatusSnapshots(_ context.Context, teamID, projectID string, _, _ time.Time) ([]coord.StatusSnapshot, error) {
	f.gotTeam, f.gotProject = teamID, projectID
	return f.snaps, f.err
}

func newOverviewSvc(t *testing.T, status StatusHistorySource, objs ...client.Object) *OverviewService {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(overviewScheme(t)).WithObjects(objs...).Build()
	return NewOverviewService(c, status)
}

const ovwTeamUID = "22222222-2222-2222-2222-222222222222"
const ovwProjUID = "33333333-3333-3333-3333-333333333333"

func ovwAdminAuth() discussion.AuthorContext {
	return discussion.AuthorContext{Principal: "root", TeamID: uuid.Nil, IsAdmin: true}
}
func ovwTeamAuth() discussion.AuthorContext {
	return discussion.AuthorContext{Principal: "alice", TeamID: uuid.MustParse(ovwTeamUID)}
}

// TestOverviewRunsAndTokens — runs-by-phase and token-sum over the window from
// the informer cache. Two in-window runs (one running, one succeeded) sum their
// tokens; a run that terminated before the window is excluded.
func TestOverviewRunsAndTokens(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	from, to := now.Add(-7*24*time.Hour), now
	inWindow := now.Add(-2 * 24 * time.Hour)
	preWindow := now.Add(-40 * 24 * time.Hour)
	preWindowEnd := now.Add(-39 * 24 * time.Hour)

	status := &fakeStatusSource{snaps: []coord.StatusSnapshot{{Date: "2026-09-30", Counts: map[string]int{"done": 1}}}}
	svc := newOverviewSvc(t, status,
		team("squad-a", "alpha", ovwTeamUID),
		projectWithUID("squad-a", "proj-x", "repo", ovwProjUID),
		overviewRun("squad-a", "r-run", "proj-x", ksquadv1.RunPhaseRunning, &inWindow, nil,
			&ksquadv1.TokenUsage{InputTokens: 100, OutputTokens: 40, TotalTokens: 140}),
		overviewRun("squad-a", "r-done", "proj-x", ksquadv1.RunPhaseSucceeded, &inWindow, &now,
			&ksquadv1.TokenUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}),
		overviewRun("squad-a", "r-old", "proj-x", ksquadv1.RunPhaseSucceeded, &preWindow, &preWindowEnd,
			&ksquadv1.TokenUsage{InputTokens: 999, OutputTokens: 999, TotalTokens: 1998}),
		// A run in a DIFFERENT project — must be ignored.
		overviewRun("squad-a", "r-other", "proj-y", ksquadv1.RunPhaseRunning, &inWindow, nil,
			&ksquadv1.TokenUsage{InputTokens: 7, OutputTokens: 7, TotalTokens: 14}),
	)

	got, err := svc.Series(context.Background(), ovwTeamAuth(), "proj-x", from, to)
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	if got.RunsByStatus.Total != 2 {
		t.Fatalf("runs total = %d, want 2 (byPhase=%v)", got.RunsByStatus.Total, got.RunsByStatus.ByPhase)
	}
	if got.RunsByStatus.ByPhase["running"] != 1 || got.RunsByStatus.ByPhase["succeeded"] != 1 {
		t.Errorf("byPhase = %v, want running:1 succeeded:1", got.RunsByStatus.ByPhase)
	}
	if got.Tokens.Total != 155 || got.Tokens.Input != 110 || got.Tokens.Output != 45 {
		t.Errorf("tokens = %+v, want in=110 out=45 total=155", got.Tokens)
	}
	if got.Tokens.RunsCounted != 2 {
		t.Errorf("runsCounted = %d, want 2", got.Tokens.RunsCounted)
	}
	// The coord seam is scoped by the resolved UID and the caller's team.
	if status.gotProject != ovwProjUID {
		t.Errorf("coord seam project = %q, want resolved UID %q", status.gotProject, ovwProjUID)
	}
	if status.gotTeam != ovwTeamUID {
		t.Errorf("coord seam team = %q, want %q", status.gotTeam, ovwTeamUID)
	}
	if !got.TicketsByStatus.Available || len(got.TicketsByStatus.Snapshots) != 1 {
		t.Errorf("tickets tile: available=%v snaps=%d", got.TicketsByStatus.Available, len(got.TicketsByStatus.Snapshots))
	}
}

// TestOverviewNilStatusDegrades — a nil coord seam degrades ONLY the tickets
// tile; runs + tokens still serve from the cache.
func TestOverviewNilStatusDegrades(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	claimed := now.Add(-time.Hour)
	svc := newOverviewSvc(t, nil,
		team("squad-a", "alpha", ovwTeamUID),
		projectWithUID("squad-a", "proj-x", "repo", ovwProjUID),
		overviewRun("squad-a", "r1", "proj-x", ksquadv1.RunPhaseRunning, &claimed, nil, nil),
	)
	got, err := svc.Series(context.Background(), ovwTeamAuth(), "proj-x", now.Add(-7*24*time.Hour), now)
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	if got.TicketsByStatus.Available {
		t.Errorf("tickets tile should be degraded with a nil seam")
	}
	if !got.RunsByStatus.Available || got.RunsByStatus.Total != 1 {
		t.Errorf("runs tile should still serve: %+v", got.RunsByStatus)
	}
}

// TestOverviewAdminFleetWide — an admin resolves the Project fleet-wide by UID
// and the coord seam is called with an EMPTY teamID (trusted fleet-admin path).
func TestProjectOverviewAdminFleetWide(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	status := &fakeStatusSource{}
	svc := newOverviewSvc(t, status,
		projectWithUID("squad-a", "proj-x", "repo", ovwProjUID),
	)
	_, err := svc.Series(context.Background(), ovwAdminAuth(), ovwProjUID, now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("Series (admin): %v", err)
	}
	if status.gotTeam != "" {
		t.Errorf("admin coord seam team = %q, want empty (fleet-admin)", status.gotTeam)
	}
	if status.gotProject != ovwProjUID {
		t.Errorf("admin coord seam project = %q, want %q", status.gotProject, ovwProjUID)
	}
}

// TestOverviewForeignProject404 — a non-admin naming a Project outside their
// team gets the existence-hiding ErrProjectNotFound.
func TestOverviewForeignProject404(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	svc := newOverviewSvc(t, &fakeStatusSource{},
		team("squad-a", "alpha", ovwTeamUID),
		projectWithUID("squad-b", "proj-x", "repo", ovwProjUID), // in a DIFFERENT namespace
	)
	_, err := svc.Series(context.Background(), ovwTeamAuth(), "proj-x", now.Add(-24*time.Hour), now)
	if err == nil {
		t.Fatalf("Series should 404 for a foreign project")
	}
}

// TestParseOverviewWindow — the window grammar: default 30d, ?window=Nd,
// explicit ?from/?to, and the error cases.
func TestParseOverviewWindow(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

	// default
	from, to, err := parseOverviewWindow(httptest.NewRequest("GET", "/x", nil), now)
	if err != nil || !to.Equal(now) || from.After(now.Add(-29*24*time.Hour)) {
		t.Errorf("default window: from=%s to=%s err=%v", from, to, err)
	}

	// ?window=7d
	from, _, err = parseOverviewWindow(httptest.NewRequest("GET", "/x?window=7d", nil), now)
	if err != nil || now.Sub(from) != 7*24*time.Hour {
		t.Errorf("7d window: from=%s err=%v", from, err)
	}

	// explicit from/to
	r := httptest.NewRequest("GET", "/x?from=2026-09-01T00:00:00Z&to=2026-09-10T00:00:00Z", nil)
	from, to, err = parseOverviewWindow(r, now)
	if err != nil || from.Day() != 1 || to.Day() != 10 {
		t.Errorf("explicit window: from=%s to=%s err=%v", from, to, err)
	}

	// bad window token
	if _, _, err = parseOverviewWindow(httptest.NewRequest("GET", "/x?window=lots", nil), now); err == nil {
		t.Error("bad window token should error")
	}
	// to before from
	if _, _, err = parseOverviewWindow(httptest.NewRequest("GET", "/x?from=2026-09-10T00:00:00Z&to=2026-09-01T00:00:00Z", nil), now); err == nil {
		t.Error("to<from should error")
	}
	// window too wide
	if _, _, err = parseOverviewWindow(httptest.NewRequest("GET", "/x?window=999d", nil), now); err == nil {
		t.Error("999d should error (max 366)")
	}
	// 'to' without 'from'
	if _, _, err = parseOverviewWindow(httptest.NewRequest("GET", "/x?to=2026-09-10T00:00:00Z", nil), now); err == nil {
		t.Error("to without from should error")
	}
}
