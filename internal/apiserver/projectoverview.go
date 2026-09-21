package apiserver

// ============================================================================
// Project-overview time-series read model (ISI-4509 / ISI-4505 S4) — the
// window-parameterized backing S3's project-overview charts call, served at
// GET /api/projects/{projectId}/overview?window=30d (or ?from=&to=).
// ============================================================================
//
// DESIGN-SPEC-ISI-4505 §4/§6 asked for three project-scoped, windowed series to
// back the overview dashboard; §6 open-Q1 asked whether the "tickets by status
// over time" chart needs a net-new snapshot store. The answer is NO — all three
// are read-model projections over EXISTING stores (ADR-020, the same discipline
// as the 8.8a dashboard). This surface composes them:
//
//   1. ticketsByStatus — daily point-in-time counts per §13 state, reconstructed
//      from coord.audit_log `state_transition` rows (coord seam, StatusHistorySource
//      → coord.WorkItemReadStore.ProjectStatusSnapshots). NO new storage.
//   2. runsByStatus    — Run CRs for the Project (spec.projectRef), grouped by
//      RunPhase, counting runs whose activity overlaps the window. Informer
//      cache, no seam.
//   3. tokens          — summed Run.status.totalTokenUsage (per-run LLM
//      interaction record, ISI-4238) across the in-window runs. Informer cache,
//      no seam. $ estimate is left nil unless a price table is configured
//      (net-new; tokens are the authoritative legibility figure, cost is not).
//
// Per-source degradation mirrors the dashboard (8.8a AC3): the CRD-backed series
// (runs/tokens) serve whenever the Project resolves; the coord-backed
// ticketsByStatus degrades to `available:false` with a reason when the coord
// seam is unwired or errors — never a whole-payload failure, never a fake band.
//
// Tenancy: the SAME resolveProject…WithUID spine + existence-hiding 404 the
// dashboard uses. Non-admin is Team-fenced; admin is fleet-wide (409 on a bare
// name that spans squads).

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/coord"
)

// Window bounds (defense-in-depth against an unbounded daily reconstruction).
const (
	overviewDefaultWindow = 30 * 24 * time.Hour
	overviewMaxWindowDays = 366
)

// OverviewWindow echoes the resolved window back to the client so a chart can
// label its axis without re-deriving the bounds it asked for.
type OverviewWindow struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Bucket string    `json:"bucket"` // always "day" for now
}

// TicketsSeriesTile is the stacked-area backing: one snapshot per day, each a
// per-status count map (zero-filled across the §13 enum). Degradable — the coord
// seam is optional (8.8a AC4), so an unwired/erroring source renders
// available:false, never a fabricated series.
type TicketsSeriesTile struct {
	TileStatus
	Snapshots []coord.StatusSnapshot `json:"snapshots,omitempty"`
}

// RunsByStatusTile groups the Project's in-window Runs by RunPhase (lowercased,
// lossless). The overview mock buckets these into Completed/Running/Blocked/
// Paused/Cancelled bars; the read model stays honest and returns the raw phase
// counts so the client owns the display bucketing (Succeeded→completed,
// Pending/Claiming/Running→running, Paused→paused, Canceling/Cancelled→cancelled,
// Failed→a failed/blocked bar). Always available (informer cache, no seam).
type RunsByStatusTile struct {
	TileStatus
	ByPhase map[string]int `json:"byPhase"`
	Total   int            `json:"total"`
}

// TokensTile is the summed per-run token usage over the window (ISI-4238). Only
// input/output/total exist on the CR (no cost/reasoning/cache) — EstimatedUSD is
// nil unless a price table is wired. RunsCounted is the number of in-window Runs
// that contributed a usage figure (provenance: a low count next to a big total
// flags a single dominant Run). Always available (informer cache, no seam).
type TokensTile struct {
	TileStatus
	Input        int64    `json:"input"`
	Output       int64    `json:"output"`
	Total        int64    `json:"total"`
	EstimatedUSD *float64 `json:"estimatedUsd,omitempty"`
	RunsCounted  int      `json:"runsCounted"`
}

// ProjectOverviewSeries is the single composed payload behind the overview
// route. Project + Window identify the scope; the three series degrade
// independently (only ticketsByStatus can degrade — the CRD series always
// serve).
type ProjectOverviewSeries struct {
	Project         ProjectRef        `json:"project"`
	Window          OverviewWindow    `json:"window"`
	TicketsByStatus TicketsSeriesTile `json:"ticketsByStatus"`
	RunsByStatus    RunsByStatusTile  `json:"runsByStatus"`
	Tokens          TokensTile        `json:"tokens"`
}

// StatusHistorySource is the coord seam for the tickets-by-status-over-time
// rollup (ISI-4509). teamID scopes tenancy (coord.work_item.team_id; "" ⇒
// trusted fleet-admin); projectID is the coord.work_item.project_id (Project CR
// UID). Implemented by *coord.WorkItemReadStore.ProjectStatusSnapshots.
type StatusHistorySource interface {
	ProjectStatusSnapshots(ctx context.Context, teamID, projectID string, from, to time.Time) ([]coord.StatusSnapshot, error)
}

