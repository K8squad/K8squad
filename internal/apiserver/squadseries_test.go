package apiserver

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/coord"
)

// fakeFleetStatusSource is a canned coord seam for the fleet-series test — the
// coord SQL reconstruction is unit-tested in pkg/coord; here we only prove the
// seam wires through, degrades on nil, and is scoped by the caller's team.
type fakeFleetStatusSource struct {
	snaps   []coord.StatusSnapshot
	err     error
	gotTeam string
	called  bool
}

func (f *fakeFleetStatusSource) FleetStatusSnapshots(_ context.Context, teamID string, _, _ time.Time) ([]coord.StatusSnapshot, error) {
	f.called = true
	f.gotTeam = teamID
	return f.snaps, f.err
}

// teamWithExecNS builds a Team whose reconciled execution namespace (where Run
// CRs live, ISI-4565) differs from its home namespace.
func teamWithExecNS(homeNS, name, uid, execNS string) *ksquadv1.Team {
	tm := team(homeNS, name, uid)
	tm.Status.Namespace = execNS
	return tm
}

func newSquadSeriesSvc(t *testing.T, status FleetStatusHistorySource, objs ...client.Object) *SquadSeriesService {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(overviewScheme(t)).WithObjects(objs...).Build()
	return NewSquadSeriesService(c, status)
}

// TestSquadSeriesTenantRunsTokensAndTickets — a tenant's series folds the Runs
// in their execution namespace into the window phase-rollup + token-sum, and the
// coord seam is scoped by the caller's team UID.
func TestSquadSeriesTenantRunsTokensAndTickets(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	from, to := now.Add(-7*24*time.Hour), now
	inWindow := now.Add(-2 * 24 * time.Hour)
	preWindow := now.Add(-40 * 24 * time.Hour)
	preWindowEnd := now.Add(-39 * 24 * time.Hour)

	status := &fakeFleetStatusSource{snaps: []coord.StatusSnapshot{{Date: "2026-09-30", Counts: map[string]int{"done": 2}}}}
	svc := newSquadSeriesSvc(t, status,
		teamWithExecNS("squad-a", "alpha", ovwTeamUID, "squad-a-exec"),
		// Runs live in the EXECUTION namespace, spanning two Projects — the fleet
		// series counts both (no per-project filter).
		overviewRun("squad-a-exec", "r-run", "proj-x", ksquadv1.RunPhaseRunning, &inWindow, nil,
			&ksquadv1.TokenUsage{InputTokens: 100, OutputTokens: 40, TotalTokens: 140}),
		overviewRun("squad-a-exec", "r-done", "proj-y", ksquadv1.RunPhaseSucceeded, &inWindow, &now,
			&ksquadv1.TokenUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}),
		// Terminated before the window — excluded.
		overviewRun("squad-a-exec", "r-old", "proj-x", ksquadv1.RunPhaseSucceeded, &preWindow, &preWindowEnd,
			&ksquadv1.TokenUsage{InputTokens: 999, OutputTokens: 999, TotalTokens: 1998}),
	)

	got, err := svc.Series(context.Background(), ovwTeamAuth(), from, to)
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	if got.Fleet {
		t.Errorf("tenant series must not be marked fleet")
	}
	if got.RunsByStatus.Total != 2 {
		t.Fatalf("runs total = %d, want 2 (byPhase=%v)", got.RunsByStatus.Total, got.RunsByStatus.ByPhase)
	}
	if got.RunsByStatus.ByPhase["running"] != 1 || got.RunsByStatus.ByPhase["succeeded"] != 1 {
		t.Errorf("byPhase = %v, want running:1 succeeded:1", got.RunsByStatus.ByPhase)
	}
	if got.Tokens.Total != 155 || got.Tokens.Input != 110 || got.Tokens.Output != 45 || got.Tokens.RunsCounted != 2 {
		t.Errorf("tokens = %+v, want in=110 out=45 total=155 counted=2", got.Tokens)
	}
	if status.gotTeam != ovwTeamUID {
		t.Errorf("coord seam team = %q, want %q", status.gotTeam, ovwTeamUID)
	}
	if !got.TicketsByStatus.Available || len(got.TicketsByStatus.Snapshots) != 1 {
		t.Errorf("tickets tile: available=%v snaps=%d", got.TicketsByStatus.Available, len(got.TicketsByStatus.Snapshots))
	}
}

