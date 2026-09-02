package apiserver

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
)

// muxVar reads a path variable, empty when absent.
func muxVar(r *http.Request, key string) string { return mux.Vars(r)[key] }

// ============================================================================
// Agents org read model (stories 8.10 + 8.11 / ISI-3548, child of ISI-3543) —
// the projection the console Agents surface renders against:
//
//	GET /api/teams/{teamId}/org           → TeamOrg (Team → Agent → Role diagram, 8.10)
//	GET /api/teams/{teamId}/status/stream → per-agent status deltas over SSE (8.10 live)
//	GET /api/agents/{agentId}             → OrgAgent (agent detail header, 8.11)
//	GET /api/agents/{agentId}/runs        → []RunSummary (agent run history, 8.11)
//
// ============================================================================
//
// Source of truth is the controller-runtime informer cache (same discipline as overview.go /
// credentials.go): Teams, Agents, AgentRuntimes, Roles and Runs are CRDs, so the org hierarchy and
// each agent's live status are the cache's level-triggered projection of etcd — no second store to
// keep in sync (R6: the console is a PURE CONSUMER, no compose/edit — that stays 8.5). The
// projection is Team-SCOPED: it answers only for the caller's authorized Team (AuthorContext.TeamID,
// §7.3.3 tenancy root), and a deny is EXISTENCE-HIDING (404, never 403 — a Team-B caller cannot
// tell a Team-A org/agent from a missing one). Field names match console/lib/agents/types.ts.
//
// Identifiers: teamId and agentId are OBJECT UIDs (the same rename-proof scope key overview.go uses
// for the Team). A Run is identified by its object NAME everywhere in the console (the SSE hub's
// {runId}, SquadOverview's deep-links), so currentRunId and RunSummary.id are the Run NAME.

// AgentStatus is the four-value legibility bucket the org diagram paints, derived from the Agent's
// current Run phase (§8) + Paused condition reason (§5.2). NOT the raw seven-value RunPhase enum.
const (
	AgentStatusIdle    = "idle"
	AgentStatusRunning = "running"
	AgentStatusBlocked = "blocked"
	AgentStatusPaused  = "paused"
)

// Paused sub-reasons surfaced on a paused agent/run (§5.2, story 7.6/8.11): the vendor-neutral axis
// the chip renders as "paused: rate-limited" / "paused: credential".
const (
	PausedReasonCredential = "credential"
	PausedReasonRateLimit  = "rate_limited"
)

// RoleBadge is a Role badge on an Agent node (read-only, from the Role CRD, §5.1). ID is the Role
// object UID; Name is its metadata.name.
type RoleBadge struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// OrgAgent is an Agent node in the org diagram (8.10) and the header of the detail page (8.11).
// RuntimeType is the resolved AgentRuntime.type (§5.3); Status is the derived four-value bucket;
// PausedReason (when paused) carries the §5.2 sub-state; CurrentRunID is the Agent's active Run
// (nil when idle) — the deep-link target for a live drill-in.
type OrgAgent struct {
	ID           string      `json:"id"`
	Name         string      `json:"name"`
	RuntimeType  string      `json:"runtimeType"`
	Status       string      `json:"status"`
	PausedReason *string     `json:"pausedReason,omitempty"`
	Roles        []RoleBadge `json:"roles"`
	CurrentRunID *string     `json:"currentRunId,omitempty"`
}

// TeamOrg is the Team → Agent → Role hierarchy (8.10), a read-only projection over the CRDs.
type TeamOrg struct {
	TeamID   string     `json:"teamId"`
	TeamName string     `json:"teamName"`
	Agents   []OrgAgent `json:"agents"`
}

// AgentStatusDelta is one per-agent status frame the org diagram overlays on its snapshot (8.10
// live SSE). Matches the client contract in lib/agents/useTeamStatus.ts.
type AgentStatusDelta struct {
	AgentID      string  `json:"agentId"`
	Status       string  `json:"status"`
	PausedReason *string `json:"pausedReason,omitempty"`
	CurrentRunID *string `json:"currentRunId,omitempty"`
}

