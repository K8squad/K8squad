package apiserver

// ============================================================================
// Fleet overview time-series read model (ISI-5706 WS-1c) — the fleet-wide
// generalization of the per-project overview series (projectoverview.go,
// ISI-4509), served at GET /api/squad/overview/series?window=30d (or ?from=&to=).
// ============================================================================
//
// Parent plan ISI-5697 (Plan tab rev2); design ref ISI-5699 (approved). Where
// GET /api/projects/{id}/overview answers for ONE Project, this answers for the
// whole fleet (admin) or the caller's whole Team (tenant) — the Plan tab's
// global charts. It REUSES the S1 primitives verbatim:
//
//   1. ticketsByStatus — daily point-in-time per-§13-state counts, reconstructed
//      from coord.audit_log state_transition rows across EVERY board Project in
//      scope (coord.WorkItemReadStore.FleetStatusSnapshots — the project_id
//      filter dropped from ProjectStatusSnapshots). No new store.
//   2. runsByStatus    — Run CRs in scope grouped by RunPhase, counting runs
//      whose activity overlaps the window. Informer cache, no seam. This is a
//      WINDOW AGGREGATE, not a run-phase time series: Run CRs carry only live
//      status (etcd has no phase-transition history store, unlike board items'
//      audit_log), so a fabricated daily run series would violate FR-I3. The
//      honest figure is the in-window phase rollup — the same primitive the
//      per-project overview serves.
//   3. tokens          — summed Run.status.totalTokenUsage over the in-window
//      runs. Informer cache, no seam.
//
// Per-panel degradation mirrors the per-project overview + the dashboard (8.8a
// AC3, ISI-4229 degrade-don't-blank): the CRD series (runs/tokens) serve
// whenever the scope resolves; the coord-backed ticketsByStatus degrades to
// available:false with a reason when the seam is unwired or errors — never a
// whole-payload failure, never a fabricated band.
//
// Tenancy mirrors the sibling fleet reads (overview.go): admin ⇒ fleet-wide
// (every squad's Runs + board items, empty teamID into the coord seam); tenant ⇒
// fenced to their Team (resolved by object UID — rename/collision-safe), 404
// when the Team scope resolves to no Team.

import (
	"context"
	"errors"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/coord"
)

// SquadOverviewSeries is the composed payload behind GET /api/squad/overview/series.
// Window echoes the resolved bounds; Fleet marks the admin (fleet-wide) projection
// so the console can render a fleet banner exactly as the squad overview does. The
// three series degrade independently (only ticketsByStatus can degrade — the CRD
// series always serve once the scope resolves).
type SquadOverviewSeries struct {
	Window          OverviewWindow    `json:"window"`
	Fleet           bool              `json:"fleet,omitempty"`
	TicketsByStatus TicketsSeriesTile `json:"ticketsByStatus"`
	RunsByStatus    RunsByStatusTile  `json:"runsByStatus"`
	Tokens          TokensTile        `json:"tokens"`
}

// FleetStatusHistorySource is the coord seam for the fleet/Team-wide
// tickets-by-status-over-time rollup. teamID scopes tenancy (coord.work_item.team_id;
// "" ⇒ trusted fleet-admin). Implemented by *coord.WorkItemReadStore.FleetStatusSnapshots.
type FleetStatusHistorySource interface {
	FleetStatusSnapshots(ctx context.Context, teamID string, from, to time.Time) ([]coord.StatusSnapshot, error)
}

// SquadSeriesService composes the fleet overview series. reader is the informer
// cache (Runs + Team resolution); status is the optional coord seam.
type SquadSeriesService struct {
	reader client.Reader
	status FleetStatusHistorySource
}

// NewSquadSeriesService builds the read model. reader MUST have api/v1alpha1
// registered. status is optional (nil ⇒ the ticketsByStatus tile renders the
// documented "not configured" state; runs/tokens still serve).
func NewSquadSeriesService(reader client.Reader, status FleetStatusHistorySource) *SquadSeriesService {
	return &SquadSeriesService{reader: reader, status: status}
}

