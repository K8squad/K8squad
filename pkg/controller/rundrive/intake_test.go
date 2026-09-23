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

package rundrive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeIntakeSource binds the board read-side without a live Postgres.
type fakeIntakeSource struct {
	items []IntakeItem
	err   error
	calls []int
	// rearmed records the work-item ids RearmSettled was called for, in order
	// (ISI-4556): the mint must re-arm a settled claim BEFORE creating the Run.
	rearmed []string
	// rearmErr, when set, makes RearmSettled fail (the fail-closed path must
	// skip the mint entirely, leaving the item on todo for the next tick).
	rearmErr error
}

func (f *fakeIntakeSource) DueWorkItems(_ context.Context, limit int) ([]IntakeItem, error) {
	f.calls = append(f.calls, limit)
	if f.err != nil {
		return nil, f.err
	}
	return f.items, nil
}

func (f *fakeIntakeSource) RearmSettled(_ context.Context, workItemID string) error {
	f.rearmed = append(f.rearmed, workItemID)
	return f.rearmErr
}

// squadGraph builds the minimal resolvable world: a Team (uid teamUID, squad
// ns squadNS, composition [agentName]) + its Agent + a Project named projectName.
func squadGraph(teamUID, squadNS, teamName, agentName, projectName string) []client.Object {
	team := &api.Team{
		ObjectMeta: metav1.ObjectMeta{
			Name: teamName,
			UID:  types.UID(teamUID),
		},
		Spec: api.TeamSpec{
			NamespaceStrategy: "dedicated",
			Agents:            []api.ObjectRef{{Name: agentName}},
		},
	}
	team.Status.Namespace = squadNS
	agent := &api.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: agentName, Namespace: squadNS},
	}
	project := &api.Project{
		ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: squadNS},
	}
	return []client.Object{team, agent, project}
}

func intakeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	if err := api.AddToScheme(sch); err != nil {
		t.Fatalf("build scheme: %v", err)
	}
	return sch
}

// newIntake assembles the sweeper over a fake client seeded with objs.
func newIntake(t *testing.T, src IntakeSource, objs ...client.Object) (*Intake, client.WithWatch, *[]string) {
	t.Helper()
	var logs []string
	cl := fake.NewClientBuilder().
		WithScheme(intakeScheme(t)).
		WithObjects(objs...).
		Build()
	in := &Intake{
		Source: src,
		Client: cl,
		Log:    func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
	}
	return in, cl, &logs
}

// ---------------------------------------------------------------------------
// Board read-side (SQL shape)
// ---------------------------------------------------------------------------

