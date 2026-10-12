package apiserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// ============================================================================
// Agent-activity read model (ISI-5704 / WS-1a, parent plan ISI-5697 rev2,
// approved design ISI-5699 mock B).
// ============================================================================
//
// GET /api/squad/agents/activity is the server-side aggregate that replaces the
// console overview's N+1 fan-out: the pre-redesign FleetOverview read the roster
// from GET /api/squad/agents and then issued one GET /api/runs/{runId} per live
// run, deriving each card's last-thinking line CLIENT-side (latestSnippet). This
// endpoint folds that into ONE projection — per agent its current-or-last run and
// the SERVER-classified last-thinking snippet — so the console renders the
// Paperclip-style agent cards without the per-run round trip.
//
// Three disciplines carry over from the design and its lineage:
//   - lastThinkingSnippet is the CLASSIFIED agent narration (port of
//     console/lib/run-activity.ts + lib/discussion/activityView.ts), NEVER the raw
//     [run …]/[tool:…]/[status]/[server:…] wire stream (ISI-5695/5696). A raw
//     tool/lifecycle line is demoted — it counts toward lastActivityAt (freshness)
//     but is never surfaced as "what the agent is thinking".
//   - fabrication discipline (FR-I3): an unknown source is left empty (the FE
//     renders "—"/"waiting for first step…"), never a placeholder; blocked is only
//     a Paused/held run (an authoritative reconciler signal), never synthesized.
//   - scope mirrors the other /api/squad/* read models: admin ⇒ fleet-wide (every
//     squad's agents/runs), tenant ⇒ fenced to their Team (home ns agents, exec ns
//     runs — the ISI-4737/ISI-4565 split).

// AgentActivityList is the GET /api/squad/agents/activity payload.
type AgentActivityList struct {
	Agents []AgentActivity `json:"agents"`
	Counts ActivityCounts  `json:"counts"`
	// Fleet is set only for a global-admin caller (every squad's agents), mirroring
	// the other fleet-aware list projections so the console can badge the view.
	Fleet bool `json:"fleet,omitempty"`
}

// ActivityCounts is the fleet/Team rollup of agent states (Henrik's "global stats
// first" ask). running/idle/blocked partition the agents exactly: every agent is
// counted in exactly one bucket, so running+idle+blocked == len(Agents).
type ActivityCounts struct {
	Running int `json:"running"`
	Idle    int `json:"idle"`
	Blocked int `json:"blocked"`
}

// AgentActivity is one agent row: identity + its current-or-last run. The state is
// the aggregate bucket the card chips off (running | idle | blocked).
type AgentActivity struct {
	AgentID string `json:"agentId"`
	Name    string `json:"name"`
	Role    string `json:"role,omitempty"`
	// State is the agent's live bucket: running (a non-terminal, non-paused run),
	// blocked (a Paused/held run — the only authoritative not-progressing signal),
	// or idle (no active run). Matches ActivityCounts' partition.
	State string `json:"state"`
	// CurrentOrLastRun is the agent's freshest non-terminal run, or — when none is
	// active — its most recent terminal run, so an idle card still shows what the
	// agent last worked on. Nil only when the agent has never run (the FE renders
	// the honest "No run yet" empty state, FR-I3).
	CurrentOrLastRun *ActivityRun `json:"currentOrLastRun"`
}

// ActivityRun is the per-agent run projection the AgentRunCard renders. Identity
// fields are always present; the enrichment fields degrade to empty/nil when their
// source is unavailable rather than being fabricated (FR-I3).
type ActivityRun struct {
	RunID       string `json:"runId"`
	Phase       string `json:"phase"`
	WorkItemRef string `json:"workItemRef,omitempty"`
	// WorkItemTitle is the human-readable work-item title (coord.work_item), the
	// card's chip label — far friendlier than the opaque WorkItemRef uuid. Empty
	// when the DB is unwired or the item is not found (the card falls back to the
	// ref / a generic label).
	WorkItemTitle string `json:"workItemTitle,omitempty"`
	// WorkItemURL is the IN-CONSOLE deep-link to the ticket the agent worked on
	// (/projects/{id}/issues/{ref}), set for a board-sourced work item. Mutually
	// exclusive with DiscussionURL: a discussion thread-run deep-links to the room,
	// not to a /issues path that would 404.
	WorkItemURL string `json:"workItemUrl,omitempty"`
	// DiscussionURL is the in-console deep-link to the discussion room
	// (/projects/{id}/discussion) for a discussion-sourced run (party/thread work),
	// the fallback chip when there is no board ticket.
	DiscussionURL string `json:"discussionUrl,omitempty"`
	// LastThinkingSnippet is the newest CLASSIFIED agent narration line (prefixes
	// stripped, whitespace collapsed), never the raw [run]/[tool] stream. Empty when
	// the run has reported no narration yet (the FE renders "waiting for first
	// step…").
	LastThinkingSnippet string `json:"lastThinkingSnippet,omitempty"`
	// LastActivityAt is the instant of the agent's most recent activity of ANY kind
	// (thinking, tool call, or lifecycle), for the card's relative-time line. Nil
	// when the run carries no timestamped activity.
	LastActivityAt *time.Time `json:"lastActivityAt,omitempty"`
}

