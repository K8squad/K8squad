package apiserver

// ============================================================================
// Per-Project dashboard read model (story 8.8a / ISI-2906) — the ONE payload
// every 8.8b–8.8f tile draws from, served at GET /api/projects/{projectId}/dashboard.
// ============================================================================
//
// Arch §13 (r24 dashboard read model), ADR-020: the dashboard is a read model
// over EXISTING stores — no new aggregation service and no rollup datastore
// (R6/ponytail). The payload composes four independently-queried sources:
//
//   - the `coord` audit seam      → tickets-by-status, recent tickets, throughput,
//                                   pending approvals (8.8b/8.8c; Epic 2 §6.5)
//   - the `scm` mirror seam       → PR states for the mini-board (8.8d; §5.4)
//   - the OTel metrics seam       → token/cost totals + trend (8.8e; §17.2)
//   - Run/claim state (this file) → live Runs (8.8f; §6/§8, informer cache)
//
// Per-tile degradation (8.8a AC3): each source is queried independently and an
// unavailable source degrades ONLY its tile — an unwired seam renders
// `available:false` with a machine-readable reason (never a fake number,
// FR-I3 provenance; "not configured", never a whole-dashboard failure). The
// live-Runs tile needs no external seam (Runs are CRDs; the informer cache is
// the store), so it serves whenever the caller's Team resolves.
//
// Authorization (8.8a AC2): the route rides the SAME §12.3 deny-by-default
// BFFAuthz middleware as every other gated surface — there is NO
// dashboard-specific authz path. The projection is scoped to the caller's
// Team namespace (AuthorContext.TeamID → §12.1 tenancy root); a Project
// outside it does not exist (404, existence-hiding NFR-SEC5).
//
// Seam ownership (8.8a AC4): coord/scm/metrics sources are OPTIONAL seams
// declared here and wired by their owning epics — Epic 2 (coord), Epic 11.3
// (PR sync, ISI-2738…), Epic 13.4 (token metering, T38). 8.8a does NOT block
// on them; the tiles light up as those epics land.

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
)

// ErrProjectNotFound is returned when the named Project does not exist in the
// caller's Team namespace. The handler answers 404 — existence-hiding: a
// foreign-Project caller cannot distinguish deny from not-found.
var ErrProjectNotFound = errors.New("apiserver: no project matches the caller's team scope")

// ============================================================================
// Tile envelope — every tile carries its own availability, never a fake value.
// ============================================================================

// TileStatus is the shared per-tile degradation envelope (8.8a AC3). A tile
// with Available=false MUST render an explicit empty/"not configured" state;
// Reason is machine-readable ("source not wired" | the source's error).
type TileStatus struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

func degradedTile(reason string) TileStatus { return TileStatus{Available: false, Reason: reason} }

// ============================================================================
// 8.8b/8.8c — tickets + pending approvals (coord audit seam, Epic 2 §6.5)
// ============================================================================

// TicketSummary is one recent ticket row (title, status badge, age anchor).
type TicketSummary struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Status    string     `json:"status"`
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
}

// ThroughputPoint is resolved-per-day count over the dashboard window (8.8b
// "throughput over time", from the Epic 2 coordination record).
type ThroughputPoint struct {
	Date  string `json:"date"` // YYYY-MM-DD
	Count int    `json:"count"`
}

// PendingApproval is one work item gated on a human decision (Epic 2.12
// `blocked_reason=needs_approval`). The gate MECHANISM is 2.12; this is its
// console read surface (8.8c) — raise/read only, approve/reject verbs belong
// to the 2.12 apiserver action and are linked to, never duplicated here.
type PendingApproval struct {
	TicketID        string     `json:"ticketId"`
	Title           string     `json:"title"`
	RequestingAgent string     `json:"requestingAgent,omitempty"`
	RunID           string     `json:"runId,omitempty"`
	RaisedAt        *time.Time `json:"raisedAt,omitempty"`
}