// TokenUsage is best-effort runtime-reported token usage (§11 OQ14) — legibility, NOT the billing
// authority (authoritative consumption = 8.8 via the OTel metering spine). The CRD status carries
// no token counts, so the projection leaves this nil rather than fabricating a figure (no-placeholder
// discipline, FR-I3); a future runtime-reported source fills it in.
type TokenUsage struct {
	Input  *int64 `json:"input,omitempty"`
	Output *int64 `json:"output,omitempty"`
	Total  *int64 `json:"total,omitempty"`
}

// RunSummary is one row in an Agent's Run history (8.11). ID is the Run object NAME (the console-wide
// run identifier: SSE {runId}, /runs/{id} deep-links). DurationSeconds is server-computed (elapsed,
// or live-so-far for an active Run). TraceID / Tokens are best-effort and left empty when the CRD
// status has no source — an honest "—" beats a fabricated value.
type RunSummary struct {
	ID              string      `json:"id"`
	Phase           string      `json:"phase"`
	PausedReason    *string     `json:"pausedReason,omitempty"`
	WorkItemRef     string      `json:"workItemRef,omitempty"`
	StartedAt       *time.Time  `json:"startedAt,omitempty"`
	EndedAt         *time.Time  `json:"endedAt,omitempty"`
	DurationSeconds *int64      `json:"durationSeconds,omitempty"`
	Tokens          *TokenUsage `json:"tokens,omitempty"`
	TraceID         string      `json:"traceId,omitempty"`
}

// ErrAgentNotFound is returned when no Agent resolves to the given UID within the caller's Team
// namespace. The handler answers 404 — existence-hiding, so a foreign or missing Agent are
// indistinguishable to the caller (the 8.7d deny posture).
var ErrAgentNotFound = errors.New("apiserver: no agent matches the caller's team scope")

// OrgReader projects the Agents surface for a single Team (identified by its K8s object UID, the
// value AuthorContext.TeamID carries). Every method scopes STRICTLY to teamUID and never leaks
// another Team's Agents/Roles/Runs. Production wires the cache-backed reader; tests wire a fake
// client.Reader.
type OrgReader interface {
	// Org projects the Team → Agent → Role hierarchy for teamUID.
	Org(ctx context.Context, teamUID string) (TeamOrg, error)
	// Agent projects a single Agent (by UID) within teamUID's namespace.
	Agent(ctx context.Context, teamUID, agentUID string) (OrgAgent, error)
	// AgentRuns lists an Agent's Runs (most-recent-first), paginated by limit/offset.
	AgentRuns(ctx context.Context, teamUID, agentUID string, limit, offset int) ([]RunSummary, error)
	// AgentStatuses is the light per-agent status projection the SSE stream polls: it lists only
	// Agents + Runs (no Role/Runtime resolution), so a repeated poll stays cheap.
	AgentStatuses(ctx context.Context, teamUID string) ([]AgentStatusDelta, error)
}

// ClientOrgReader is the production OrgReader over any client.Reader (the informer cache in the
// host; a fake client in tests). Read-only. Team UID→(name, namespace) is memoized: a Team's UID
// and name are immutable for the object's lifetime, so the cluster-wide Team list runs at most once
// per distinct UID (the credentials.go perf discipline).
type ClientOrgReader struct {
	reader client.Reader
	mu     sync.RWMutex
	teams  map[string]teamIdentity // teamUID → identity (immutable once resolved)
}

type teamIdentity struct {
	name string
	ns   string
}

// NewClientOrgReader builds the org read model over a client.Reader (the informer cache in the
// host). The reader's scheme must have api/v1alpha1 registered.
func NewClientOrgReader(r client.Reader) *ClientOrgReader {
	return &ClientOrgReader{reader: r, teams: map[string]teamIdentity{}}
}

