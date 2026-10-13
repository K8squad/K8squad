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

package run

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/reconcile"
)

// fakeJournal records every PhaseEdge the reconciler appends, and can be told
// to fail the write (to prove a journal error never fails the reconcile).
type fakeJournal struct {
	edges []PhaseEdge
	err   error
}

func (f *fakeJournal) RecordRunPhaseTransition(_ context.Context, edge PhaseEdge) error {
	if f.err != nil {
		return f.err
	}
	f.edges = append(f.edges, edge)
	return nil
}

// newRunWithTenancy is newRun() with the ProjectRef/TeamRef the journal derives
// tenancy from (api.ObjectRef carries only a name).
func newRunWithTenancy() *api.Run {
	run := newRun()
	run.Spec.ProjectRef = api.ObjectRef{Name: "proj-a"}
	run.Spec.TeamRef = api.ObjectRef{Name: "team-a"}
	return run
}

func projectCR(name, uid string) *api.Project {
	return &api.Project{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(uid)}}
}

func teamCRns(name, uid string) *api.Team {
	return &api.Team{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(uid)}}
}

// TestPhaseToken pins the CR-phase → chip-token fold for every member of the
// RunPhase state machine (arch §8) plus the empty/unknown phase. This is the
// contract WS-3 reconstruction keys line up with (coord.StatusSnapshot.Counts),
// so a silent change here would skew the chart.
func TestPhaseToken(t *testing.T) {
	cases := []struct {
		phase api.RunPhase
		token string
		ok    bool
	}{
		{api.RunPhasePending, phaseTokenClaiming, true},
		{api.RunPhaseClaiming, phaseTokenClaiming, true},
		{api.RunPhaseRunning, phaseTokenRunning, true},
		{api.RunPhasePaused, phaseTokenRunning, true},
		{api.RunPhaseSucceeded, phaseTokenComplete, true},
		{api.RunPhaseFailed, phaseTokenFailed, true},
		{api.RunPhaseCanceling, phaseTokenFailed, true},
		{api.RunPhaseCancelled, phaseTokenFailed, true},
		{api.RunPhase(""), "", false},
		{api.RunPhase("Bogus"), "", false},
	}
	for _, tc := range cases {
		tok, ok := phaseToken(tc.phase)
		if tok != tc.token || ok != tc.ok {
			t.Errorf("phaseToken(%q) = (%q, %v), want (%q, %v)", tc.phase, tok, ok, tc.token, tc.ok)
		}
	}
}

// TestReconcileJournalsPhaseTransitions is the WS-2 core (ISI-5749): driven
// through the durable happy path, the projector appends exactly one journal row
// per CHIP-TOKEN change, deriving tenancy from the Run CR. It proves both
// acceptance halves at once:
//   - emit on an actual change: NULL→claiming, claiming→running, running→complete.
//   - NO emit on an unchanged token: the Pending→Claiming CR transition folds
//     claiming→claiming (two distinct CR phases, same chart band) and is skipped,
//     and a repeated same-step reconcile is a no-op.
func TestReconcileJournalsPhaseTransitions(t *testing.T) {
	run := newRunWithTenancy()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(run, projectCR("proj-a", "uid-proj-a"), teamCRns("team-a", "uid-team-a")).
		WithStatusSubresource(&api.Run{}).Build()
	src := &mutableSource{found: true}
	jr := &fakeJournal{}
	r := &Reconciler{Client: c, Source: src, PhaseJournal: jr, Now: func() metav1.Time { return fixedNow }}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "run-1", Namespace: "default"}}

	// Pending → ClaimingSandbox → Running → Running(no-op) → Succeeded.
	for _, step := range []reconcile.Step{
		reconcile.StepPending, reconcile.StepClaimingSandbox,
		reconcile.StepRunning, reconcile.StepRunning, reconcile.StepSucceeded,
	} {
		src.step = step
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("step %q: reconcile: %v", step, err)
		}
	}

	want := []struct{ from, to string }{
		{"", phaseTokenClaiming}, // first edge: NULL from_phase
		{phaseTokenClaiming, phaseTokenRunning},
		{phaseTokenRunning, phaseTokenComplete},
	}
	if len(jr.edges) != len(want) {
		t.Fatalf("appended %d edges %+v, want %d", len(jr.edges), jr.edges, len(want))
	}
	for i, w := range want {
		e := jr.edges[i]
		if e.FromPhase != w.from || e.ToPhase != w.to {
			t.Errorf("edge[%d] = %q->%q, want %q->%q", i, e.FromPhase, e.ToPhase, w.from, w.to)
		}
		if e.RunUID != "uid-run-1" || e.ProjectID != "uid-proj-a" || e.TeamID != "uid-team-a" {
			t.Errorf("edge[%d] tenancy = run=%q project=%q team=%q, want run=uid-run-1 project=uid-proj-a team=uid-team-a",
				i, e.RunUID, e.ProjectID, e.TeamID)
		}
		if !e.TS.Equal(fixedNow.Time) {
			t.Errorf("edge[%d] TS = %v, want %v", i, e.TS, fixedNow.Time)
		}
	}
}