// AgentActivityReader projects the agent-activity aggregate for the caller's scope.
// admin ⇒ fleet-wide; non-admin ⇒ fenced to teamUID's Team (existence-hiding 404
// via ErrTeamNotFound for an unknown scope). Production wires the cache+DB-backed
// reader; tests wire a fake.
type AgentActivityReader interface {
	AgentActivity(ctx context.Context, teamUID string, admin bool) (AgentActivityList, error)
}

// ClientAgentActivityReader is the production AgentActivityReader. Agents/Runs come
// from the informer cache (client.Reader); the snippet narration + work-item
// title/source come from coord (*sql.DB). A nil db degrades gracefully: the snippet
// then derives from Run.Status.LLMInteractions alone and work items are assumed
// board-sourced (the common case) with no title.
type ClientAgentActivityReader struct {
	reader client.Reader
	db     *sql.DB
}

// NewClientAgentActivityReader builds the agent-activity read model over the
// informer cache and the coord store. db may be nil (DB-less dev host).
func NewClientAgentActivityReader(r client.Reader, db *sql.DB) *ClientAgentActivityReader {
	return &ClientAgentActivityReader{reader: r, db: db}
}

// AgentActivity lists the agents in scope with their current-or-last run. For an
// admin it spans every squad (agents + runs cluster-wide, runs normalized to their
// home namespace via the Team exec→home map); for a tenant it fences to the Team's
// home namespace (agents) and execution namespace (runs) — the ISI-4737/ISI-4565
// split the org/overview readers already honor.
func (r *ClientAgentActivityReader) AgentActivity(ctx context.Context, teamUID string, admin bool) (AgentActivityList, error) {
	var agents ksquadv1.AgentList
	var runs ksquadv1.RunList
	// execToHome maps a run's (execution) namespace to the project/home namespace so
	// an in-console project id can be built; for the tenant path there is a single
	// home namespace and the map is unused.
	var execToHome map[string]string
	homeNS := ""

	if admin {
		if err := r.reader.List(ctx, &agents); err != nil {
			return AgentActivityList{}, err
		}
		if err := r.reader.List(ctx, &runs); err != nil {
			return AgentActivityList{}, err
		}
		var teams ksquadv1.TeamList
		if err := r.reader.List(ctx, &teams); err != nil {
			return AgentActivityList{}, err
		}
		execToHome = execToHomeNamespace(teams.Items)
	} else {
		if teamUID == "" {
			return AgentActivityList{}, ErrTeamNotFound
		}
		var teams ksquadv1.TeamList
		if err := r.reader.List(ctx, &teams); err != nil {
			return AgentActivityList{}, err
		}
		var team *ksquadv1.Team
		for i := range teams.Items {
			if string(teams.Items[i].UID) == teamUID {
				team = &teams.Items[i]
				break
			}
		}
		if team == nil {
			return AgentActivityList{}, ErrTeamNotFound
		}
		homeNS = team.Namespace
		runNS := team.Status.Namespace
		if runNS == "" {
			runNS = homeNS
		}
		if err := r.reader.List(ctx, &agents, client.InNamespace(homeNS)); err != nil {
			return AgentActivityList{}, err
		}
		if err := r.reader.List(ctx, &runs, client.InNamespace(runNS)); err != nil {
			return AgentActivityList{}, err
		}
	}

	// Attribute runs to agents by the Agent NAME they select (same rule as the org
	// read model: an empty spec.agents is a reconciler-defaulted run that applies to
	// every agent in the squad). Admin ignores namespace on the ref (fleet-wide);
	// the tenant path fences to the home namespace.
	names := agentNames(agents.Items)
	var runsByAgent map[string][]*ksquadv1.Run
	if admin {
		runsByAgent = indexRunsByAgentFleet(runs.Items, names)
	} else {
		runsByAgent = indexRunsByAgent(runs.Items, homeNS, names)
	}

	// Pass 1: pick each agent's current-or-last run + state, collecting the distinct
	// selected runs so their enrichment (snippet, title, source) is computed once.
	type pick struct {
		run   *ksquadv1.Run
		state string
	}
	picks := make([]pick, len(agents.Items))
	selected := map[string]*ksquadv1.Run{} // run name → run (distinct)
	counts := ActivityCounts{}
	for i := range agents.Items {
		run, state := pickCurrentOrLast(runsByAgent[agents.Items[i].Name])
		picks[i] = pick{run: run, state: state}
		switch state {
		case AgentStatusRunning:
			counts.Running++
		case AgentStatusBlocked:
			counts.Blocked++
		default:
			counts.Idle++
		}
		if run != nil {
			selected[run.Name] = run
		}
	}

	// Pass 2: enrich the distinct selected runs. Snippet classification needs the
	// run-scoped coord.comment narration (best-effort DB read, memoized per run);
	// work-item title/source is one batched read over every selected ref.
	enriched := r.enrichRuns(ctx, selected, execToHome, homeNS)

	out := AgentActivityList{Agents: make([]AgentActivity, 0, len(agents.Items)), Counts: counts, Fleet: admin}
	for i := range agents.Items {
		a := &agents.Items[i]
		row := AgentActivity{
			AgentID: string(a.UID),
			Name:    a.Name,
			Role:    a.Spec.RoleRef.Name,
			State:   picks[i].state,
		}
		if run := picks[i].run; run != nil {
			if ar, ok := enriched[run.Name]; ok {
				row.CurrentOrLastRun = ar
			}
		}
		out.Agents = append(out.Agents, row)
	}
	sort.Slice(out.Agents, func(a, b int) bool {
		if out.Agents[a].Name != out.Agents[b].Name {
			return out.Agents[a].Name < out.Agents[b].Name
		}
		return out.Agents[a].AgentID < out.Agents[b].AgentID
	})
	return out, nil
}