// Series composes the payload over [from, to]. Scope resolves like the squad
// overview (admin ⇒ fleet-wide, else Team-fenced by UID). A tenant whose Team
// scope resolves to no Team gets ErrTeamNotFound (the handler maps it to 404).
func (s *SquadSeriesService) Series(ctx context.Context, auth discussion.AuthorContext, from, to time.Time) (SquadOverviewSeries, error) {
	// Resolve the Run-read scope first — an unresolvable tenant scope is a
	// top-level 404 (tenancy), distinct from a read error below which degrades
	// only the affected tiles.
	runNS, fleet, err := s.resolveRunScope(ctx, auth)
	if err != nil {
		return SquadOverviewSeries{}, err
	}

	out := SquadOverviewSeries{
		Window: OverviewWindow{From: from, To: to, Bucket: "day"},
		Fleet:  fleet,
	}

	// ── runs + tokens: informer cache, no seam — one List, two projections. ──
	runs, rerr := s.listRuns(ctx, runNS, fleet)
	if rerr != nil {
		out.RunsByStatus = RunsByStatusTile{TileStatus: degradedTile(rerr.Error()), ByPhase: map[string]int{}}
		out.Tokens = TokensTile{TileStatus: degradedTile(rerr.Error())}
	} else {
		out.RunsByStatus = runsByStatus(runs, from, to)
		out.Tokens = tokenSum(runs, from, to)
	}

	// ── ticketsByStatus: coord seam (fleet/Team status-history rollup). ──────
	switch s.status {
	case nil:
		out.TicketsByStatus = TicketsSeriesTile{TileStatus: degradedTile("source not wired: coordination status-history (coord.audit_log state_transition)")}
	default:
		teamID := ""
		if !auth.IsAdmin {
			teamID = auth.TeamID.String()
		}
		snaps, serr := s.status.FleetStatusSnapshots(ctx, teamID, from, to)
		if serr != nil {
			out.TicketsByStatus = TicketsSeriesTile{TileStatus: degradedTile(serr.Error())}
		} else {
			out.TicketsByStatus = TicketsSeriesTile{TileStatus: TileStatus{Available: true}, Snapshots: snaps}
		}
	}

	return out, nil
}

// resolveRunScope returns the namespace the Run List should be fenced to and
// whether the projection is fleet-wide. admin ⇒ ("", true) — every squad's
// execution namespace (no InNamespace). tenant ⇒ (execNamespace, false),
// resolved from the Team CR by object UID (rename/collision-safe), with
// ErrTeamNotFound when the Team scope resolves to no Team (existence-hiding 404).
func (s *SquadSeriesService) resolveRunScope(ctx context.Context, auth discussion.AuthorContext) (string, bool, error) {
	if auth.IsAdmin {
		return "", true, nil
	}
	teamUID := auth.TeamID.String()
	if teamUID == "" {
		return "", false, ErrTeamNotFound
	}
	var teams ksquadv1.TeamList
	if err := s.reader.List(ctx, &teams); err != nil {
		return "", false, err
	}
	for i := range teams.Items {
		if string(teams.Items[i].UID) != teamUID {
			continue
		}
		// Run CRs live in the squad's EXECUTION namespace (Team.Status.Namespace),
		// falling back to the HOME namespace for the co-tenant/pre-per-team layout
		// (ISI-4565) — exactly like ClientOverviewReader.Overview.
		runNS := teams.Items[i].Status.Namespace
		if runNS == "" {
			runNS = teams.Items[i].Namespace
		}
		return runNS, false, nil
	}
	return "", false, ErrTeamNotFound
}

// listRuns reads the Runs in scope from the informer cache. fleet ⇒ every
// namespace (no InNamespace); tenant ⇒ the resolved execution namespace.
func (s *SquadSeriesService) listRuns(ctx context.Context, runNS string, fleet bool) ([]ksquadv1.Run, error) {
	var runs ksquadv1.RunList
	var opts []client.ListOption
	if !fleet {
		opts = append(opts, client.InNamespace(runNS))
	}
	if err := s.reader.List(ctx, &runs, opts...); err != nil {
		return nil, err
	}
	return runs.Items, nil
}

// ============================================================================
// Handler — GET /api/squad/overview/series behind the §13 BFF authz choke point
// ============================================================================

// squadOverviewSeries is the handler behind the route. Window is parsed from the
// query (?window=Nd | ?from=&to= RFC3339, shared grammar with the per-project
// overview); a bad window is 400. A tenant whose Team scope resolves to no Team
// gets 404; any other read error is 502.
func (s *Server) squadOverviewSeries(svc *SquadSeriesService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := discussion.AuthFromContext(r.Context())
		if !ok || auth.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		from, to, perr := parseOverviewWindow(r, time.Now())
		if perr != nil {
			writeJSONError(w, http.StatusBadRequest, perr.Error())
			return
		}
		series, err := svc.Series(r.Context(), auth, from, to)
		switch {
		case errors.Is(err, ErrTeamNotFound):
			writeJSONError(w, http.StatusNotFound, "no overview for this team scope")
		case err != nil:
			writeJSONError(w, http.StatusBadGateway, "overview series read model unavailable")
		default:
			writeJSON(w, http.StatusOK, series)
		}
	}
}