// resolveTeam resolves the Team UID to its (name, namespace) — the §12.1 tenancy root — memoized.
func (r *ClientOrgReader) resolveTeam(ctx context.Context, teamUID string) (teamIdentity, error) {
	if teamUID == "" {
		return teamIdentity{}, ErrTeamNotFound
	}
	r.mu.RLock()
	id, hit := r.teams[teamUID]
	r.mu.RUnlock()
	if hit {
		return id, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if id, ok := r.teams[teamUID]; ok {
		return id, nil
	}
	var teams ksquadv1.TeamList
	if err := r.reader.List(ctx, &teams); err != nil {
		return teamIdentity{}, err
	}
	for i := range teams.Items {
		if string(teams.Items[i].UID) == teamUID {
			id := teamIdentity{name: teams.Items[i].Name, ns: teams.Items[i].Namespace}
			r.teams[teamUID] = id
			return id, nil
		}
	}
	return teamIdentity{}, ErrTeamNotFound
}

// Org resolves the Team by UID, then projects every Agent in the Team's namespace into an org node —
// resolving its runtime flavor (AgentRuntime.type), its Role badge, and its derived live status from
// the Runs that select it. Deterministic: agents sort by name, roles by name.
func (r *ClientOrgReader) Org(ctx context.Context, teamUID string) (TeamOrg, error) {
	team, err := r.resolveTeam(ctx, teamUID)
	if err != nil {
		return TeamOrg{}, err
	}
	agents, runtimeType, roleByName, runsByAgent, err := r.load(ctx, team.ns)
	if err != nil {
		return TeamOrg{}, err
	}

	out := TeamOrg{TeamID: teamUID, TeamName: team.name, Agents: []OrgAgent{}}
	for i := range agents.Items {
		out.Agents = append(out.Agents, projectAgent(&agents.Items[i], runtimeType, roleByName, runsByAgent))
	}
	sort.Slice(out.Agents, func(a, b int) bool { return out.Agents[a].Name < out.Agents[b].Name })
	return out, nil
}

// Agent resolves a single Agent by UID within the caller's Team namespace and projects it. A UID
// that resolves to no Agent in this namespace (missing, or belonging to another Team) yields
// ErrAgentNotFound — existence-hiding.
func (r *ClientOrgReader) Agent(ctx context.Context, teamUID, agentUID string) (OrgAgent, error) {
	team, err := r.resolveTeam(ctx, teamUID)
	if err != nil {
		return OrgAgent{}, err
	}
	agents, runtimeType, roleByName, runsByAgent, err := r.load(ctx, team.ns)
	if err != nil {
		return OrgAgent{}, err
	}
	for i := range agents.Items {
		if string(agents.Items[i].UID) == agentUID {
			return projectAgent(&agents.Items[i], runtimeType, roleByName, runsByAgent), nil
		}
	}
	return OrgAgent{}, ErrAgentNotFound
}

// AgentRuns lists the Runs that select the Agent (by UID), most-recent-first, paginated. The Agent
// must resolve within the caller's Team namespace (existence-hiding 404 otherwise).
func (r *ClientOrgReader) AgentRuns(ctx context.Context, teamUID, agentUID string, limit, offset int) ([]RunSummary, error) {
	team, err := r.resolveTeam(ctx, teamUID)
	if err != nil {
		return nil, err
	}
	var agents ksquadv1.AgentList
	if err := r.reader.List(ctx, &agents, client.InNamespace(team.ns)); err != nil {
		return nil, err
	}
	var agentName string
	for i := range agents.Items {
		if string(agents.Items[i].UID) == agentUID {
			agentName = agents.Items[i].Name
			break
		}
	}
	if agentName == "" {
		return nil, ErrAgentNotFound
	}

	var runs ksquadv1.RunList
	if err := r.reader.List(ctx, &runs, client.InNamespace(team.ns)); err != nil {
		return nil, err
	}
	selected := runsForAgent(runs.Items, agentName, team.ns)
	// Most-recent-first: by claim time when known, else creation time, tie-broken by name so the
	// order is stable across identical timestamps.
	sort.Slice(selected, func(a, b int) bool {
		ta, tb := runStart(selected[a]), runStart(selected[b])
		if !ta.Equal(tb) {
			return ta.After(tb)
		}
		return selected[a].Name > selected[b].Name
	})

	out := []RunSummary{}
	for i := offset; i < len(selected) && (limit <= 0 || len(out) < limit); i++ {
		out = append(out, projectRunSummary(selected[i]))
	}
	return out, nil
}

// AgentStatuses lists only Agents + Runs (no Role/Runtime resolution) and derives each Agent's live
// status delta — the cheap projection the SSE stream re-polls.
func (r *ClientOrgReader) AgentStatuses(ctx context.Context, teamUID string) ([]AgentStatusDelta, error) {
	team, err := r.resolveTeam(ctx, teamUID)
	if err != nil {
		return nil, err
	}
	var agents ksquadv1.AgentList
	if err := r.reader.List(ctx, &agents, client.InNamespace(team.ns)); err != nil {
		return nil, err
	}
	var runs ksquadv1.RunList
	if err := r.reader.List(ctx, &runs, client.InNamespace(team.ns)); err != nil {
		return nil, err
	}
	runsByAgent := indexRunsByAgent(runs.Items, team.ns)

	out := make([]AgentStatusDelta, 0, len(agents.Items))
	for i := range agents.Items {
		a := &agents.Items[i]
		status, reason, runID := deriveStatus(runsByAgent[a.Name])
		out = append(out, AgentStatusDelta{
			AgentID:      string(a.UID),
			Status:       status,
			PausedReason: reason,
			CurrentRunID: runID,
		})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].AgentID < out[b].AgentID })
	return out, nil
}