// pickCurrentOrLast chooses the agent's current (freshest non-terminal) run and its
// aggregate state; with no active run it falls back to the most recent terminal run
// (so an idle card still shows the last thing the agent worked on) and reports idle.
// A Paused current run is blocked (held on a credential/rate-limit/human wait — the
// one authoritative not-progressing signal); any other active phase is running.
func pickCurrentOrLast(runs []*ksquadv1.Run) (*ksquadv1.Run, string) {
	var current, lastTerminal *ksquadv1.Run
	for _, run := range runs {
		if isTerminal(run.Status.Phase) {
			if lastTerminal == nil || runStart(run).After(runStart(lastTerminal)) ||
				(runStart(run).Equal(runStart(lastTerminal)) && run.Name > lastTerminal.Name) {
				lastTerminal = run
			}
			continue
		}
		// Freshest non-terminal run wins; deterministic tie-break by name (the
		// deriveStatus discipline — stable across cache reads).
		if current == nil || runStart(run).After(runStart(current)) ||
			(runStart(run).Equal(runStart(current)) && run.Name > current.Name) {
			current = run
		}
	}
	if current != nil {
		if current.Status.Phase == ksquadv1.RunPhasePaused {
			return current, AgentStatusBlocked
		}
		return current, AgentStatusRunning
	}
	return lastTerminal, AgentStatusIdle // lastTerminal may be nil (never ran)
}