// TestSQLIntakeSourceDueWorkItems pins the shipped query shape: todo + team
// assigned, bounded, identifiers only.
func TestSQLIntakeSourceDueWorkItems(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	src, err := NewSQLIntakeSource(db)
	if err != nil {
		t.Fatalf("NewSQLIntakeSource: %v", err)
	}

	rows := sqlmock.NewRows([]string{"id", "team_id", "project_id", "requested_agent"}).
		AddRow("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222", "proj-a", "coder").
		AddRow("33333333-3333-3333-3333-333333333333", "22222222-2222-2222-2222-222222222222", "proj-a", nil)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id::text, team_id::text, project_id::text, requested_agent
		  FROM coord.work_item
		 WHERE state = 'todo' AND team_id IS NOT NULL
		 ORDER BY created_at, id
		 LIMIT $1`)).
		WithArgs(7).
		WillReturnRows(rows)

	items, err := src.DueWorkItems(context.Background(), 7)
	if err != nil {
		t.Fatalf("DueWorkItems: %v", err)
	}
	if len(items) != 2 || items[0].ID != "11111111-1111-1111-1111-111111111111" ||
		items[0].TeamID != "22222222-2222-2222-2222-222222222222" || items[0].ProjectID != "proj-a" {
		t.Fatalf("unexpected items: %+v", items)
	}
	// requested_agent is selected and NULL scans to "" (Intake → Team.Spec.Agents[0]).
	if items[0].RequestedAgent != "coder" {
		t.Fatalf("row 0 requested_agent: got %q want coder", items[0].RequestedAgent)
	}
	if items[1].RequestedAgent != "" {
		t.Fatalf("row 1 requested_agent (NULL): got %q want empty", items[1].RequestedAgent)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestNewSQLIntakeSourceNilDB(t *testing.T) {
	if _, err := NewSQLIntakeSource(nil); err == nil {
		t.Fatal("nil db must error, not panic")
	}
}

// ---------------------------------------------------------------------------
// Sweep: the dispatch decision
// ---------------------------------------------------------------------------

// The happy path, the whole M1.3 contract in one test: a todo ticket reaches
// the Run plane as ONE Run CR in the squad namespace, addressed to the team's
// agent, carrying only identifiers.
func TestIntakeSweepDispatchesTodoWorkItem(t *testing.T) {
	const (
		itemID     = "11111111-1111-1111-1111-111111111111"
		teamUID    = "22222222-2222-2222-2222-222222222222"
		squadNS    = "squad-alpha"
		agentName  = "coder"
		projectUID = "proj-uid-1"
	)
	// Project resolvable by UID…
	objs := squadGraph(teamUID, squadNS, "alpha", agentName, "proj-cr-name")
	for _, o := range objs {
		if p, ok := o.(*api.Project); ok {
			p.UID = types.UID(projectUID)
		}
	}
	in, cl, _ := newIntake(t, &fakeIntakeSource{items: []IntakeItem{
		{ID: itemID, TeamID: teamUID, ProjectID: projectUID},
	}}, objs...)

	in.sweep(context.Background())

	var runs api.RunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs.Items) != 1 {
		t.Fatalf("want exactly 1 dispatched Run, got %d", len(runs.Items))
	}
	run := runs.Items[0]
	if run.Name != "intake-"+itemID {
		t.Fatalf("deterministic name: got %q want intake-%s", run.Name, itemID)
	}
	if run.Namespace != squadNS {
		t.Fatalf("run namespace: got %q want squad namespace %q", run.Namespace, squadNS)
	}
	if run.Spec.WorkItemRef != itemID {
		t.Fatalf("workItemRef: got %q", run.Spec.WorkItemRef)
	}
	if run.Spec.TeamRef.Name != "alpha" {
		t.Fatalf("teamRef: got %q", run.Spec.TeamRef.Name)
	}
	if run.Spec.ProjectRef.Name != "proj-cr-name" {
		t.Fatalf("projectRef should resolve UID→CR name, got %q", run.Spec.ProjectRef.Name)
	}
	if len(run.Spec.Agents) != 1 || run.Spec.Agents[0].Name != agentName {
		t.Fatalf("agents: got %+v want [%s]", run.Spec.Agents, agentName)
	}
	if run.Spec.OwnedBy != api.PrincipalRef(IntakePrincipal) {
		t.Fatalf("ownedBy: got %q want %q", run.Spec.OwnedBy, IntakePrincipal)
	}
}

// ISI-4820: the composition's Agent CRs live in the Team's HOME namespace
// (Team.Namespace, e.g. bmad-squad), NOT the reconciled EXEC namespace
// (Status.Namespace). buildRun must resolve the agent in the home ns and carry
// that ns on the Run's dispatch-agent ref so dispatch.go finds the same CR —
// otherwise every non-mirrored agent silently fails to mint a Run and the
// ticket loops on 'todo' forever ("works only for sam; change status to force").
func TestIntakeSweepResolvesAgentInHomeNamespace(t *testing.T) {
	const (
		itemID  = "11111111-1111-1111-1111-111111111111"
		teamUID = "22222222-2222-2222-2222-222222222222"
		homeNS  = "bmad-squad"
		execNS  = "ksquad-team-bmad-squad-f6e8fc70"
		agent   = "winston"
		projUID = "proj-uid-1"
	)
	team := &api.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "bmad-squad", Namespace: homeNS, UID: types.UID(teamUID)},
		Spec: api.TeamSpec{
			NamespaceStrategy: "dedicated",
			// composition agent carries an EMPTY namespace (the live shape)
			Agents: []api.ObjectRef{{Name: agent}},
		},
	}
	team.Status.Namespace = execNS
	objs := []client.Object{
		team,
		// Agent + Project authored in the HOME ns, NOT the exec ns.
		&api.Agent{ObjectMeta: metav1.ObjectMeta{Name: agent, Namespace: homeNS}},
		&api.Project{ObjectMeta: metav1.ObjectMeta{Name: "proj", Namespace: homeNS, UID: types.UID(projUID)}},
	}
	in, cl, logs := newIntake(t, &fakeIntakeSource{items: []IntakeItem{
		{ID: itemID, TeamID: teamUID, ProjectID: projUID},
	}}, objs...)

	in.sweep(context.Background())

	var runs api.RunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs.Items) != 1 {
		t.Fatalf("want 1 minted Run (home-ns agent must resolve), got %d; logs=%v", len(runs.Items), *logs)
	}
	run := runs.Items[0]
	if run.Namespace != execNS {
		t.Fatalf("run namespace: got %q want exec ns %q", run.Namespace, execNS)
	}
	if len(run.Spec.Agents) != 1 || run.Spec.Agents[0].Name != agent {
		t.Fatalf("agents: got %+v want [%s]", run.Spec.Agents, agent)
	}
	// The dispatch-agent ref MUST carry the home ns (≠ run ns) so dispatch.go's
	// own empty-ns→run.Namespace fallback resolves the same CR.
	if run.Spec.Agents[0].Namespace != homeNS {
		t.Fatalf("agent ref namespace: got %q want home ns %q (else dispatch re-fails)", run.Spec.Agents[0].Namespace, homeNS)
	}
}

// ISI-4820: the mirrored case (`sam` was copied into the exec ns). When the
// Agent CR resolves in the run's OWN ns, the ref stays bare — the existing
// convention (TeamRef/ProjectRef only carry a ns when cross-ns).
func TestIntakeSweepResolvesMirroredAgentInExecNamespace(t *testing.T) {
	const (
		itemID  = "11111111-1111-1111-1111-111111111111"
		teamUID = "22222222-2222-2222-2222-222222222222"
		homeNS  = "bmad-squad"
		execNS  = "ksquad-team-bmad-squad-f6e8fc70"
		agent   = "sam"
		projUID = "proj-uid-1"
	)
	team := &api.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "bmad-squad", Namespace: homeNS, UID: types.UID(teamUID)},
		Spec: api.TeamSpec{
			NamespaceStrategy: "dedicated",
			Agents:            []api.ObjectRef{{Name: agent}},
		},
	}
	team.Status.Namespace = execNS
	objs := []client.Object{
		team,
		// Agent present ONLY in the exec ns (mirrored), not home; project home.
		&api.Agent{ObjectMeta: metav1.ObjectMeta{Name: agent, Namespace: execNS}},
		&api.Project{ObjectMeta: metav1.ObjectMeta{Name: "proj", Namespace: homeNS, UID: types.UID(projUID)}},
	}
	in, cl, logs := newIntake(t, &fakeIntakeSource{items: []IntakeItem{
		{ID: itemID, TeamID: teamUID, ProjectID: projUID},
	}}, objs...)

	in.sweep(context.Background())

	var runs api.RunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs.Items) != 1 {
		t.Fatalf("want 1 minted Run (exec-ns fallback must resolve), got %d; logs=%v", len(runs.Items), *logs)
	}
	run := runs.Items[0]
	if len(run.Spec.Agents) != 1 || run.Spec.Agents[0].Name != agent {
		t.Fatalf("agents: got %+v want [%s]", run.Spec.Agents, agent)
	}
	if run.Spec.Agents[0].Namespace != "" {
		t.Fatalf("agent ref namespace: got %q want bare (agent in run's own ns)", run.Spec.Agents[0].Namespace)
	}
}

// twoAgentSquad builds a resolvable world with a Team whose composition is
// [agent0, agent1] (both Agent CRs present in the squad ns) + a Project. Used to
// prove the requested_agent preference actually selects a non-default agent.
func twoAgentSquad(teamUID, squadNS, teamName, agent0, agent1, projectName string) []client.Object {
	team := &api.Team{
		ObjectMeta: metav1.ObjectMeta{Name: teamName, UID: types.UID(teamUID)},
		Spec: api.TeamSpec{
			NamespaceStrategy: "dedicated",
			Agents:            []api.ObjectRef{{Name: agent0}, {Name: agent1}},
		},
	}
	team.Status.Namespace = squadNS
	return []client.Object{
		team,
		&api.Agent{ObjectMeta: metav1.ObjectMeta{Name: agent0, Namespace: squadNS}},
		&api.Agent{ObjectMeta: metav1.ObjectMeta{Name: agent1, Namespace: squadNS}},
		&api.Project{ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: squadNS}},
	}
}

// ADR-0022 §7.4: a ticket carrying requested_agent mints its Run addressed to
// THAT agent, not the hardcoded Team.Spec.Agents[0] — the whole point of the
// dispatch path (the human's choice rides the level-triggered pipeline).
func TestIntakeSweepHonorsRequestedAgent(t *testing.T) {
	const (
		itemID  = "11111111-1111-1111-1111-111111111111"
		teamUID = "22222222-2222-2222-2222-222222222222"
		squadNS = "squad-alpha"
	)
	objs := twoAgentSquad(teamUID, squadNS, "alpha", "coder", "reviewer", "proj")
	in, cl, _ := newIntake(t, &fakeIntakeSource{items: []IntakeItem{
		{ID: itemID, TeamID: teamUID, ProjectID: "proj", RequestedAgent: "reviewer"},
	}}, objs...)

	in.sweep(context.Background())

	var runs api.RunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs.Items) != 1 {
		t.Fatalf("want 1 Run, got %d", len(runs.Items))
	}
	if got := runs.Items[0].Spec.Agents; len(got) != 1 || got[0].Name != "reviewer" {
		t.Fatalf("requested agent must win over Agents[0]: got %+v want [reviewer]", got)
	}
}

// Defensive validation (§7.4): a requested_agent no longer in the composition
// (the team changed between dispatch and this tick) falls back to Agents[0]
// rather than minting a Run for an off-team agent — and says so in the log.
func TestIntakeSweepStaleRequestedAgentFallsBack(t *testing.T) {
	const (
		itemID  = "11111111-1111-1111-1111-111111111111"
		teamUID = "22222222-2222-2222-2222-222222222222"
		squadNS = "squad-alpha"
	)
	objs := twoAgentSquad(teamUID, squadNS, "alpha", "coder", "reviewer", "proj")
	in, cl, logs := newIntake(t, &fakeIntakeSource{items: []IntakeItem{
		{ID: itemID, TeamID: teamUID, ProjectID: "proj", RequestedAgent: "ghost"},
	}}, objs...)

	in.sweep(context.Background())

	var runs api.RunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs.Items) != 1 {
		t.Fatalf("want 1 Run, got %d", len(runs.Items))
	}
	if got := runs.Items[0].Spec.Agents; len(got) != 1 || got[0].Name != "coder" {
		t.Fatalf("stale requested agent must fall back to Agents[0]: got %+v want [coder]", got)
	}
	found := false
	for _, l := range *logs {
		if strings.Contains(l, "no longer in team") && strings.Contains(l, "ghost") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a fallback log mentioning the stale agent; logs=%v", *logs)
	}
}

// A ticket the Run plane already owns — by ANY author, any name — is never
// given a second Run (the suppress rule that makes re-sweeps free).
func TestIntakeSweepSkipsWorkItemWithExistingRun(t *testing.T) {
	const (
		itemID  = "11111111-1111-1111-1111-111111111111"
		teamUID = "22222222-2222-2222-2222-222222222222"
	)
	objs := squadGraph(teamUID, "squad-alpha", "alpha", "coder", "proj")
	objs = append(objs, &api.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "hand-authored", Namespace: "squad-alpha"},
		Spec:       api.RunSpec{WorkItemRef: itemID},
	})
	in, cl, _ := newIntake(t, &fakeIntakeSource{items: []IntakeItem{
		{ID: itemID, TeamID: teamUID, ProjectID: "proj"},
	}}, objs...)

	in.sweep(context.Background())

	var runs api.RunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs.Items) != 1 || runs.Items[0].Name != "hand-authored" {
		t.Fatalf("intake must not add a Run; got %+v", runs.Items)
	}
}

// The deterministic name makes the create itself idempotent: two sweeps in a
// row converge (second sees the Run via the index anyway; a race that slips
// between read and create lands on AlreadyExists, counted as dispatched).
func TestIntakeSweepIdempotentAcrossPasses(t *testing.T) {
	const (
		itemID  = "11111111-1111-1111-1111-111111111111"
		teamUID = "22222222-2222-2222-2222-222222222222"
	)
	objs := squadGraph(teamUID, "squad-alpha", "alpha", "coder", "proj")
	in, cl, _ := newIntake(t, &fakeIntakeSource{items: []IntakeItem{
		{ID: itemID, TeamID: teamUID, ProjectID: "proj"},
	}}, objs...)

	in.sweep(context.Background())
	in.sweep(context.Background())

	var runs api.RunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs.Items) != 1 {
		t.Fatalf("re-sweep must not duplicate: got %d Runs", len(runs.Items))
	}
}

// ISI-4495 (comment-triggered re-dispatch): a TERMINAL Run no longer suppresses
// intake. A ticket that re-entered 'todo' (a human reopen, or a human comment
// nudging a parked lane) gets the NEXT generation Run, deterministically
// suffixed -r2, so it coexists with the terminal first-born instead of
// colliding on intake-<id>.
func TestIntakeSweepRemintsAfterTerminalRun(t *testing.T) {
	const (
		itemID  = "11111111-1111-1111-1111-111111111111"
		teamUID = "22222222-2222-2222-2222-222222222222"
	)
	objs := squadGraph(teamUID, "squad-alpha", "alpha", "coder", "proj")
	objs = append(objs, &api.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "intake-" + itemID, Namespace: "squad-alpha"},
		Spec:       api.RunSpec{WorkItemRef: itemID},
		Status:     api.RunStatus{Phase: api.RunPhaseSucceeded},
	})
	in, cl, _ := newIntake(t, &fakeIntakeSource{items: []IntakeItem{
		{ID: itemID, TeamID: teamUID, ProjectID: "proj"},
	}}, objs...)

	in.sweep(context.Background())

	var runs api.RunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs.Items) != 2 {
		t.Fatalf("want the terminal Run + one re-mint, got %d", len(runs.Items))
	}
	for _, r := range runs.Items {
		if r.Name == "intake-"+itemID && r.Status.Phase != api.RunPhaseSucceeded {
			t.Fatalf("first-born Run must be untouched, got phase %q", r.Status.Phase)
		}
	}
	want := "intake-" + itemID + "-r2"
	found := false
	for _, r := range runs.Items {
		if r.Name == want {
			found = true
			if r.Spec.WorkItemRef != itemID {
				t.Fatalf("re-mint workItemRef: got %q", r.Spec.WorkItemRef)
			}
		}
	}
	if !found {
		t.Fatalf("re-mint %q not created; got %+v", want, runs.Items)
	}

	// Idempotency: a second sweep sees the re-mint (phase unset ⇒ live) and
	// must not mint a third generation.
	in.sweep(context.Background())
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("re-list runs: %v", err)
	}
	if len(runs.Items) != 2 {
		t.Fatalf("re-sweep must not over-mint: got %d Runs", len(runs.Items))
	}
}

// ISI-4829 regression: old generations are GC-pruned, so the SURVIVING Run set
// has gaps. Naming the next mint off the count of surviving Runs (2 here) lands
// on "-r3", which still exists — Create answers AlreadyExists every tick and no
// fresh Run is ever minted: the ticket freezes on 'todo' ("impossible to trigger
// an agent run from the UI"). The next mint must clear the HIGHEST surviving
// suffix (r3) → "-r4", never the count.
func TestIntakeSweepRemintClearsHighestSurvivingGeneration(t *testing.T) {
	const (
		itemID  = "11111111-1111-1111-1111-111111111111"
		teamUID = "22222222-2222-2222-2222-222222222222"
	)
	objs := squadGraph(teamUID, "squad-alpha", "alpha", "coder", "proj")
	// Two surviving terminal Runs with a pruned gap: the first-born (gen 1) and a
	// high generation (r3). Intermediate r2 was GC'd. Count == 2.
	objs = append(objs,
		&api.Run{
			ObjectMeta: metav1.ObjectMeta{Name: "intake-" + itemID, Namespace: "squad-alpha"},
			Spec:       api.RunSpec{WorkItemRef: itemID},
			Status:     api.RunStatus{Phase: api.RunPhaseSucceeded},
		},
		&api.Run{
			ObjectMeta: metav1.ObjectMeta{Name: "intake-" + itemID + "-r3", Namespace: "squad-alpha"},
			Spec:       api.RunSpec{WorkItemRef: itemID},
			Status:     api.RunStatus{Phase: api.RunPhaseSucceeded},
		},
	)
	in, cl, _ := newIntake(t, &fakeIntakeSource{items: []IntakeItem{
		{ID: itemID, TeamID: teamUID, ProjectID: "proj"},
	}}, objs...)

	in.sweep(context.Background())

	var runs api.RunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs.Items) != 3 {
		t.Fatalf("want the 2 survivors + one re-mint, got %d Runs", len(runs.Items))
	}
	want := "intake-" + itemID + "-r4"
	found := false
	for _, r := range runs.Items {
		if r.Name == want {
			found = true
		}
	}
	if !found {
		names := make([]string, 0, len(runs.Items))
		for _, r := range runs.Items {
			names = append(names, r.Name)
		}
		t.Fatalf("re-mint must clear highest surviving suffix as %q; got %v", want, names)
	}
}

// The live half of the guard: a Running (non-terminal) Run still suppresses
// intake — a comment on a ticket whose run is actively working must never mint
// a racing second Run (§6.2 custody).
func TestIntakeSweepStillSuppressesLiveRun(t *testing.T) {
	const (
		itemID  = "11111111-1111-1111-1111-111111111111"
		teamUID = "22222222-2222-2222-2222-222222222222"
	)
	objs := squadGraph(teamUID, "squad-alpha", "alpha", "coder", "proj")
	objs = append(objs, &api.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "intake-" + itemID, Namespace: "squad-alpha"},
		Spec:       api.RunSpec{WorkItemRef: itemID},
		Status:     api.RunStatus{Phase: api.RunPhaseRunning},
	})
	in, cl, _ := newIntake(t, &fakeIntakeSource{items: []IntakeItem{
		{ID: itemID, TeamID: teamUID, ProjectID: "proj"},
	}}, objs...)

	in.sweep(context.Background())

	var runs api.RunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs.Items) != 1 {
		t.Fatalf("live Run must suppress re-mint: got %d Runs", len(runs.Items))
	}
}

// A create racing to AlreadyExists is a dispatched ticket, not an error.
func TestIntakeSweepAlreadyExistsTolerated(t *testing.T) {
	const (
		itemID  = "11111111-1111-1111-1111-111111111111"
		teamUID = "22222222-2222-2222-2222-222222222222"
	)
	objs := squadGraph(teamUID, "squad-alpha", "alpha", "coder", "proj")
	in, _, logs := newIntake(t, &fakeIntakeSource{items: []IntakeItem{
		{ID: itemID, TeamID: teamUID, ProjectID: "proj"},
	}}, objs...)

	// Force the create to answer AlreadyExists (a racing sweep won the name).
	in.Client = fake.NewClientBuilder().
		WithScheme(intakeScheme(t)).
		WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*api.Run); ok {
					return apierrors.NewAlreadyExists(schema.GroupResource{Group: "ksquad.io", Resource: "runs"}, obj.GetName())
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()

	in.sweep(context.Background())

	for _, l := range *logs {
		if strings.Contains(l, "create run for") {
			t.Fatalf("AlreadyExists must not log as a create error: %s", l)
		}
	}
}

// Honest-degraded table: each unresolvable world state leaves the ticket in
// todo (no Run) and says why in the log.
func TestIntakeSweepHonestDegraded(t *testing.T) {
	const teamUID = "22222222-2222-2222-2222-222222222222"
	cases := []struct {
		name string
		objs []client.Object
		item IntakeItem
		want string
	}{
		{
			name: "team uid resolves to no team",
			objs: nil,
			item: IntakeItem{ID: "i-1", TeamID: teamUID, ProjectID: "p"},
			want: "resolves to no Team CR",
		},
		{
			name: "team namespace not reconciled",
			objs: func() []client.Object {
				objs := squadGraph(teamUID, "squad-alpha", "alpha", "coder", "p")
				objs[0].(*api.Team).Status.Namespace = ""
				return objs
			}(),
			item: IntakeItem{ID: "i-1", TeamID: teamUID, ProjectID: "p"},
			want: "no reconciled squad namespace",
		},
		{
			name: "team composition empty",
			objs: func() []client.Object {
				objs := squadGraph(teamUID, "squad-alpha", "alpha", "coder", "p")
				objs[0].(*api.Team).Spec.Agents = nil
				return objs
			}(),
			item: IntakeItem{ID: "i-1", TeamID: teamUID, ProjectID: "p"},
			want: "names no agent",
		},
		{
			name: "project unresolvable",
			objs: squadGraph(teamUID, "squad-alpha", "alpha", "coder", "other-proj"),
			item: IntakeItem{ID: "i-1", TeamID: teamUID, ProjectID: "missing-proj"},
			want: "resolves to no Project CR",
		},
		{
			name: "agent missing",
			objs: func() []client.Object {
				objs := squadGraph(teamUID, "squad-alpha", "alpha", "gone-agent", "p")
				return objs[:len(objs)-2] // drop agent + project… then re-add project
			}(),
			item: IntakeItem{ID: "i-1", TeamID: teamUID, ProjectID: "p"},
			want: "resolve agent",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "agent missing" {
				tc.objs = append(tc.objs, &api.Project{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "squad-alpha"}})
			}
			in, cl, logs := newIntake(t, &fakeIntakeSource{items: []IntakeItem{tc.item}}, tc.objs...)
			in.sweep(context.Background())
			var runs api.RunList
			if err := cl.List(context.Background(), &runs); err != nil {
				t.Fatalf("list runs: %v", err)
			}
			if len(runs.Items) != 0 {
				t.Fatalf("degraded world must not create a Run; got %d", len(runs.Items))
			}
			found := false
			for _, l := range *logs {
				if strings.Contains(l, tc.want) {
					found = true
				}
			}
			if !found {
				t.Fatalf("no log line explains the skip (want %q); logs: %v", tc.want, *logs)
			}
		})
	}
}

// A source error is logged and retried next tick — the sweeper must survive
// its own infrastructure.
func TestIntakeSweepSourceErrorLoggedNotFatal(t *testing.T) {
	in, cl, logs := newIntake(t, &fakeIntakeSource{err: errors.New("db gone")})
	in.sweep(context.Background())
	if len(*logs) == 0 {
		t.Fatal("source error must be logged")
	}
	var runs api.RunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("list runs: %v", err)
	}
}

// The per-pass bound drains a wide todo lane progressively.
func TestIntakeSweepBoundsPerPass(t *testing.T) {
	const teamUID = "22222222-2222-2222-2222-222222222222"
	objs := squadGraph(teamUID, "squad-alpha", "alpha", "coder", "p")
	var items []IntakeItem
	for i := 0; i < 5; i++ {
		items = append(items, IntakeItem{
			ID:        "item-" + string(rune('a'+i)),
			TeamID:    teamUID,
			ProjectID: "p",
		})
	}
	in, cl, _ := newIntake(t, &fakeIntakeSource{items: items}, objs...)
	in.MaxPerPass = 2
	in.sweep(context.Background())

	var runs api.RunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs.Items) != 2 {
		t.Fatalf("MaxPerPass=2 must bound the pass; got %d Runs", len(runs.Items))
	}
}

// Start honors ctx cancellation (the manager Runnable contract).
func TestIntakeStartStopsOnContextCancel(t *testing.T) {
	in, _, _ := newIntake(t, &fakeIntakeSource{})
	in.Tick = time.Hour // an hour: only cancellation can end it promptly
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- in.Start(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not honor cancellation")
	}
}

// ---------------------------------------------------------------------------
// ISI-4556: the generation re-arm — a settled claim re-armed BEFORE the mint
// ---------------------------------------------------------------------------

// The ISI-4556 contract: a ticket whose only Runs are terminal (settled
// generation 1) gets its claim re-armed and exactly ONE next-generation Run
// minted. The re-arm must precede the mint — a Run born over a terminal
// durable step absorbs (AC5), the projector stamps it 'Succeeded', and the
// next tick mints again: the runaway mint loop. Here the rearm-error arm
// below proves the ordering structurally: no re-arm, no mint.
func TestIntakeSweepRearmsSettledClaimBeforeMint(t *testing.T) {
	const (
		itemID  = "11111111-1111-1111-1111-111111111111"
		teamUID = "22222222-2222-2222-2222-222222222222"
	)
	objs := squadGraph(teamUID, "squad-alpha", "alpha", "coder", "proj")
	objs = append(objs, &api.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "intake-" + itemID, Namespace: "squad-alpha"},
		Spec:       api.RunSpec{WorkItemRef: itemID},
		Status:     api.RunStatus{Phase: api.RunPhaseSucceeded},
	})
	src := &fakeIntakeSource{items: []IntakeItem{
		{ID: itemID, TeamID: teamUID, ProjectID: "proj"},
	}}
	in, cl, _ := newIntake(t, src, objs...)

	in.sweep(context.Background())

	if len(src.rearmed) != 1 || src.rearmed[0] != itemID {
		t.Fatalf("the minted item must be re-armed exactly once first: rearmed=%v", src.rearmed)
	}
	var runs api.RunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs.Items) != 2 {
		t.Fatalf("want terminal gen-1 + exactly one re-mint, got %d Runs", len(runs.Items))
	}
	want := "intake-" + itemID + "-r2"
	found := false
	for _, r := range runs.Items {
		if r.Name == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("re-mint %q missing; got runs: %+v", want, runs.Items)
	}

	// Stability across ticks (the operator-restart shape: a todo item whose
	// only Runs are terminal must not re-mint a generation every tick): the
	// second sweep sees the re-mint (phase unset ⇒ live) and mints nothing.
	src.rearmed = nil
	in.sweep(context.Background())
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("re-list runs: %v", err)
	}
	if len(runs.Items) != 2 {
		t.Fatalf("re-sweep must not over-mint: got %d Runs", len(runs.Items))
	}
	if len(src.rearmed) != 0 {
		t.Fatalf("a live (suppressed) item must not be re-armed: rearmed=%v", src.rearmed)
	}
}

// A ticket whose run is actively working (non-terminal) is suppressed BEFORE
// the re-arm: a live Run's claim must never be touched, not even as a no-op.
func TestIntakeSweepDoesNotRearmLiveRunItems(t *testing.T) {
	const (
		itemID  = "11111111-1111-1111-1111-111111111111"
		teamUID = "22222222-2222-2222-2222-222222222222"
	)
	objs := squadGraph(teamUID, "squad-alpha", "alpha", "coder", "proj")
	objs = append(objs, &api.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "intake-" + itemID, Namespace: "squad-alpha"},
		Spec:       api.RunSpec{WorkItemRef: itemID},
		Status:     api.RunStatus{Phase: api.RunPhaseRunning},
	})
	src := &fakeIntakeSource{items: []IntakeItem{
		{ID: itemID, TeamID: teamUID, ProjectID: "proj"},
	}}
	in, cl, _ := newIntake(t, src, objs...)

	in.sweep(context.Background())

	if len(src.rearmed) != 0 {
		t.Fatalf("live item must not be re-armed: rearmed=%v", src.rearmed)
	}
	var runs api.RunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs.Items) != 1 {
		t.Fatalf("live Run must suppress mint + re-arm: got %d Runs", len(runs.Items))
	}
}

// Fail-closed: a re-arm infrastructure error must skip the mint entirely —
// minting over an unre-armed terminal step is exactly the ISI-4556 loop — and
// say so in the log, leaving the item on todo for the next tick.
func TestIntakeSweepRearmFailureSkipsMint(t *testing.T) {
	const (
		itemID  = "11111111-1111-1111-1111-111111111111"
		teamUID = "22222222-2222-2222-2222-222222222222"
	)
	objs := squadGraph(teamUID, "squad-alpha", "alpha", "coder", "proj")
	src := &fakeIntakeSource{
		items:    []IntakeItem{{ID: itemID, TeamID: teamUID, ProjectID: "proj"}},
		rearmErr: errors.New("coord db: connection refused"),
	}
	in, cl, logs := newIntake(t, src, objs...)

	in.sweep(context.Background())

	var runs api.RunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs.Items) != 0 {
		t.Fatalf("re-arm failure must skip the mint: got %d Runs", len(runs.Items))
	}
	found := false
	for _, l := range *logs {
		if strings.Contains(l, "not re-armed") && strings.Contains(l, itemID) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a not-re-armed log naming the item; logs=%v", *logs)
	}
}

// TestSQLIntakeSourceRearmSettled pins the shipped re-arm transaction shape
// (ISI-4556): ONE transaction — the guarded step reset (terminal step +
// released checkout + todo lane, fence bump), the §6.5 'reconcile_rearmed'
// audit row and the §6.6 outbox event — or, on a zero-row guard match, a
// committed no-op with NO audit/outbox rows.
func TestSQLIntakeSourceRearmSettled(t *testing.T) {
	const itemID = "11111111-1111-1111-1111-111111111111"
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()
	src, err := NewSQLIntakeSource(db)
	if err != nil {
		t.Fatalf("NewSQLIntakeSource: %v", err)
	}

	rearmQ := regexp.QuoteMeta(`UPDATE coord.claim
		   SET reconcile_step    = 'pending',
		       fence_token       = fence_token + 1,
		       reclaim_fenced_at = clock_timestamp()
		 WHERE work_item_id = $1::uuid
		   AND holder_principal IS NULL
		   AND reconcile_step IN ('succeeded','failed','cancelled')
		   AND EXISTS (
		         SELECT 1 FROM coord.work_item wi
		          WHERE wi.id = coord.claim.work_item_id
		            AND wi.state = 'todo')
		 RETURNING fence_token`)
	auditQ := regexp.QuoteMeta(`INSERT INTO coord.audit_log
		       (work_item_id, run_id, event_type, principal, fence_token, to_state)
		VALUES ($1::uuid, NULL, 'reconcile_rearmed', $2, $3, 'pending')`)
	outboxQ := regexp.QuoteMeta(`INSERT INTO coord.outbox
		       (entity, project_id, squad, event_type, work_item_id, run_id, payload)
		SELECT 'run', wi.project_id, wi.team_id::text, 'reconcile_rearmed',
		       wi.id, NULL,
		       jsonb_build_object('to_step', 'pending'::text, 'fence_token', $2::bigint)
		  FROM coord.work_item wi WHERE wi.id = $1::uuid`)

	// The re-armed path: guard matches → audit + outbox co-commit.
	mock.ExpectBegin()
	mock.ExpectQuery(rearmQ).
		WithArgs(itemID).
		WillReturnRows(sqlmock.NewRows([]string{"fence_token"}).AddRow(7))
	mock.ExpectExec(auditQ).
		WithArgs(itemID, "ksquad-operator", int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(outboxQ).
		WithArgs(itemID, int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := src.RearmSettled(context.Background(), itemID); err != nil {
		t.Fatalf("RearmSettled (re-armed): %v", err)
	}

	// The no-op path: guard matches zero rows (fresh item, live claim, or the
	// lane moved on) → committed empty transaction, no audit/outbox rows.
	mock.ExpectBegin()
	mock.ExpectQuery(rearmQ).
		WithArgs(itemID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectCommit()
	if err := src.RearmSettled(context.Background(), itemID); err != nil {
		t.Fatalf("RearmSettled (no-op): %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}