// TestReconcileJournalNoEmitOnUnchangedPhase proves the no-op path: a repeated
// reconcile at the same durable step (hence same phase) appends nothing, so the
// append-only journal never accrues a spurious same-token edge.
func TestReconcileJournalNoEmitOnUnchangedPhase(t *testing.T) {
	run := newRunWithTenancy()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(run, projectCR("proj-a", "uid-proj-a"), teamCRns("team-a", "uid-team-a")).
		WithStatusSubresource(&api.Run{}).Build()
	src := &mutableSource{found: true, step: reconcile.StepRunning}
	jr := &fakeJournal{}
	r := &Reconciler{Client: c, Source: src, PhaseJournal: jr, Now: func() metav1.Time { return fixedNow }}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "run-1", Namespace: "default"}}

	// First reconcile moves ""→Running (one edge); the next two are no-ops.
	for i := 0; i < 3; i++ {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	if len(jr.edges) != 1 {
		t.Fatalf("appended %d edges %+v, want exactly 1 (the ''→running edge, then no-ops)", len(jr.edges), jr.edges)
	}
	if jr.edges[0].FromPhase != "" || jr.edges[0].ToPhase != phaseTokenRunning {
		t.Errorf("edge = %q->%q, want ''->running", jr.edges[0].FromPhase, jr.edges[0].ToPhase)
	}
}

// TestReconcileJournalTeamNullWhenUnresolved: a Run whose Team CR is absent still
// records an honest row with an empty TeamID (NULL team_id — fleet-admin still
// sees it, ADR-0032); only Project is required.
func TestReconcileJournalTeamNullWhenUnresolved(t *testing.T) {
	run := newRunWithTenancy()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(run, projectCR("proj-a", "uid-proj-a")). // no Team CR
		WithStatusSubresource(&api.Run{}).Build()
	src := &mutableSource{found: true, step: reconcile.StepRunning}
	jr := &fakeJournal{}
	r := &Reconciler{Client: c, Source: src, PhaseJournal: jr, Now: func() metav1.Time { return fixedNow }}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "run-1", Namespace: "default"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(jr.edges) != 1 {
		t.Fatalf("appended %d edges, want 1", len(jr.edges))
	}
	if jr.edges[0].TeamID != "" || jr.edges[0].ProjectID != "uid-proj-a" {
		t.Errorf("tenancy = team=%q project=%q, want team='' project=uid-proj-a", jr.edges[0].TeamID, jr.edges[0].ProjectID)
	}
}

// TestReconcileJournalSkipsWhenProjectUnresolved: project_id is the journal's
// required tenancy predicate, so a Run whose Project CR cannot be resolved is
// skipped — and the status reconcile still succeeds (observability history must
// never fail the control loop).
func TestReconcileJournalSkipsWhenProjectUnresolved(t *testing.T) {
	run := newRunWithTenancy()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(run). // neither Project nor Team CR present
		WithStatusSubresource(&api.Run{}).Build()
	src := &mutableSource{found: true, step: reconcile.StepRunning}
	jr := &fakeJournal{}
	r := &Reconciler{Client: c, Source: src, PhaseJournal: jr, Now: func() metav1.Time { return fixedNow }}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "run-1", Namespace: "default"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile must still succeed when the journal is skipped: %v", err)
	}
	if len(jr.edges) != 0 {
		t.Fatalf("appended %d edges, want 0 (project unresolved ⇒ skip)", len(jr.edges))
	}
	// The status projection itself still landed.
	if got := getRun(t, c); got.Status.Phase != api.RunPhaseRunning {
		t.Errorf("status.phase = %q, want Running (projection must land even when the journal is skipped)", got.Status.Phase)
	}
}

// TestReconcileJournalWriteErrorNeverFailsReconcile: a journal append failure is
// logged and swallowed — the reconcile returns no error and the status patch is
// unaffected.
func TestReconcileJournalWriteErrorNeverFailsReconcile(t *testing.T) {
	run := newRunWithTenancy()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(run, projectCR("proj-a", "uid-proj-a"), teamCRns("team-a", "uid-team-a")).
		WithStatusSubresource(&api.Run{}).Build()
	src := &mutableSource{found: true, step: reconcile.StepRunning}
	jr := &fakeJournal{err: errors.New("db down")}
	r := &Reconciler{Client: c, Source: src, PhaseJournal: jr, Now: func() metav1.Time { return fixedNow }}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "run-1", Namespace: "default"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("a journal write error must not fail the reconcile, got %v", err)
	}
	if got := getRun(t, c); got.Status.Phase != api.RunPhaseRunning {
		t.Errorf("status.phase = %q, want Running", got.Status.Phase)
	}
}

// TestReconcileNilJournalNoop: the seam is optional — a nil PhaseJournal reconcile
// projects status and does not panic (pre-WS-2 behaviour, strictly non-regressing).
func TestReconcileNilJournalNoop(t *testing.T) {
	run := newRunWithTenancy()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(run).WithStatusSubresource(&api.Run{}).Build()
	src := &mutableSource{found: true, step: reconcile.StepRunning}
	r := &Reconciler{Client: c, Source: src, Now: func() metav1.Time { return fixedNow }}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "run-1", Namespace: "default"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile with nil PhaseJournal: %v", err)
	}
	if got := getRun(t, c); got.Status.Phase != api.RunPhaseRunning {
		t.Errorf("status.phase = %q, want Running", got.Status.Phase)
	}
}