// enrichRuns builds the ActivityRun projection for each distinct selected run:
// identity + deep-link URLs + the classified last-thinking snippet + lastActivityAt.
// The work-item title/source come from ONE batched coord.work_item read; the
// snippet's narration comes from a per-run coord.comment read (bounded by the
// number of agents, not runs, and replacing the old per-run client fan-out).
func (r *ClientAgentActivityReader) enrichRuns(
	ctx context.Context,
	selected map[string]*ksquadv1.Run,
	execToHome map[string]string,
	tenantHomeNS string,
) map[string]*ActivityRun {
	out := make(map[string]*ActivityRun, len(selected))

	// Batched work-item title/source for every selected ref (board vs discussion
	// decides which deep-link the card gets).
	refs := make([]string, 0, len(selected))
	for _, run := range selected {
		if run.Spec.WorkItemRef != "" {
			refs = append(refs, run.Spec.WorkItemRef)
		}
	}
	titleByRef, sourceByRef := r.workItemMeta(ctx, refs)

	for name, run := range selected {
		phase := string(run.Status.Phase)
		if phase == "" {
			phase = string(ksquadv1.RunPhasePending)
		}
		ar := &ActivityRun{
			RunID:       run.Name,
			Phase:       phase,
			WorkItemRef: run.Spec.WorkItemRef,
		}

		// Home namespace for the in-console project id: the tenant path knows it
		// directly; the admin path maps the run's execution namespace back to home.
		home := tenantHomeNS
		if home == "" {
			home = run.Namespace
			if h, ok := execToHome[run.Namespace]; ok {
				home = h
			}
		}
		if run.Spec.WorkItemRef != "" && run.Spec.ProjectRef.Name != "" {
			ar.WorkItemTitle = titleByRef[run.Spec.WorkItemRef]
			projectID := inConsoleProjectID(home, run.Spec.ProjectRef.Name)
			// Discussion-sourced work deep-links to the room; everything else (and the
			// DB-less default) to the ticket. source is "" when unknown → treat as board.
			if sourceByRef[run.Spec.WorkItemRef] == "discussion" {
				ar.DiscussionURL = "/projects/" + projectID + "/discussion"
			} else {
				ar.WorkItemURL = "/projects/" + projectID + "/issues/" + url.PathEscape(run.Spec.WorkItemRef)
			}
		}

		snippet, lastAt := r.runSnippet(ctx, run)
		ar.LastThinkingSnippet = snippet
		ar.LastActivityAt = lastAt
		out[name] = ar
	}
	return out
}

