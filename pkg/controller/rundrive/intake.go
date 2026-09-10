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
}

// IntakeSource is the board read-side seam, minimal so tests bind a fake (the
// prod binding is sqlIntakeSource over the coordination Postgres).
type IntakeSource interface {
	// DueWorkItems lists team-assigned todo work items, oldest first, at most
	// limit rows.
	DueWorkItems(ctx context.Context, limit int) ([]IntakeItem, error)
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
		SELECT id::text, team_id::text, project_id::text
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
		if err := rows.Scan(&it.ID, &it.TeamID, &it.ProjectID); err != nil {
			return nil, fmt.Errorf("rundrive.intake: scan due work item: %w", err)
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rundrive.intake: due work items cursor: %w", err)
	}
	return items, nil
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

	// Resolve the existing Run index once per pass: workItemRef → exists.
	// Any Run already owning the ticket — intake-authored or not — suppresses
	// intake (the drive loop owns everything from here).
	var runs api.RunList
	if err := i.Client.List(ctx, &runs); err != nil {
		i.logf("rundrive.intake: list runs: %v", err)
		return
	}
	hasRun := make(map[string]bool, len(runs.Items))
	for idx := range runs.Items {
		hasRun[runs.Items[idx].Spec.WorkItemRef] = true
	}

	created := 0
	dispatched := 0
	for _, item := range items {
		if dispatched >= limit {
			break // a source that ignored its LIMIT must not stampede the pass
		}
		dispatched++
		if hasRun[item.ID] {
			continue
		}
		run, err := i.buildRun(ctx, item, teamByUID)
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
// can log-and-retry the item.
func (i *Intake) buildRun(ctx context.Context, item IntakeItem, teamByUID map[string]api.Team) (*api.Run, error) {
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

	// Agent selection (M1: one agent, one ticket): the Team composition's
	// first entry. spec.agents empty would defer to a reconciler default
	// (story 1.3) that has not landed — selecting the composition here keeps
	// the dispatch concrete instead of relying on an unimplemented default.
	if len(team.Spec.Agents) == 0 {
		return nil, fmt.Errorf("team %s/%s composition names no agent to dispatch to", team.Namespace, team.Name)
	}
	agentRef := team.Spec.Agents[0]
	agentNS := agentRef.Namespace
	if agentNS == "" {
		agentNS = ns
	}
	var agent api.Agent
	if err := i.Client.Get(ctx, client.ObjectKey{Namespace: agentNS, Name: agentRef.Name}, &agent); err != nil {
		return nil, fmt.Errorf("resolve agent %s/%s: %w", agentNS, agentRef.Name, err)
	}

	return &api.Run{
		ObjectMeta: runObjectMeta(item.ID, ns),
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
			Agents:      []api.ObjectRef{{Name: agentRef.Name}},
			OwnedBy:     api.PrincipalRef(IntakePrincipal),
		},
	}, nil
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
// item id> (a uuid is already DNS-1123-safe), in the squad namespace. The
// deterministic name IS the create idempotency: two racing sweeps converge on
// AlreadyExists instead of duplicate Runs.
func runObjectMeta(workItemID, ns string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      "intake-" + workItemID,
		Namespace: ns,
		Labels: map[string]string{
			"ksquad.io/created-by": "intake",
		},
	}
}

func (i *Intake) logf(format string, args ...any) {
	if i.Log != nil {
		i.Log(format, args...)
	}
}