// load lists the Agents in ns plus the lookup tables the per-agent projection needs: runtime
// flavor by AgentRuntime name, Role by name, and Runs indexed by the Agent they select.
func (r *ClientOrgReader) load(
	ctx context.Context, ns string,
) (agents ksquadv1.AgentList, runtimeType map[string]string, roleByName map[string]*ksquadv1.Role, runsByAgent map[string][]*ksquadv1.Run, err error) {
	if err = r.reader.List(ctx, &agents, client.InNamespace(ns)); err != nil {
		return
	}
	var runtimes ksquadv1.AgentRuntimeList
	if err = r.reader.List(ctx, &runtimes, client.InNamespace(ns)); err != nil {
		return
	}
	var roles ksquadv1.RoleList
	if err = r.reader.List(ctx, &roles, client.InNamespace(ns)); err != nil {
		return
	}
	var runs ksquadv1.RunList
	if err = r.reader.List(ctx, &runs, client.InNamespace(ns)); err != nil {
		return
	}

	runtimeType = make(map[string]string, len(runtimes.Items))
	for i := range runtimes.Items {
		runtimeType[runtimes.Items[i].Name] = runtimes.Items[i].Spec.Type
	}
	roleByName = make(map[string]*ksquadv1.Role, len(roles.Items))
	for i := range roles.Items {
		roleByName[roles.Items[i].Name] = &roles.Items[i]
	}
	runsByAgent = indexRunsByAgent(runs.Items, ns)
	return
}

// projectAgent projects one Agent into its org node: runtime flavor (falling back to the ref name
// when the AgentRuntime is not in the cache — legibility over a blank), its Role badge, and the
// derived live status.
func projectAgent(
	a *ksquadv1.Agent,
	runtimeType map[string]string,
	roleByName map[string]*ksquadv1.Role,
	runsByAgent map[string][]*ksquadv1.Run,
) OrgAgent {
	rt := runtimeType[a.Spec.RuntimeRef.Name]
	if rt == "" {
		rt = a.Spec.RuntimeRef.Name
	}
	roles := []RoleBadge{}
	if a.Spec.RoleRef.Name != "" {
		badge := RoleBadge{Name: a.Spec.RoleRef.Name}
		if role, ok := roleByName[a.Spec.RoleRef.Name]; ok {
			badge.ID = string(role.UID)
		}
		roles = append(roles, badge)
	}
	status, reason, runID := deriveStatus(runsByAgent[a.Name])
	return OrgAgent{
		ID:           string(a.UID),
		Name:         a.Name,
		RuntimeType:  rt,
		Status:       status,
		PausedReason: reason,
		Roles:        roles,
		CurrentRunID: runID,
	}
}

// indexRunsByAgent groups Runs by the Agent NAME they select (Run.spec.agents). An explicit
// foreign namespace in the ref is skipped (ObjectRef.Namespace empty means the Run's own
// namespace) so a cross-namespace name never attributes to a same-named Agent here — the
// credentials.go tenancy rule.
func indexRunsByAgent(items []ksquadv1.Run, ns string) map[string][]*ksquadv1.Run {
	byAgent := map[string][]*ksquadv1.Run{}
	for i := range items {
		run := &items[i]
		for _, ref := range run.Spec.Agents {
			if ref.Namespace != "" && ref.Namespace != ns {
				continue
			}
			byAgent[ref.Name] = append(byAgent[ref.Name], run)
		}
	}
	return byAgent
}