// TicketsTile feeds the 8.8b KPI "Tickets by status" card, the Recent Tickets
// list, and the 8.8c Pending Approvals queue. CanAct is the caller's
// write-level membership on the Project (§12.3): a viewer sees the queue
// read-only (approve/reject hidden; the apiserver 403s regardless — hidden in
// UI is defense-in-depth, the server gate is authoritative).
type TicketsTile struct {
	TileStatus
	ByStatus         map[string]int    `json:"byStatus,omitempty"`
	Recent           []TicketSummary   `json:"recent,omitempty"`
	Throughput       []ThroughputPoint `json:"throughput,omitempty"`
	PendingApprovals []PendingApproval `json:"pendingApprovals,omitempty"`
	CanAct           bool              `json:"canAct,omitempty"`
}

// ============================================================================
// 8.8d — PR mini-board (scm mirror seam, §5.4 review_state)
// ============================================================================

// PullRequest is one mirrored PR row. HeadSHA→Run correlation
// (head_sha→run.commit_sha) fills RunName where a producing Run matches.
// ReviewState is the §5.4 `review_state` mapped to a mini-board column:
// ready-for-review | draft | blocked | merged — the seam normalizes provider
// states into these four; anything else is dropped from the board rather
// than mis-bucketed.
type PullRequest struct {
	Number      int        `json:"number"`
	Title       string     `json:"title"`
	Branch      string     `json:"branch,omitempty"`
	HeadSHA     string     `json:"headSha,omitempty"`
	RunName     string     `json:"runName,omitempty"`
	ReviewState string     `json:"reviewState,omitempty"`
	UpdatedAt   *time.Time `json:"updatedAt,omitempty"`
}

// Mini-board column values (§5.4 review_state mapping).
const (
	PRReadyForReview = "ready-for-review"
	PRDraft          = "draft"
	PRBlocked        = "blocked"
	PRMerged         = "merged"
)

// PRTile groups PRs by review_state into the mini-board columns:
// ready-for-review / draft / blocked / merged (§5.4).
type PRTile struct {
	TileStatus
	ReadyForReview []PullRequest `json:"readyForReview,omitempty"`
	Draft          []PullRequest `json:"draft,omitempty"`
	Blocked        []PullRequest `json:"blocked,omitempty"`
	Merged         []PullRequest `json:"merged,omitempty"`
}

// ============================================================================
// 8.8e — token consumption + trend (OTel metrics seam, §17.2 / Epic 13.4)
// ============================================================================

// TokenTrendPoint is tokens/day over the selectable window (time-series over
// the same seam — no new store).
type TokenTrendPoint struct {
	Date   string `json:"date"` // YYYY-MM-DD
	Tokens int64  `json:"tokens"`
}

// ConsumptionTile carries current totals (per user/agent/Run/Project axes are
// the seam's to fill; the dashboard renders the Project rollup) plus trend and
// an estimated cost via the configurable price table where set. Figures are
// best-effort/runtime-reported (OQ14) — legibility, not billing authority.
type ConsumptionTile struct {
	TileStatus
	TotalTokens   int64             `json:"totalTokens,omitempty"`
	EstimatedCost *float64          `json:"estimatedCost,omitempty"`
	Currency      string            `json:"currency,omitempty"`
	Trend         []TokenTrendPoint `json:"trend,omitempty"`
}

// ============================================================================
// 8.8f — Live Runs (Run/claim state; no external seam)
// ============================================================================

// LiveRun is the agent↔task↔Project mapping row (who is running what), a read
// model derived from Run/claim state (FR-I3 provenance). PausedReason /
// ResumeAt / FallbackModel surface the 13.9 rate-limit + fallback indicators:
// a Run Paused(rate_limited) (2.11/3.7) carries its resume clock in a
// `RateLimited` condition whose message ends with `resume_at=<RFC3339>` until
// the status subresource grows a first-class field; the parse is best-effort
// and absent indicators simply render nothing.
type LiveRun struct {
	Name          string     `json:"name"`
	WorkItem      string     `json:"workItem,omitempty"`
	Agent         string     `json:"agent,omitempty"` // first selected Agent (spec.agents[0])
	Phase         string     `json:"phase"`
	ClaimedAt     *time.Time `json:"claimedAt,omitempty"`
	PausedReason  string     `json:"pausedReason,omitempty"`
	ResumeAt      *time.Time `json:"resumeAt,omitempty"`
	FallbackModel string     `json:"fallbackModel,omitempty"`
}