// OverviewService composes the project-overview series. reader is the informer
// cache (Runs + Project resolution); status is the optional coord seam.
type OverviewService struct {
	reader client.Reader
	status StatusHistorySource
}

// NewOverviewService builds the read model. reader MUST have api/v1alpha1
// registered. status is optional (nil ⇒ the ticketsByStatus tile renders the
// documented "not configured" state; runs/tokens still serve).
func NewOverviewService(reader client.Reader, status StatusHistorySource) *OverviewService {
	return &OverviewService{reader: reader, status: status}
}

// Series composes the payload for one Project over [from, to]. Project scope
// resolves exactly like the dashboard (admin ⇒ fleet-wide, else Team-fenced),
// yielding the CRD (namespace, name) for the Run reads and the UID for the coord
// query.
func (s *OverviewService) Series(ctx context.Context, auth discussion.AuthorContext, projectID string, from, to time.Time) (ProjectOverviewSeries, error) {
	var ns, name, uid string
	var err error
	if auth.IsAdmin {
		ns, name, uid, err = resolveProjectFleetWideWithUID(ctx, s.reader, projectID)
	} else {
		ns, name, uid, err = resolveProjectInTeamWithUID(ctx, s.reader, auth.TeamID.String(), projectID)
	}
	if err != nil {
		return ProjectOverviewSeries{}, err
	}

	out := ProjectOverviewSeries{
		Project: ProjectRef{Name: name, Namespace: ns},
		Window:  OverviewWindow{From: from, To: to, Bucket: "day"},
	}

	// ── runs + tokens: informer cache, no seam — one List, two projections. ──
	// Run CRs live in the squad's execution namespace (Team.Status.Namespace), not
	// the Project's home namespace (ISI-4565) — resolve it or the tile reads empty.
	runNS, nerr := runNamespaceForHome(ctx, s.reader, ns)
	if nerr != nil {
		return ProjectOverviewSeries{}, nerr
	}
	runs, rerr := s.projectRuns(ctx, runNS, name)
	if rerr != nil {
		out.RunsByStatus = RunsByStatusTile{TileStatus: degradedTile(rerr.Error()), ByPhase: map[string]int{}}
		out.Tokens = TokensTile{TileStatus: degradedTile(rerr.Error())}
	} else {
		out.RunsByStatus = runsByStatus(runs, from, to)
		out.Tokens = tokenSum(runs, from, to)
	}

	// ── ticketsByStatus: coord seam (ISI-4509 status-history rollup). ────────
	switch s.status {
	case nil:
		out.TicketsByStatus = TicketsSeriesTile{TileStatus: degradedTile("source not wired: coordination status-history (coord.audit_log state_transition)")}
	default:
		teamID := ""
		if !auth.IsAdmin {
			teamID = auth.TeamID.String()
		}
		snaps, serr := s.status.ProjectStatusSnapshots(ctx, teamID, uid, from, to)
		if serr != nil {
			out.TicketsByStatus = TicketsSeriesTile{TileStatus: degradedTile(serr.Error())}
		} else {
			out.TicketsByStatus = TicketsSeriesTile{TileStatus: TileStatus{Available: true}, Snapshots: snaps}
		}
	}

	return out, nil
}

// projectRuns lists the Project's Runs from the informer cache (spec.projectRef
// filter, exactly like DashboardService.liveRuns).
func (s *OverviewService) projectRuns(ctx context.Context, ns, project string) ([]ksquadv1.Run, error) {
	var runs ksquadv1.RunList
	if err := s.reader.List(ctx, &runs, client.InNamespace(ns)); err != nil {
		return nil, err
	}
	out := make([]ksquadv1.Run, 0, len(runs.Items))
	for i := range runs.Items {
		if runs.Items[i].Spec.ProjectRef.Name == project {
			out = append(out, runs.Items[i])
		}
	}
	return out, nil
}

// runInWindow is the window-overlap predicate: a Run counts if it was active at
// any point in [from, to] — its start (claim, else creation) is at/before `to`
// and it had not already terminated before `from`. Runs carry no first-class
// window column, so start=runStart and end=latestConditionTime(terminal only)
// are the best-effort anchors (same as the RunSummary projection).
func runInWindow(run *ksquadv1.Run, from, to time.Time) bool {
	start := runStart(run)
	if start.After(to) {
		return false
	}
	if isTerminal(run.Status.Phase) {
		if end := latestConditionTime(run); end != nil && end.Before(from) {
			return false
		}
	}
	return true
}

// runsByStatus groups the in-window Runs by coalesced RunPhase (blank ⇒
// Pending), lowercased. Deterministic zero-length map when nothing is in window.
func runsByStatus(runs []ksquadv1.Run, from, to time.Time) RunsByStatusTile {
	byPhase := map[string]int{}
	total := 0
	for i := range runs {
		if !runInWindow(&runs[i], from, to) {
			continue
		}
		phase := string(runs[i].Status.Phase)
		if phase == "" {
			phase = string(ksquadv1.RunPhasePending)
		}
		byPhase[strings.ToLower(phase)]++
		total++
	}
	return RunsByStatusTile{TileStatus: TileStatus{Available: true}, ByPhase: byPhase, Total: total}
}