// TestSquadSeriesAdminFleetWide — an admin lists Runs across EVERY namespace and
// the coord seam is called with an EMPTY teamID (trusted fleet-admin path).
func TestSquadSeriesAdminFleetWide(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	inWindow := now.Add(-time.Hour)
	status := &fakeFleetStatusSource{}
	svc := newSquadSeriesSvc(t, status,
		// Runs in two DIFFERENT squad namespaces — both counted fleet-wide.
		overviewRun("squad-a-exec", "r-a", "proj-x", ksquadv1.RunPhaseRunning, &inWindow, nil, nil),
		overviewRun("squad-b-exec", "r-b", "proj-z", ksquadv1.RunPhaseRunning, &inWindow, nil, nil),
	)
	got, err := svc.Series(context.Background(), ovwAdminAuth(), now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("Series (admin): %v", err)
	}
	if !got.Fleet {
		t.Errorf("admin series must be marked fleet")
	}
	if got.RunsByStatus.Total != 2 {
		t.Errorf("admin runs total = %d, want 2 (fleet-wide across namespaces)", got.RunsByStatus.Total)
	}
	if !status.called || status.gotTeam != "" {
		t.Errorf("admin coord seam: called=%v team=%q, want called empty-team", status.called, status.gotTeam)
	}
}

// TestSquadSeriesNilStatusDegrades — a nil coord seam degrades ONLY the tickets
// tile; runs + tokens still serve from the cache.
func TestSquadSeriesNilStatusDegrades(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	claimed := now.Add(-time.Hour)
	svc := newSquadSeriesSvc(t, nil,
		teamWithExecNS("squad-a", "alpha", ovwTeamUID, "squad-a-exec"),
		overviewRun("squad-a-exec", "r1", "proj-x", ksquadv1.RunPhaseRunning, &claimed, nil, nil),
	)
	got, err := svc.Series(context.Background(), ovwTeamAuth(), now.Add(-7*24*time.Hour), now)
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	if got.TicketsByStatus.Available {
		t.Errorf("tickets tile should be degraded with a nil seam (reason=%q)", got.TicketsByStatus.Reason)
	}
	if !got.RunsByStatus.Available || got.RunsByStatus.Total != 1 {
		t.Errorf("runs tile should still serve: %+v", got.RunsByStatus)
	}
}

// TestSquadSeriesTenantNoTeam404 — a tenant whose Team scope resolves to no Team
// gets the existence-hiding ErrTeamNotFound (the handler maps it to 404).
func TestSquadSeriesTenantNoTeam404(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	svc := newSquadSeriesSvc(t, &fakeFleetStatusSource{},
		// A Team exists, but NOT the one the caller's UID names.
		teamWithExecNS("squad-b", "beta", "99999999-9999-9999-9999-999999999999", "squad-b-exec"),
	)
	_, err := svc.Series(context.Background(), ovwTeamAuth(), now.Add(-24*time.Hour), now)
	if err == nil {
		t.Fatalf("Series should 404 for an unresolvable team scope")
	}
}

// TestSquadSeriesTenantEmptyScope404 — a non-admin with no Team scope at all is
// a 404, never a silent fleet-wide read.
func TestSquadSeriesTenantEmptyScope404(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	svc := newSquadSeriesSvc(t, &fakeFleetStatusSource{})
	noScope := discussion.AuthorContext{Principal: "nobody", TeamID: uuid.Nil}
	if _, err := svc.Series(context.Background(), noScope, now.Add(-24*time.Hour), now); err == nil {
		t.Fatalf("Series should 404 for an empty tenant scope")
	}
}

// compile-time: *coord.WorkItemReadStore satisfies the seam (production wiring).
var _ FleetStatusHistorySource = (*coord.WorkItemReadStore)(nil)