// LiveRunsTile is the 8.8f panel body. SSE updating rides the §4.4 progress
// bus (the console refetches this payload on bus events — no polling, no new
// transport); the payload itself is a plain level-triggered read.
type LiveRunsTile struct {
	TileStatus
	Runs []LiveRun `json:"runs"`
}

// ============================================================================
// The payload (8.8a AC1: ONE RBAC-filtered payload, every tile's source)
// ============================================================================

// ProjectRef identifies the dashboard's scope: the Project name within the
// caller's Team namespace.
type ProjectRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// ProjectDashboard is the single composed payload behind the per-Project
// dashboard. Project identifies the scope; the four tiles degrade
// independently.
type ProjectDashboard struct {
	Project      ProjectRef      `json:"project"`
	Tickets      TicketsTile     `json:"tickets"`
	PullRequests PRTile          `json:"pullRequests"`
	Consumption  ConsumptionTile `json:"consumption"`
	LiveRuns     LiveRunsTile    `json:"liveRuns"`
}

// ============================================================================
// Source seams — owned by their epics, wired when those epics land (8.8a AC4)
// ============================================================================

// TicketFacts is what the Epic 2 coord audit seam hands the dashboard.
type TicketFacts struct {
	ByStatus         map[string]int
	Recent           []TicketSummary
	Throughput       []ThroughputPoint
	PendingApprovals []PendingApproval
	CanAct           bool // caller holds write-level membership (§12.3/Epic 15.4)
}

// TicketSource is the coord-audit seam (Epic 2 §6.5; work items + approval
// gates are Postgres rows, ADR-001 — never CRDs). Implementations MUST scope
// strictly to (namespace, project) and to the caller's principal for CanAct.
type TicketSource interface {
	TicketFacts(ctx context.Context, principal, namespace, project string) (TicketFacts, error)
}

// PRSource is the scm-mirror seam (§5.4 review_state, Epic 11.3). Returns PRs
// for the Project's synced repo; an unsynced repo returns (nil, nil) — an
// EMPTY board, not an error (8.8a per-tile rule / 8.8d AC2).
type PRSource interface {
	PullRequests(ctx context.Context, namespace, project string) ([]PullRequest, error)
}

// MetricsSource is the OTel metrics query seam (§17.2, Epic 13.4): a
// time-series query over `ksquad.agent.tokens` — no new store behind it.
type MetricsSource interface {
	TokenConsumption(ctx context.Context, namespace, project string) (totalTokens int64, estimatedCost *float64, currency string, trend []TokenTrendPoint, err error)
}

// ============================================================================
// Service — composes the payload; per-tile degradation is the invariant.
// ============================================================================

// DashboardService is the 8.8a read model. It reads Runs from a client.Reader
// (the host's informer cache) and queries the optional seams independently:
// a nil seam or a failing source degrades exactly its tile.
type DashboardService struct {
	reader  client.Reader
	tickets TicketSource
	prs     PRSource
	metrics MetricsSource
}

// NewDashboardService builds the dashboard read model. reader MUST have
// api/v1alpha1 registered. Every seam is optional (nil ⇒ its tile renders the
// documented "not configured" state — 8.8a does not block on Epics 2/11/13).
func NewDashboardService(reader client.Reader, tickets TicketSource, prs PRSource, metrics MetricsSource) *DashboardService {
	return &DashboardService{reader: reader, tickets: tickets, prs: prs, metrics: metrics}
}

