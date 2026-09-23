/*
Copyright 2026 The K8squad Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// intake.go — M1.3 ticket intake (ISI-4129): the bridge from the BOARD to the
// Run plane. The board (a projection of coord.work_item, §13) had a write path
// (create lands in backlog; a human moves a card to todo, ISI-2909/ISI-3959)
// and the drive loop could execute a Run — but NOTHING created the Run CR for
// a board ticket, so a todo item never reached an agent. This sweeper is that
// missing link:
//
//	todo work item (team-assigned) → Run CR in the squad namespace
//	                                → driver claims (state → in_progress, §6.2)
//	                                → dispatch/sandbox machinery delivers it
//
// The same bounded background-sweep shape as the 3.3 kill sweep (cancel.go):
// level-triggered off the durable board state, so a missed tick costs delay,
// never correctness. Bounded intake latency is the M1.3 acceptance criterion
// ("a ticket created on the board reaches the agent and is claimed within a
// bounded time"): one tick (IntakeInterval) bounds board→Run, and the drive
// loop's Run watch bounds Run→claim.
//
// FR-B3 discipline (folds ISI-2524/ISI-2526 intent — no direct human-agent
// chat): the intake carries ONLY record identifiers (work item id, team,
// project, agent). No work content rides this path — the dispatcher reads
// title/body from the coordination record at dispatch time (dispatch.go), so
// the shared record stays the single channel, structurally.
//
// Idempotency is structural, not advisory:
//   - a deterministic Run name (intake-<work-item-id>) makes the create
//     itself idempotent — a racing tick converges on AlreadyExists;
//   - an existing Run for the workItemRef (any name: intake-created, human-
//     authored, coordinator-dispatched) suppresses intake entirely — intake
//     never stacks a second Run on a ticket the Run plane already owns.
//
// Honest degraded states, loudly logged, never silently skipped-and-forgotten:
// a team that has not reconciled its squad namespace yet, a Team/Project/Agent
// reference that does not resolve, or a Team with an empty composition all
// leave the item in todo for the next tick — self-healing once the world
// catches up, and visible in the operator log meanwhile.
// The intake sweep's Run create needs the ksquad.io/runs create grant
// (ISI-4132: the chart ClusterRole carried only read verbs, so intake dispatch
// 403'd on a fresh install — the marker keeps controller-gen + the chart
// lockstep honest).
// +kubebuilder:rbac:groups=ksquad.io,resources=runs,verbs=create
package rundrive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// IntakeInterval is the intake sweep's tick: the bounded-time half of the M1.3
// acceptance criterion. Ten seconds keeps board→dispatch comfortably inside
// human patience for a board action while the query itself is a cheap indexed
// scan (idx_work_item_state on (project_id, state)).
const IntakeInterval = 10 * time.Second

// DefaultIntakeMaxPerPass bounds how many Runs one tick may create, so a
// backlog-sized todo lane drains progressively instead of stampeding the
// warm pool in a single sweep.
const DefaultIntakeMaxPerPass = 32

// IntakePrincipal is the OwnedBy principal stamped on intake-created Runs —
// the ownership signal for permission checks (story 1.6), honestly attributing
// the operator's intake decision rather than defaulting it away.
const IntakePrincipal = "ksquad-intake"

// IntakeItem is one due board ticket: identifiers only (the FR-B3 rule above).
type IntakeItem struct {
	ID        string // coord.work_item.id (uuid)
	TeamID    string // coord.work_item.team_id (Team CR uid)
	ProjectID string // coord.work_item.project_id (Project CR uid or name)
	// RequestedAgent is the human's pre-run agent choice (mig 0021, ADR-0022),
	// "" when none. buildRun prefers it over Team.Spec.Agents[0], validating
	// membership defensively — a stale choice (composition changed since dispatch)
	// falls back rather than dispatching to an agent no longer on the team.
	RequestedAgent string
}

// IntakeSource is the board read-side seam, minimal so tests bind a fake (the
// prod binding is sqlIntakeSource over the coordination Postgres).
type IntakeSource interface {
	// DueWorkItems lists team-assigned todo work items, oldest first, at most
	// limit rows.
	DueWorkItems(ctx context.Context, limit int) ([]IntakeItem, error)
	// RearmSettled re-arms a SETTLED claim for the next generation (ISI-4556):
	// when the work item sits on the dispatch lane ('todo') and its coord.claim
	// row is terminal (succeeded/failed from a PREVIOUS generation's Run) with
	// the checkout released (holder NULL), the durable reconcile_step is reset
	// to 'pending' in the same discipline as a §6.3 re-entry — one transaction
	// co-committing the guarded step reset + fence bump, its §6.5 audit row and
	// §6.6 outbox event. Items that never ran (step already 'pending') or that
	// are mid-flight match the guard zero times: a pure no-op, never a clobber
	// of a live Run's step.
	RearmSettled(ctx context.Context, workItemID string) error
}

// sqlIntakeSource binds IntakeSource to the coord schema. Only rows that can
// legally dispatch are selected: state='todo' (the human's dispatch signal —
// backlog is not dispatched, in_progress/in_review/done are past intake) and
// team_id NOT NULL (a team-less item has no squad to dispatch to).
type sqlIntakeSource struct{ db *sql.DB }

// NewSQLIntakeSource binds the intake read-side over db.
func NewSQLIntakeSource(db *sql.DB) (IntakeSource, error) {
	if db == nil {
		return nil, errors.New("rundrive.NewSQLIntakeSource: nil db")
	}
	return sqlIntakeSource{db: db}, nil
}

// DueWorkItems implements IntakeSource.
func (s sqlIntakeSource) DueWorkItems(ctx context.Context, limit int) ([]IntakeItem, error) {
	if limit <= 0 {
		limit = DefaultIntakeMaxPerPass
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id::text, team_id::text, project_id::text, requested_agent
		  FROM coord.work_item
		 WHERE state = 'todo' AND team_id IS NOT NULL
		 ORDER BY created_at, id
		 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("rundrive.intake: list due work items: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var items []IntakeItem
	for rows.Next() {
		var it IntakeItem
		var requestedAgent sql.NullString
		if err := rows.Scan(&it.ID, &it.TeamID, &it.ProjectID, &requestedAgent); err != nil {
			return nil, fmt.Errorf("rundrive.intake: scan due work item: %w", err)
		}
		it.RequestedAgent = requestedAgent.String // "" when NULL
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rundrive.intake: due work items cursor: %w", err)
	}
	return items, nil
}

// RearmSettled implements IntakeSource.RearmSettled: the ISI-4556 generation
// re-arm. A settled ticket (terminal reconcile_step from a previous
// generation's Run, checkout released by the §6.3 terminal effect) that
// re-enters 'todo' carries a claim row the §6.2 acquire alone cannot revive —
// the drive loop treats a terminal durable step as absorbing (AC5), so the
// freshly minted next-generation Run would absorb within seconds (the
// projector mirroring 'Succeeded' onto it), the item would STAY on 'todo',
// and the next intake tick would mint yet another generation: the observed
// runaway mint loop. Re-arming the durable step to 'pending' BEFORE the mint
// closes that loop at its source: the new Run reads a dispatchable claim,
// acquires and drives exactly like a first-generation ticket, and its
// non-terminal phase suppresses further mints.
//
// One transaction, the §6.3 re-entry discipline (mirroring ProdClaims.enter):
//
//	UPDATE coord.claim SET reconcile_step='pending', fence_token+1 …
//	  WHERE work_item_id = :item
//	    AND holder_principal IS NULL                  (checkout released)
//	    AND reconcile_step IN ('succeeded','failed','cancelled')
//	    AND the work item still sits on 'todo'        (the dispatch signal)
//	INSERT INTO coord.audit_log … 'reconcile_rearmed' (§6.5 provenance)
//	INSERT INTO coord.outbox … 'reconcile_rearmed'    (§6.6 projection)
//
// The guard makes it self-policing and idempotent: a never-run item (step
// 'pending'), a live or mid-flight claim, or an item that left 'todo' matches
// zero rows and the whole transaction is an audited no-op — a racing lane
// move can never resurrect a claim a live Run holds, and a second call after
// a committed re-arm matches nothing. The fence bump keeps any same-fence
// zombie of the settled generation fenced out of the re-armed epoch.
func (s sqlIntakeSource) RearmSettled(ctx context.Context, workItemID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("rundrive.intake.RearmSettled: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	var fenceAfter int64
	q := fmt.Sprintf(`
		UPDATE coord.claim
		   SET reconcile_step    = 'pending',
		       fence_token       = fence_token + 1,
		       reclaim_fenced_at = clock_timestamp()
		 WHERE work_item_id = $1::uuid
		   AND holder_principal IS NULL
		   AND reconcile_step IN %s
		   AND EXISTS (
		         SELECT 1 FROM coord.work_item wi
		          WHERE wi.id = coord.claim.work_item_id
		            AND wi.state = 'todo')
		 RETURNING fence_token`, terminalSet)
	switch err := tx.QueryRowContext(ctx, q, workItemID).Scan(&fenceAfter); {
	case errors.Is(err, sql.ErrNoRows):
		// Not a settled-on-todo shape (never ran, mid-flight, held, or the
		// lane moved on): nothing to re-arm. Commit the empty transaction so
		// success and audited no-op answer identically.
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("rundrive.intake.RearmSettled: commit (no-op): %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("rundrive.intake.RearmSettled: re-arm: %w", err)
	}

	// §6.5 provenance, same transaction (the enter shape: to_state carries the
	// re-armed step; run_id stays NULL — no Run drives this transition yet).
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coord.audit_log
		       (work_item_id, run_id, event_type, principal, fence_token, to_state)
		VALUES ($1::uuid, NULL, 'reconcile_rearmed', $2, $3, 'pending')`,
		workItemID, OperatorPrincipal, fenceAfter); err != nil {
		return fmt.Errorf("rundrive.intake.RearmSettled: audit: %w", err)
	}
	// §6.6 exactly-once projection, same transaction (the enter outbox shape).
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coord.outbox
		       (entity, project_id, squad, event_type, work_item_id, run_id, payload)
		SELECT 'run', wi.project_id, wi.team_id::text, 'reconcile_rearmed',
		       wi.id, NULL,
		       jsonb_build_object('to_step', 'pending'::text, 'fence_token', $2::bigint)
		  FROM coord.work_item wi WHERE wi.id = $1::uuid`,
		workItemID, fenceAfter); err != nil {
		return fmt.Errorf("rundrive.intake.RearmSettled: outbox: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rundrive.intake.RearmSettled: commit: %w", err)
	}
	return nil
}

// Intake is a manager Runnable: every tick, dispatch due board tickets to the
// Run plane by creating their Run CRs.
type Intake struct {
	// Source reads the board (required; prod binding over the coord DB).
	Source IntakeSource
	// Client reads Teams/Projects/Runs and creates the Run CRs (the manager's
	// client; required).
	Client client.Client
	// Tick overrides IntakeInterval when > 0 (tests shrink it).
	Tick time.Duration
	// MaxPerPass bounds Run creates per tick (<= 0 → DefaultIntakeMaxPerPass).
	MaxPerPass int
	// Log receives diagnostics (nil discards).
	Log func(format string, args ...any)
}

// Start runs the sweep until ctx is done. Errors are logged and retried next
// tick — a transient DB or API-server stall must never kill the sweeper.
func (i *Intake) Start(ctx context.Context) error {
	tick := i.Tick
	if tick <= 0 {
		tick = IntakeInterval
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			i.sweep(ctx)
		}
	}
}

// sweep is one level-triggered pass: read the due board, resolve each ticket's
// squad graph, create the missing Run CRs.
func (i *Intake) sweep(ctx context.Context) {
	limit := i.MaxPerPass
	if limit <= 0 {
		limit = DefaultIntakeMaxPerPass
	}
	items, err := i.Source.DueWorkItems(ctx, limit)
	if err != nil {
		i.logf("rundrive.intake: %v", err)
		return
	}
	if len(items) == 0 {
		return
	}

	// Resolve the Team index once per pass: uid → Team (the board's team_id
	// is the Team CR's uid, the same linkage resolveTeamNamespace pins).
	var teams api.TeamList
	if err := i.Client.List(ctx, &teams); err != nil {
		i.logf("rundrive.intake: list teams: %v", err)
		return
	}
	teamByUID := make(map[string]api.Team, len(teams.Items))
	for idx := range teams.Items {
		teamByUID[string(teams.Items[idx].UID)] = teams.Items[idx]
	}

	// Resolve the existing Run index once per pass: workItemRef → live-run flag
	// + minted generation count. A NON-TERMINAL Run owning the ticket —
	// intake-authored or not — suppresses intake (the drive loop owns everything
	// from here). A TERMINAL Run (Succeeded/Failed/Cancelled) no longer does:
	// the ticket re-entered 'todo' (a human lane move, or the ISI-4495
	// comment-triggered re-dispatch — a settled/succeeded run parks the item
	// with the checkout RELEASED, settle.go), which is the board explicitly
	// asking for the NEXT Run. Intake first re-arms the settled claim
	// (RearmSettled, ISI-4556) and then mints a fresh, deterministically-
	// suffixed Run CR over the re-armed 'pending' step; without the re-arm the
	// new Run would absorb on the terminal durable step (AC5), the projector
	// would mirror 'Succeeded' onto it, and this sweep would mint the next
	// generation every tick — the runaway mint loop this guard closes.
	var runs api.RunList
	if err := i.Client.List(ctx, &runs); err != nil {
		i.logf("rundrive.intake: list runs: %v", err)
		return
	}
	liveRun := make(map[string]bool, len(runs.Items))
	generation := make(map[string]int, len(runs.Items))
	for idx := range runs.Items {
		r := runs.Items[idx]
		generation[r.Spec.WorkItemRef]++
		if !runPhaseTerminal(r.Status.Phase) {
			liveRun[r.Spec.WorkItemRef] = true
		}
	}

	created := 0
	dispatched := 0
	for _, item := range items {
		if dispatched >= limit {
			break // a source that ignored its LIMIT must not stampede the pass
		}
		dispatched++
		if liveRun[item.ID] {
			continue
		}
		// ISI-4556: re-arm a settled claim BEFORE minting over it. The order is
		// load-bearing: the minted Run must be born into a world whose durable
		// step is already 'pending', or the projector (which keys on the work
		// item's claim step) races the drive loop and stamps the newborn Run
		// 'Succeeded' — and a terminal newborn re-mints on the next tick. An
		// error here is honest-degraded: log, leave the item on 'todo', retry
		// next tick — never mint over an unre-armed terminal step.
		if err := i.Source.RearmSettled(ctx, item.ID); err != nil {
			i.logf("rundrive.intake: work item %s not re-armed: %v", item.ID, err)
			continue
		}
		run, err := i.buildRun(ctx, item, teamByUID, generation[item.ID])
		if err != nil {
			// Honest degraded: log and leave the item in todo for the next
			// tick — a not-yet-reconciled Team or a dangling reference is a
			// world state, not an intake failure.
			i.logf("rundrive.intake: work item %s not dispatched: %v", item.ID, err)
			continue
		}
		if err := i.Client.Create(ctx, run); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// A racing tick (or a same-named human Run) got there first:
				// the ticket is on the Run plane, which is all intake owes.
				created++
				continue
			}
			i.logf("rundrive.intake: create run for %s: %v", item.ID, err)
			continue
		}
		created++
		i.logf("rundrive.intake: dispatched work item %s to agent %s (run %s/%s)",
			item.ID, run.Spec.Agents[0].Name, run.Namespace, run.Name)
	}
	if created > 0 {
		i.logf("rundrive.intake: pass dispatched %d/%d due work item(s)", created, len(items))
	}
}

// buildRun resolves one ticket's squad graph and renders its Run CR. It errors
// (never panics, never half-builds) when the graph does not resolve, so sweep
// can log-and-retry the item. generation is the number of Run CRs this work
// item has already minted (0 on first mint): the deterministic name suffixes
// -r<generation+1> from the second mint on, so a re-entered 'todo' ticket (a
// human reopen, or the ISI-4495 comment re-dispatch) gets a FRESH Run CR beside
// the terminal one instead of colliding on the first-born name (intake-<id>).
func (i *Intake) buildRun(ctx context.Context, item IntakeItem, teamByUID map[string]api.Team, generation int) (*api.Run, error) {
	team, ok := teamByUID[item.TeamID]
	if !ok {
		return nil, fmt.Errorf("team uid %s resolves to no Team CR", item.TeamID)
	}
	// The write-model namespace discipline (mirrors the compose write path):
	// a Run is written into the RECONCILED squad namespace (Status.Namespace),
	// not wherever the Team CR happens to sit. An unreconciled Team has no
	// namespace to run in yet — honest skip, next tick retries.
	ns := team.Status.Namespace
	if ns == "" {
		return nil, fmt.Errorf("team %s/%s has no reconciled squad namespace yet", team.Namespace, team.Name)
	}

	projectRef, err := i.resolveProject(ctx, ns, item.ProjectID)
	if err != nil {
		return nil, err
	}

	// Agent selection (M1: one agent, one ticket). The default is the Team
	// composition's first entry; spec.agents empty would defer to a reconciler
	// default (story 1.3) that has not landed — selecting the composition here
	// keeps the dispatch concrete instead of relying on an unimplemented default.
	if len(team.Spec.Agents) == 0 {
		return nil, fmt.Errorf("team %s/%s composition names no agent to dispatch to", team.Namespace, team.Name)
	}
	agentRef := team.Spec.Agents[0]
	// The human's pre-run choice (mig 0021, ADR-0022 §3 D2) wins over the default
	// — but only if it is STILL a member of the composition. Validating membership
	// here (not just trusting the column) is the §7.4 "defensive" check: the
	// dispatch write already enforced agent-∈-Team, yet the composition can change
	// between dispatch and this tick, so a stale choice falls back to Agents[0]
	// rather than minting a Run for an agent no longer on the team.
	if item.RequestedAgent != "" {
		matched := false
		for _, a := range team.Spec.Agents {
			if a.Name == item.RequestedAgent {
				agentRef = a
				matched = true
				break
			}
		}
		if !matched {
			i.logf("rundrive.intake: work item %s requested agent %q no longer in team %s/%s composition; falling back to %s",
				item.ID, item.RequestedAgent, team.Namespace, team.Name, team.Spec.Agents[0].Name)
		}
	}
	// ISI-4820: resolve the Agent CR where a composition's agents actually
	// live. The Team.Spec.Agents entries carry an EMPTY namespace, and the old
	// fallback resolved them in the EXEC/run namespace (Status.Namespace) — but
	// composition Agent CRs are authored in the Team's HOME namespace
	// (Team.Namespace, e.g. bmad-squad), not the reconciled exec ns. Only an
	// agent accidentally mirrored into the exec ns (`sam`) resolved there, so
	// every other agent silently failed to mint a Run and the ticket looped on
	// 'todo' forever ("dispatch works only for sam; change status to force it").
	// Same exec-vs-home namespace-bridge class as ISI-4738 (runNamespaceForTeam).
	// Try, in order: an EXPLICIT ref namespace (honored as authored), then the
	// HOME ns (canonical composition home), then the EXEC ns (the mirrored case
	// — keeps `sam`, which exists in both, resolving).
	agentNS, err := i.resolveAgent(ctx, agentRef, team.Namespace, ns)
	if err != nil {
		return nil, err
	}

	return &api.Run{
		ObjectMeta: runObjectMeta(item.ID, ns, generation),
		Spec: api.RunSpec{
			// M1.2 (ISI-4128): like projectRef — a Team CR living outside the
			// squad namespace must be referenced by namespace or later
			// Team-resolution steps (context assembly, dispatch) miss it.
			TeamRef: func() api.ObjectRef {
				if team.Namespace != ns {
					return api.ObjectRef{Name: team.Name, Namespace: team.Namespace}
				}
				return api.ObjectRef{Name: team.Name}
			}(),
			ProjectRef:  *projectRef,
			WorkItemRef: item.ID,
			// ISI-4820: carry the namespace the Agent CR actually resolved in
			// when it is not the run's own ns — exactly the TeamRef/ProjectRef
			// cross-ns convention above. dispatch.go resolves the dispatch agent
			// with the SAME empty-ns→run.Namespace fallback, so without this the
			// run would mint but then fail agent resolution at dispatch for any
			// home-ns (non-mirrored) agent.
			Agents:  []api.ObjectRef{agentRefForRun(agentRef.Name, agentNS, ns)},
			OwnedBy: api.PrincipalRef(IntakePrincipal),
		},
	}, nil
}

// resolveAgent finds the composition's Agent CR, trying (in order) an explicit
// ref namespace, the Team's HOME namespace, then the EXEC/run namespace, and
// returns the namespace it resolved in. Empty and duplicate candidates are
// skipped so a home-less Team (homeNS == "") or homeNS == execNS degrades to a
// single exec-ns lookup. The error preserves every namespace tried, so an
// honest-degraded log names where it looked.
func (i *Intake) resolveAgent(ctx context.Context, agentRef api.ObjectRef, homeNS, execNS string) (string, error) {
	var candidates []string
	add := func(ns string) {
		if ns == "" {
			return
		}
		for _, c := range candidates {
			if c == ns {
				return
			}
		}
		candidates = append(candidates, ns)
	}
	add(agentRef.Namespace)
	add(homeNS)
	add(execNS)

	var lastErr error
	for _, ns := range candidates {
		var agent api.Agent
		if err := i.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: agentRef.Name}, &agent); err != nil {
			lastErr = err
			continue
		}
		return ns, nil
	}
	return "", fmt.Errorf("resolve agent %q in %v: %w", agentRef.Name, candidates, lastErr)
}

// agentRefForRun renders the Run's dispatch-agent ObjectRef: bare name when the
// Agent CR lives in the run's own ns (the existing convention, keeping specs
// clean), name+namespace when it lives elsewhere — so dispatch.go's cross-ns
// resolution finds the same CR intake validated.
func agentRefForRun(name, agentNS, runNS string) api.ObjectRef {
	if agentNS != "" && agentNS != runNS {
		return api.ObjectRef{Name: name, Namespace: agentNS}
	}
	return api.ObjectRef{Name: name}
}

// resolveProject mirrors the apiserver's fleet-wide resolution (UID-first,
// unique even across a bare-name collision; then a name match scoped to the
// squad namespace, exactly resolveProjectInTeam's boundary).
func (i *Intake) resolveProject(ctx context.Context, ns, projectID string) (*api.ObjectRef, error) {
	if projectID == "" {
		return nil, fmt.Errorf("work item carries no project id")
	}
	var projects api.ProjectList
	if err := i.Client.List(ctx, &projects); err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	byUID := ""
	for idx := range projects.Items {
		p := projects.Items[idx]
		if string(p.UID) == projectID {
			// M1.2 (ISI-4128): the run controller resolves projectRef in the
			// Run's namespace when the ref carries none — a Project CR living
			// outside the squad namespace must be referenced by namespace or
			// context assembly fails to find it.
			if p.Namespace != ns {
				return &api.ObjectRef{Name: p.Name, Namespace: p.Namespace}, nil
			}
			return &api.ObjectRef{Name: p.Name}, nil // UID match wins outright
		}
		if p.Namespace == ns && p.Name == projectID && byUID == "" {
			byUID = p.Name // squad-scoped name match, remembered
		}
	}
	if byUID != "" {
		return &api.ObjectRef{Name: byUID}, nil
	}
	return nil, fmt.Errorf("project %q resolves to no Project CR (uid-first, then name in %s)", projectID, ns)
}

// runObjectMeta renders the deterministic Run identity: name intake-<work
// item id> (a uuid is already DNS-1123-safe) for the first mint, then
// intake-<work item id>-r<N> for the Nth RE-mint (N = generation+1 ≥ 2), in the
// squad namespace. The deterministic name IS the create idempotency: two racing
// sweeps compute the same generation and converge on AlreadyExists instead of
// duplicate Runs.
func runObjectMeta(workItemID, ns string, generation int) metav1.ObjectMeta {
	name := "intake-" + workItemID
	if generation > 0 {
		name = fmt.Sprintf("intake-%s-r%d", workItemID, generation+1)
	}
	return metav1.ObjectMeta{
		Name:      name,
		Namespace: ns,
		Labels: map[string]string{
			"ksquad.io/created-by": "intake",
		},
	}
}

// runPhaseTerminal reports whether a Run phase is terminal (§8): Succeeded,
// Failed, Cancelled. A terminal Run no longer owns the ticket — the checkout
// was released — so a ticket re-entering 'todo' may mint the next generation.
// An unset phase (not yet reconciled) is NOT terminal: a just-created Run
// suppresses intake exactly like a live one (fail-closed against double-mint).
func runPhaseTerminal(phase api.RunPhase) bool {
	switch phase {
	case api.RunPhaseSucceeded, api.RunPhaseFailed, api.RunPhaseCancelled:
		return true
	default:
		return false
	}
}

func (i *Intake) logf(format string, args ...any) {
	if i.Log != nil {
		i.Log(format, args...)
	}
}