// runsForAgent returns the Runs selecting agentName (same tenancy rule as indexRunsByAgent).
func runsForAgent(items []ksquadv1.Run, agentName, ns string) []*ksquadv1.Run {
	var out []*ksquadv1.Run
	for i := range items {
		run := &items[i]
		for _, ref := range run.Spec.Agents {
			if ref.Namespace != "" && ref.Namespace != ns {
				continue
			}
			if ref.Name == agentName {
				out = append(out, run)
				break
			}
		}
	}
	return out
}

// deriveStatus folds an Agent's Runs into its four-value legibility bucket (§5.2/§8). The Agent's
// CURRENT Run is the freshest non-terminal one (latest claim/creation time); terminal Runs
// (Succeeded/Failed/Cancelled) never make an Agent look busy. With no active Run the Agent is idle.
// A Paused current Run yields paused (+ the credential/rate_limited sub-reason when the Paused
// condition names one); any other active phase (Pending/Claiming/Running/Canceling) is running.
//
// "blocked" is intentionally NOT synthesized here: no reconciler writes a dependency/human-wait
// signal today, and inventing one would fabricate state (FR-I3). The frontend paints blocked when a
// future reconciler condition supplies it.
func deriveStatus(runs []*ksquadv1.Run) (status string, pausedReason *string, currentRunID *string) {
	var current *ksquadv1.Run
	for _, run := range runs {
		if isTerminal(run.Status.Phase) {
			continue
		}
		if current == nil || runStart(run).After(runStart(current)) {
			current = run
		}
	}
	if current == nil {
		return AgentStatusIdle, nil, nil
	}
	name := current.Name
	if current.Status.Phase == ksquadv1.RunPhasePaused {
		return AgentStatusPaused, pausedReasonOf(current), &name
	}
	return AgentStatusRunning, nil, &name
}

// isTerminal reports whether a phase is a terminal Run outcome (never "current" work).
func isTerminal(p ksquadv1.RunPhase) bool {
	switch p {
	case ksquadv1.RunPhaseSucceeded, ksquadv1.RunPhaseFailed, ksquadv1.RunPhaseCancelled:
		return true
	default:
		return false
	}
}

// runStart is the Run's start instant for ordering/duration: its claim time when known, else its
// creation time.
func runStart(run *ksquadv1.Run) time.Time {
	if run.Status.ClaimedAt != nil {
		return run.Status.ClaimedAt.Time
	}
	return run.CreationTimestamp.Time
}

// pausedReasonOf extracts the §5.2 paused sub-reason from a Run's Paused condition: a
// credential-family reason (reusing the 8.6 vocabulary) → credential; a rate-limit reason →
// rate_limited; anything else → nil (a plain pause with no recognized sub-state).
func pausedReasonOf(run *ksquadv1.Run) *string {
	for i := range run.Status.Conditions {
		c := &run.Status.Conditions[i]
		if c.Type != "Paused" || c.Status != metav1.ConditionTrue {
			continue
		}
		if isCredentialHold(c.Reason) {
			s := PausedReasonCredential
			return &s
		}
		if isRateLimited(c.Reason) {
			s := PausedReasonRateLimit
			return &s
		}
	}
	return nil
}

// isRateLimited matches a rate-limit Paused reason on the same case/separator-normalized token as
// isCredentialHold (§8 tier-2 wait), so an upstream casing change needs no console change.
func isRateLimited(reason string) bool {
	return normalizeReason(reason) == "ratelimited"
}