// Dashboard composes the per-Project payload. It resolves the caller's Team by
// UID (the §7.3.3 tenancy root), requires the named Project to live in that
// Team's namespace (else ErrProjectNotFound → 404 existence-hiding), then
// fills the four tiles independently: the live-Runs tile from the cache, the
// seam-backed tiles from their wired sources with per-tile error capture.
func (s *DashboardService) Dashboard(ctx context.Context, auth discussion.AuthorContext, projectID string) (ProjectDashboard, error) {
	ns, err := s.teamNamespace(ctx, auth.TeamID.String())
	if err != nil {
		return ProjectDashboard{}, err
	}

	// Existence + tenancy of the Project: must be in the caller's Team namespace.
	var projects ksquadv1.ProjectList
	if err := s.reader.List(ctx, &projects, client.InNamespace(ns)); err != nil {
		return ProjectDashboard{}, err
	}
	found := false
	for i := range projects.Items {
		if projects.Items[i].Name == projectID {
			found = true
			break
		}
	}
	if !found {
		return ProjectDashboard{}, ErrProjectNotFound
	}

	out := ProjectDashboard{Project: ProjectRef{Name: projectID, Namespace: ns}}

	// ── 8.8f live Runs: informer cache, no seam — serves whenever the Team resolves.
	live, err := s.liveRuns(ctx, ns, projectID)
	switch {
	case err != nil:
		out.LiveRuns = LiveRunsTile{TileStatus: degradedTile(err.Error())}
	default:
		out.LiveRuns = LiveRunsTile{TileStatus: TileStatus{Available: true}, Runs: live}
	}

	// ── 8.8b/8.8c tickets + approvals: coord seam (Epic 2).
	switch {
	case s.tickets == nil:
		out.Tickets = TicketsTile{TileStatus: degradedTile("source not wired: coordination store (Epic 2 §6.5)")}
	default:
		facts, terr := s.tickets.TicketFacts(ctx, auth.Principal, ns, projectID)
		switch {
		case terr != nil:
			out.Tickets = TicketsTile{TileStatus: degradedTile(terr.Error())}
		default:
			out.Tickets = TicketsTile{
				TileStatus:       TileStatus{Available: true},
				ByStatus:         facts.ByStatus,
				Recent:           facts.Recent,
				Throughput:       facts.Throughput,
				PendingApprovals: facts.PendingApprovals,
				CanAct:           facts.CanAct,
			}
		}
	}

	// ── 8.8d PR mini-board: scm mirror seam (Epic 11.3). Unsynced repo ⇒ EMPTY board.
	switch {
	case s.prs == nil:
		out.PullRequests = PRTile{TileStatus: degradedTile("source not wired: scm PR mirror (Epic 11.3, §5.4)")}
	default:
		prs, perr := s.prs.PullRequests(ctx, ns, projectID)
		switch {
		case perr != nil:
			out.PullRequests = PRTile{TileStatus: degradedTile(perr.Error())}
		default:
			tile := PRTile{TileStatus: TileStatus{Available: true}}
			for _, pr := range prs {
				switch pr.ReviewState {
				case PRReadyForReview:
					tile.ReadyForReview = append(tile.ReadyForReview, pr)
				case PRDraft:
					tile.Draft = append(tile.Draft, pr)
				case PRBlocked:
					tile.Blocked = append(tile.Blocked, pr)
				case PRMerged:
					tile.Merged = append(tile.Merged, pr)
				}
				// Unknown review_state: dropped — never mis-bucketed (8.8d AC1).
			}
			out.PullRequests = tile
		}
	}

	// ── 8.8e consumption: metrics seam (Epic 13.4). Unwired ⇒ throughput-without-cost.
	switch {
	case s.metrics == nil:
		out.Consumption = ConsumptionTile{TileStatus: degradedTile("source not wired: metrics query seam (Epic 13.4, §17.2)")}
	default:
		total, cost, currency, trend, merr := s.metrics.TokenConsumption(ctx, ns, projectID)
		if merr != nil {
			out.Consumption = ConsumptionTile{TileStatus: degradedTile(merr.Error())}
		} else {
			out.Consumption = ConsumptionTile{
				TileStatus:    TileStatus{Available: true},
				TotalTokens:   total,
				EstimatedCost: cost,
				Currency:      currency,
				Trend:         trend,
			}
		}
	}

	return out, nil
}

