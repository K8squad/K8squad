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
}

func (f *fakeIntakeSource) DueWorkItems(_ context.Context, limit int) ([]IntakeItem, error) {
	f.calls = append(f.calls, limit)
	if f.err != nil {
		return nil, f.err
	}
	return f.items, nil
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

	rows := sqlmock.NewRows([]string{"id", "team_id", "project_id"}).
		AddRow("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222", "proj-a")
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id::text, team_id::text, project_id::text
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
	if len(items) != 1 || items[0].ID != "11111111-1111-1111-1111-111111111111" ||
		items[0].TeamID != "22222222-2222-2222-2222-222222222222" || items[0].ProjectID != "proj-a" {
		t.Fatalf("unexpected items: %+v", items)
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