// normalizeReason lowercases a condition reason and drops separators + non-ASCII-alphanumerics so
// the vocabulary match is casing/separator-insensitive (the credentials.go discipline).
func normalizeReason(reason string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(reason)) {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'z' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// projectRunSummary projects one Run into a history row (8.11). Phase is coalesced to Pending when
// the reconciler has not yet observed the Run. StartedAt is the claim time; EndedAt is the latest
// condition transition once the Run is terminal; DurationSeconds is elapsed (or live-so-far).
func projectRunSummary(run *ksquadv1.Run) RunSummary {
	phase := string(run.Status.Phase)
	if phase == "" {
		phase = string(ksquadv1.RunPhasePending)
	}
	rs := RunSummary{
		ID:          run.Name,
		Phase:       phase,
		WorkItemRef: run.Spec.WorkItemRef,
	}
	if run.Status.Phase == ksquadv1.RunPhasePaused {
		rs.PausedReason = pausedReasonOf(run)
	}
	if run.Status.ClaimedAt != nil {
		t := run.Status.ClaimedAt.Time
		rs.StartedAt = &t
	}
	if isTerminal(run.Status.Phase) {
		if end := latestConditionTime(run); end != nil {
			rs.EndedAt = end
		}
	}
	if rs.StartedAt != nil {
		end := time.Now()
		if rs.EndedAt != nil {
			end = *rs.EndedAt
		}
		if secs := int64(end.Sub(*rs.StartedAt).Seconds()); secs >= 0 {
			rs.DurationSeconds = &secs
		}
	}
	return rs
}

// latestConditionTime returns the most recent status-condition transition time, the best-effort
// end instant of a terminal Run (the CRD status carries no explicit endedAt). Nil when the Run has
// no conditions.
func latestConditionTime(run *ksquadv1.Run) *time.Time {
	var latest time.Time
	for i := range run.Status.Conditions {
		if t := run.Status.Conditions[i].LastTransitionTime.Time; t.After(latest) {
			latest = t
		}
	}
	if latest.IsZero() {
		return nil
	}
	return &latest
}

// ── HTTP handlers ───────────────────────────────────────────────────────────
// All ride the §13 BFF authz choke point (mounted in routes), so the AuthorContext is already
// stamped; the projection is scoped to that context's Team and the {teamId} path is verified to
// match it (a mismatch is existence-hiding 404, so a caller cannot read another Team by UID).

// teamOrg is the handler behind GET /api/teams/{teamId}/org.
func (s *Server) teamOrg(reader OrgReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		teamUID, ok := teamScope(w, r)
		if !ok {
			return
		}
		org, err := reader.Org(r.Context(), teamUID)
		if errors.Is(err, ErrTeamNotFound) {
			writeJSONError(w, http.StatusNotFound, "no org for this team")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, "org read model unavailable")
			return
		}
		writeJSON(w, http.StatusOK, org)
	}
}

// agentDetail is the handler behind GET /api/agents/{agentId}.
func (s *Server) agentDetail(reader OrgReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := authScope(w, r)
		if !ok {
			return
		}
		agentUID := muxVar(r, "agentId")
		agent, err := reader.Agent(r.Context(), auth, agentUID)
		if errors.Is(err, ErrTeamNotFound) || errors.Is(err, ErrAgentNotFound) {
			writeJSONError(w, http.StatusNotFound, "no such agent")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, "agent read model unavailable")
			return
		}
		writeJSON(w, http.StatusOK, agent)
	}
}

// agentRuns is the handler behind GET /api/agents/{agentId}/runs?limit=&offset=.
func (s *Server) agentRuns(reader OrgReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := authScope(w, r)
		if !ok {
			return
		}
		agentUID := muxVar(r, "agentId")
		limit := queryIntDefault(r, "limit", 50, 200)
		offset := queryIntDefault(r, "offset", 0, 0)
		runs, err := reader.AgentRuns(r.Context(), auth, agentUID, limit, offset)
		if errors.Is(err, ErrTeamNotFound) || errors.Is(err, ErrAgentNotFound) {
			writeJSONError(w, http.StatusNotFound, "no such agent")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, "agent-runs read model unavailable")
			return
		}
		writeJSON(w, http.StatusOK, runs)
	}
}

// orgStatusPollInterval is how often the SSE status stream re-polls the cache for per-agent status
// changes. The informer cache is in-memory, so a poll is cheap; a var (not const) so tests can
// shorten it. The stream is genuinely live within one interval without a per-Run watch.
var orgStatusPollInterval = 2 * time.Second