// workItemMeta batches a title + source read for the selected refs. Best-effort:
// a nil db or a read error yields empty maps (the card falls back to the ref and
// the board deep-link). source is coord.work_item.source ('board' | 'discussion' |
// …) — the ADR-0024b board-hide discriminator.
func (r *ClientAgentActivityReader) workItemMeta(ctx context.Context, refs []string) (title, source map[string]string) {
	title, source = map[string]string{}, map[string]string{}
	if r.db == nil || len(refs) == 0 {
		return title, source
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id::text, COALESCE(title, ''), COALESCE(source, '')
		FROM coord.work_item
		WHERE id = ANY($1::uuid[])`, pq.Array(refs))
	if err != nil {
		return title, source
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, t, s string
		if err := rows.Scan(&id, &t, &s); err != nil {
			return title, source
		}
		title[id] = t
		source[id] = s
	}
	return title, source
}

// runSnippet derives the run's last CLASSIFIED thinking line and the instant of its
// most recent activity. Two sources feed it, both classified server-side the same
// way the run-detail screen classifies the wire stream:
//   - Run.Status.LLMInteractions (in the CRD, no DB): a "response" is the model's
//     own output — meaningful thinking; prompt/tool_call/tool_response are activity
//     but not thinking.
//   - run-scoped coord.comment narration (DB, best-effort): a `[run <token>]` row
//     whose body is plain narration is meaningful; a `[tool:…]/[status]/[server:…]`
//     row is raw stream — demoted (ISI-5695/5696).
//
// lastActivityAt is the max timestamp across ALL activity (freshness); the snippet
// is the newest MEANINGFUL line, cleaned (prefixes stripped, whitespace collapsed).
func (r *ClientAgentActivityReader) runSnippet(ctx context.Context, run *ksquadv1.Run) (string, *time.Time) {
	var bestSnippet string
	var bestSnippetTs time.Time
	var lastAt time.Time

	consider := func(ts time.Time, text string, meaningful bool) {
		if !ts.IsZero() && ts.After(lastAt) {
			lastAt = ts
		}
		if !meaningful {
			return
		}
		clean := collapseWhitespace(text)
		if clean == "" {
			return
		}
		// Newest meaningful line wins; a zero ts never displaces a real one.
		if bestSnippet == "" || ts.After(bestSnippetTs) {
			bestSnippet = clean
			bestSnippetTs = ts
		}
	}

	// Source 1: Run.Status.LLMInteractions.
	for i := range run.Status.LLMInteractions {
		in := &run.Status.LLMInteractions[i]
		ts := in.Timestamp.Time
		switch in.Type {
		case "response":
			consider(ts, string(in.Response), true)
		default: // prompt | tool_call | tool_response — activity, not thinking.
			consider(ts, "", false)
		}
	}

	// Source 2: run-scoped progressmirror narration (best-effort).
	if r.db != nil && run.Spec.WorkItemRef != "" {
		token := shortRunToken(string(run.GetUID()))
		mine := "[run %"
		if token != "" {
			mine = "[run " + token + "]%"
		}
		rows, err := r.db.QueryContext(ctx, `
			SELECT body, created_at
			FROM coord.comment
			WHERE work_item_id = $1::uuid
			  AND body LIKE $2
			  AND body NOT LIKE '[run %][llm:%'
			ORDER BY created_at DESC
			LIMIT 20`, run.Spec.WorkItemRef, mine)
		if err == nil {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var body string
				var ts time.Time
				if err := rows.Scan(&body, &ts); err != nil {
					break
				}
				consider(ts, cleanActivityBody(body), isNarration(body))
			}
		}
	}

	if lastAt.IsZero() {
		return bestSnippet, nil
	}
	t := lastAt
	return bestSnippet, &t
}

// inConsoleProjectID renders the console Project id ("namespace/name", ISI-5107)
// as the SINGLE percent-encoded path segment the /projects/[projectId] routes
// expect (ISI-3982). For a DNS-label namespace/name the only character needing
// encoding is the slash, so this matches the FE's encodeProjectId(encodeURIComponent)
// without pulling Next.js route logic into Go.
func inConsoleProjectID(namespace, name string) string {
	return strings.ReplaceAll(namespace+"/"+name, "/", "%2F")
}

// ── classification helpers (Go port of console/lib/run-activity.ts +
//    lib/discussion/activityView.ts) ─────────────────────────────────────────

var runScopePrefixRe = regexp.MustCompile(`^\s*\[run\s+[A-Za-z0-9._-]+\]\s*`)

// leadingExecMarkersRe matches a leading run of bracketed execution markers
// (`[tool:…]`, `[server:…]`, `[untrusted]`, `[status…]`) — the raw wire scaffolding
// activityView.ts strips. `[^\]]*` consumes a whole `[tool:bash/result(ok)]` marker.
var leadingExecMarkersRe = regexp.MustCompile(`(?i)^(?:\[(?:tool:[^\]]*|server:[^\]]*|untrusted|status[^\]]*)\]\s*)+`)

// cleanActivityBody strips the `[run <id>]` scope prefix and any leading execution
// markers off a narration body, returning readable progress text. When stripping
// would leave nothing, the run-scope-stripped remainder is returned so a line never
// collapses to empty (parity with activityView.ts#cleanActivityBody).
func cleanActivityBody(body string) string {
	rest := strings.TrimSpace(runScopePrefixRe.ReplaceAllString(body, ""))
	stripped := strings.TrimSpace(leadingExecMarkersRe.ReplaceAllString(rest, ""))
	if stripped == "" {
		return rest
	}
	return stripped
}

// isNarration reports whether a run-scoped coord.comment row is the agent's own
// narration (meaningful thinking) rather than a raw [tool:…]/[status]/[server:…]
// execution line. Only run-scoped rows (`[run …]` prefix) are the agent's stream;
// a ticket-level comment (no prefix) is conversation, not thinking.
func isNarration(body string) bool {
	if !runScopePrefixRe.MatchString(body) {
		return false
	}
	rest := strings.TrimSpace(runScopePrefixRe.ReplaceAllString(body, ""))
	return !leadingExecMarkersRe.MatchString(rest)
}

// collapseWhitespace trims and folds internal runs of whitespace (incl. newlines)
// to a single space so a multi-line digest renders as one clean snippet line.
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// squadAgentActivity is the handler behind GET /api/squad/agents/activity. It rides
// the SAME §13 BFF authz choke point and admin/tenant scoping as the other
// /api/squad/* read models: the AuthorContext is already stamped, nothing
// tenancy-relevant is read from the request, and a tenant whose Team scope resolves
// to no Team gets an existence-hiding 404 (distinct from the 401 an unauthenticated
// caller gets). GET-only.
func (s *Server) squadAgentActivity(reader AgentActivityReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, admin, ok := authScopeAdmin(w, r)
		if !ok {
			return
		}
		list, err := reader.AgentActivity(r.Context(), auth, admin)
		if errors.Is(err, ErrTeamNotFound) {
			writeJSONError(w, http.StatusNotFound, "no agents for this caller")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, "agent activity read model unavailable")
			return
		}
		writeJSON(w, http.StatusOK, list)
	}
}