// teamNamespace resolves the caller's Team UID to its namespace (the §12.1
// "a squad IS a namespace" boundary). A UID that resolves to no Team is
// ErrTeamNotFound (404).
func (s *DashboardService) teamNamespace(ctx context.Context, teamUID string) (string, error) {
	if teamUID == "" {
		return "", ErrTeamNotFound
	}
	var teams ksquadv1.TeamList
	if err := s.reader.List(ctx, &teams); err != nil {
		return "", err
	}
	for i := range teams.Items {
		if string(teams.Items[i].UID) == teamUID {
			return teams.Items[i].Namespace, nil
		}
	}
	return "", ErrTeamNotFound
}

// liveRuns projects the Project's Runs from the informer cache into the 8.8f
// rows: the agent↔task mapping, the coalesced phase, and the 13.9 indicators
// (paused-reason, resume-at countdown, active fallback model) from Run
// conditions. Output is deterministic (sorted by name).
func (s *DashboardService) liveRuns(ctx context.Context, ns, project string) ([]LiveRun, error) {
	var runs ksquadv1.RunList
	if err := s.reader.List(ctx, &runs, client.InNamespace(ns)); err != nil {
		return nil, err
	}
	out := make([]LiveRun, 0, len(runs.Items))
	for i := range runs.Items {
		run := &runs.Items[i]
		if run.Spec.ProjectRef.Name != project {
			continue
		}
		out = append(out, liveRunRow(run))
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}

// liveRunRow projects one Run. Phase coalesces to "Pending" when the
// reconciler has not yet observed the Run (blank status.phase) so the panel
// never renders a blank cell (same rule as the 8.1 overview).
func liveRunRow(run *ksquadv1.Run) LiveRun {
	phase := string(run.Status.Phase)
	if phase == "" {
		phase = string(ksquadv1.RunPhasePending)
	}
	row := LiveRun{
		Name:     run.Name,
		WorkItem: run.Spec.WorkItemRef,
		Phase:    phase,
	}
	if len(run.Spec.Agents) > 0 {
		row.Agent = run.Spec.Agents[0].Name
	}
	if run.Status.ClaimedAt != nil {
		t := run.Status.ClaimedAt.Time
		row.ClaimedAt = &t
	}
	for i := range run.Status.Conditions {
		c := &run.Status.Conditions[i]
		switch c.Type {
		case "Paused":
			row.PausedReason = c.Reason // e.g. rate_limited (3.7), credential (7.4)
			if row.ResumeAt == nil {
				if t, ok := parseResumeAt(c.Message); ok {
					row.ResumeAt = &t
				}
			}
		case "Fallback":
			row.FallbackModel = c.Reason // active fallback model (13.9)
		}
	}
	return row
}

// parseResumeAt extracts a trailing `resume_at=<RFC3339>` marker from a
// condition message (the documented interim carriage for the 2.11/3.7 resume
// clock until RunStatus grows the first-class field). Best-effort: absent or
// malformed ⇒ no countdown, never an error.
func parseResumeAt(msg string) (time.Time, bool) {
	const marker = "resume_at="
	i := strings.LastIndex(msg, marker)
	if i < 0 {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(msg[i+len(marker):]))
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// ============================================================================
// Handler — GET /api/projects/{projectId}/dashboard behind the §13 choke point
// ============================================================================

// projectDashboard is the handler behind the route. BFFAuthz has already
// resolved the AuthorContext; the projection reads NOTHING from the request
// except the path variable. Statuses: 401 unauthenticated (defence in depth),
// 404 no Team scope or foreign/unknown Project (existence-hiding), 200 with
// whatever tiles survived their sources.
func (s *Server) projectDashboard(svc *DashboardService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := discussion.AuthFromContext(r.Context())
		if !ok || auth.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		projectID := mux.Vars(r)["projectId"]
		dash, err := svc.Dashboard(r.Context(), auth, projectID)
		switch {
		case errors.Is(err, ErrTeamNotFound):
			writeJSONError(w, http.StatusNotFound, "no dashboard for this team scope")
		case errors.Is(err, ErrProjectNotFound):
			writeJSONError(w, http.StatusNotFound, "no dashboard for this project")
		case err != nil:
			writeJSONError(w, http.StatusBadGateway, "dashboard read model unavailable")
		default:
			writeJSON(w, http.StatusOK, dash)
		}
	}
}