// tokenSum sums per-run TotalTokenUsage (ISI-4238) over the in-window Runs. A Run
// with no usage figure contributes nothing (and is not counted in RunsCounted) —
// an honest "—" beats a fabricated zero-inflated denominator. Tokens are a whole-
// run total (not time-bucketed on the CR), so a Run straddling the window edge
// contributes its full total; this is legibility, not billing (OQ14).
func tokenSum(runs []ksquadv1.Run, from, to time.Time) TokensTile {
	tile := TokensTile{TileStatus: TileStatus{Available: true}}
	for i := range runs {
		if !runInWindow(&runs[i], from, to) {
			continue
		}
		u := runs[i].Status.TotalTokenUsage
		if u == nil {
			continue
		}
		tile.Input += u.InputTokens
		tile.Output += u.OutputTokens
		tile.Total += u.TotalTokens
		tile.RunsCounted++
	}
	return tile
}

// ============================================================================
// Handler — GET /api/projects/{projectId}/overview behind the §13 choke point
// ============================================================================

// projectOverview is the handler behind the route. Window is parsed from the
// query (?window=Nd | ?from=&to= RFC3339); a bad window is 400. Scope errors map
// like the dashboard (404 no team/project, 409 ambiguous admin name).
func (s *Server) projectOverview(svc *OverviewService) http.HandlerFunc {
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
		projectID := mux.Vars(r)["projectId"]
		series, err := svc.Series(r.Context(), auth, projectID, from, to)
		switch {
		case errors.Is(err, ErrTeamNotFound):
			writeJSONError(w, http.StatusNotFound, "no overview for this team scope")
		case errors.Is(err, ErrProjectNotFound):
			writeJSONError(w, http.StatusNotFound, "no overview for this project")
		case errors.Is(err, ErrProjectAmbiguous):
			writeJSONError(w, http.StatusConflict, "project name ambiguous across squads; address by uid")
		case err != nil:
			writeJSONError(w, http.StatusBadGateway, "overview read model unavailable")
		default:
			writeJSON(w, http.StatusOK, series)
		}
	}
}

// parseOverviewWindow resolves the [from, to] window from the request, relative
// to now. Precedence: explicit ?from/?to (RFC3339) beats ?window=Nd; neither ⇒
// the default trailing 30 days. The window is clamped to overviewMaxWindowDays
// to bound the daily reconstruction. Errors (bad format, to<from, absurd width)
// are 400 — never a silent clamp that would answer a different question.
func parseOverviewWindow(r *http.Request, now time.Time) (time.Time, time.Time, error) {
	now = now.UTC()
	q := r.URL.Query()

	fromStr, toStr := q.Get("from"), q.Get("to")
	if fromStr != "" || toStr != "" {
		to := now
		if toStr != "" {
			t, err := time.Parse(time.RFC3339, toStr)
			if err != nil {
				return time.Time{}, time.Time{}, errors.New("invalid 'to' (want RFC3339)")
			}
			to = t.UTC()
		}
		if fromStr == "" {
			return time.Time{}, time.Time{}, errors.New("'from' required when 'to' is set")
		}
		from, err := time.Parse(time.RFC3339, fromStr)
		if err != nil {
			return time.Time{}, time.Time{}, errors.New("invalid 'from' (want RFC3339)")
		}
		from = from.UTC()
		if to.Before(from) {
			return time.Time{}, time.Time{}, errors.New("'to' precedes 'from'")
		}
		if to.Sub(from) > time.Duration(overviewMaxWindowDays)*24*time.Hour {
			return time.Time{}, time.Time{}, errors.New("window too wide (max 366 days)")
		}
		return from, to, nil
	}

	if wStr := q.Get("window"); wStr != "" {
		days, err := parseWindowDays(wStr)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		return now.Add(-time.Duration(days) * 24 * time.Hour), now, nil
	}

	return now.Add(-overviewDefaultWindow), now, nil
}

// parseWindowDays parses a "Nd" trailing-window token (e.g. "7d", "30d", "90d")
// to a day count in [1, overviewMaxWindowDays].
func parseWindowDays(w string) (int, error) {
	w = strings.TrimSpace(strings.ToLower(w))
	if !strings.HasSuffix(w, "d") {
		return 0, errors.New("invalid 'window' (want Nd, e.g. 30d)")
	}
	n, err := strconv.Atoi(strings.TrimSuffix(w, "d"))
	if err != nil || n < 1 {
		return 0, errors.New("invalid 'window' (want a positive Nd, e.g. 30d)")
	}
	if n > overviewMaxWindowDays {
		return 0, errors.New("window too wide (max 366 days)")
	}
	return n, nil
}