// teamStatusStream is the handler behind GET /api/teams/{teamId}/status/stream. It emits the
// current per-agent status as SSE frames on connect (so the diagram's overlay reflects truth even
// before the first change), then re-polls the cache and emits a frame only for an Agent whose
// status actually changed. Read-only: no mutate/claim/kill affordance rides the stream (R6). The
// projection is self-healing on reconnect (a fresh connect re-sends the full snapshot), so no
// Last-Event-ID replay is needed.
func (s *Server) teamStatusStream(reader OrgReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		teamUID, ok := teamScope(w, r)
		if !ok {
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no") // defeat nginx/proxy response buffering
		w.WriteHeader(http.StatusOK)

		rc := http.NewResponseController(w)
		if err := rc.Flush(); err != nil {
			return // streaming unsupported by this writer
		}

		ctx := r.Context()
		ticker := time.NewTicker(orgStatusPollInterval)
		defer ticker.Stop()

		// sent tracks the last frame emitted per agent so a re-poll only writes real changes.
		sent := map[string]string{}
		poll := func() bool {
			deltas, err := reader.AgentStatuses(ctx, teamUID)
			if err != nil {
				// Best-effort: a transient read error must not tear the stream down; keep the
				// connection open and try again next tick.
				return true
			}
			changed := false
			for _, d := range deltas {
				line := statusFrame(d)
				if sent[d.AgentID] == line {
					continue
				}
				sent[d.AgentID] = line
				if _, err := w.Write([]byte("data: " + line + "\n\n")); err != nil {
					return false
				}
				changed = true
			}
			if !changed {
				if _, err := w.Write([]byte(": ping\n\n")); err != nil {
					return false
				}
			}
			return rc.Flush() == nil
		}

		if !poll() {
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !poll() {
					return
				}
			}
		}
	}
}

// statusFrame renders one status delta as the compact JSON line the client folds. Built by hand
// (no encoding/json) so the SSE frame is single-line by construction — a value can carry no CR/LF
// (status is a fixed vocabulary, the reason is a fixed vocabulary, and the ids are K8s UIDs / names
// from the charset [A-Za-z0-9._-]), so no frame injection is possible.
func statusFrame(d AgentStatusDelta) string {
	var b strings.Builder
	b.WriteString(`{"agentId":`)
	b.WriteString(strconv.Quote(d.AgentID))
	b.WriteString(`,"status":`)
	b.WriteString(strconv.Quote(d.Status))
	if d.PausedReason != nil {
		b.WriteString(`,"pausedReason":`)
		b.WriteString(strconv.Quote(*d.PausedReason))
	}
	if d.CurrentRunID != nil {
		b.WriteString(`,"currentRunId":`)
		b.WriteString(strconv.Quote(*d.CurrentRunID))
	}
	b.WriteString("}")
	return b.String()
}

// teamScope resolves the caller's Team scope and verifies the {teamId} path matches it. It returns
// (teamUID, true) on success; on failure it has already written the response (401 unauthenticated,
// 404 existence-hiding for a foreign/absent team) and returns false.
func teamScope(w http.ResponseWriter, r *http.Request) (string, bool) {
	teamUID, ok := authScope(w, r)
	if !ok {
		return "", false
	}
	if path := muxVar(r, "teamId"); path != "" && path != teamUID {
		// A caller may only view their own Team; a foreign UID is hidden as missing (never 403).
		writeJSONError(w, http.StatusNotFound, "no org for this team")
		return "", false
	}
	return teamUID, true
}

// authScope returns the caller's server-derived Team UID. Defence in depth: BFFAuthz already
// guarantees an authenticated principal, but tenant data is never served without a resolved scope.
func authScope(w http.ResponseWriter, r *http.Request) (string, bool) {
	auth, ok := discussion.AuthFromContext(r.Context())
	if !ok || auth.Principal == "" {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return "", false
	}
	return auth.TeamID.String(), true
}

// queryIntDefault reads a non-negative integer query param, clamped to [0, max] when max > 0, with
// def when absent or malformed.
func queryIntDefault(r *http.Request, key string, def, max int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	if max > 0 && n > max {
		return max
	}
	return n
}
