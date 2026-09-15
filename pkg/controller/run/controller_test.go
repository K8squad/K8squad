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
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/reconcile"
)

type fakeSource struct {
	step  reconcile.Step
	found bool
	err   error
}

func (f fakeSource) StepForWorkItem(context.Context, string) (reconcile.Step, bool, error) {
	return f.step, f.found, f.err
}

// capturingSource records the workItemID it was asked for, so a test can assert
// the reconciler keys on spec.workItemRef (not the k8s uid).
type capturingSource struct {
	step  reconcile.Step
	found bool
	gotID string
}

func (c *capturingSource) StepForWorkItem(_ context.Context, workItemID string) (reconcile.Step, bool, error) {
	c.gotID = workItemID
	return c.step, c.found, nil
}

// fakeSettle is a fixed (dispatched, settled) SettleSource answer, plus an
// optional error, for exercising the S3 finalize-window hold.
type fakeSettle struct {
	dispatched bool
	settled    bool
	err        error
}

func (f fakeSettle) SettledForWorkItem(context.Context, string) (bool, bool, error) {
	return f.dispatched, f.settled, f.err
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := api.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return s
}

func newRun() *api.Run {
	return &api.Run{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "run-1",
			Namespace:  "default",
			UID:        types.UID("uid-run-1"),
			Generation: 4,
		},
		Spec: api.RunSpec{WorkItemRef: "wi-abc-123"},
	}
}

func reconcileOnce(t *testing.T, c client.Client, src StepSource) (ctrl.Result, error) {
	t.Helper()
	r := &Reconciler{Client: c, Source: src, Now: func() metav1.Time { return fixedNow }}
	return r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "run-1", Namespace: "default"},
	})
}

// mutableSource lets a test advance the durable step between reconciles, so the
// projector can be driven through the happy-path sequence exactly as the driver
// advances coord.claim.reconcile_step.
type mutableSource struct {
	step  reconcile.Step
	found bool
}

func (m *mutableSource) StepForWorkItem(context.Context, string) (reconcile.Step, bool, error) {
	return m.step, m.found, nil
}

// TestReconcileProjectsHappyPathInOrder is the ISI-4381 core (AC2): driven
// through the durable happy-path step sequence, the projector patches
// status.phase to the matching phase at each step, in order — the intermediate
// Pending→Claiming→Running phases become visible instead of a single jump to
// Succeeded. Idempotency is preserved: a re-reconcile at an unchanged step does
// not rewrite status.
func TestReconcileProjectsHappyPathInOrder(t *testing.T) {
	run := newRun()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(run).WithStatusSubresource(&api.Run{}).Build()
	src := &mutableSource{found: true}
	rec := record.NewFakeRecorder(32)
	r := &Reconciler{Client: c, Source: src, Recorder: rec, Now: func() metav1.Time { return fixedNow }}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "run-1", Namespace: "default"}}

	steps := []struct {
		step  reconcile.Step
		phase api.RunPhase
	}{
		{reconcile.StepPending, api.RunPhasePending},
		{reconcile.StepClaimingSandbox, api.RunPhaseClaiming},
		{reconcile.StepDispatching, api.RunPhaseClaiming},
		{reconcile.StepRunning, api.RunPhaseRunning},
		{reconcile.StepCollecting, api.RunPhaseRunning},
		{reconcile.StepSucceeded, api.RunPhaseSucceeded},
	}
	for _, s := range steps {
		src.step = s.step
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("step %q: reconcile: %v", s.step, err)
		}
		got := getRun(t, c)
		if got.Status.Phase != s.phase {
			t.Fatalf("step %q: Phase = %q, want %q", s.step, got.Status.Phase, s.phase)
		}
		rvAfterFirst := got.ResourceVersion
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("step %q: re-reconcile: %v", s.step, err)
		}
		if again := getRun(t, c); again.ResourceVersion != rvAfterFirst {
			t.Errorf("step %q: status rewritten on no-op reconcile: rv %s -> %s",
				s.step, rvAfterFirst, again.ResourceVersion)
		}
	}
}

// TestReconcile_S3FinalizeWindowHold is the S3 acceptance (ADR-0020 §2.4 / F6,
// ISI-4403): a Succeeded/Failed durable step whose a2a follow has NOT durably
// settled projects Running (the finalize window), and flips to the terminal phase
// only once settlement lands. Non-a2a Runs (no dispatch row) and a nil Settlement
// source are non-regressing — they project the terminal step straight through.
func TestReconcile_S3FinalizeWindowHold(t *testing.T) {
	cases := []struct {
		name      string
		step      reconcile.Step
		settle    *fakeSettle // nil => no Settlement source wired (S3 disabled)
		wantPhase api.RunPhase
	}{
		{"succeeded-unsettled-holds-running", reconcile.StepSucceeded,
			&fakeSettle{dispatched: true, settled: false}, api.RunPhaseRunning},
		{"succeeded-settled-is-succeeded", reconcile.StepSucceeded,
			&fakeSettle{dispatched: true, settled: true}, api.RunPhaseSucceeded},
		{"failed-unsettled-holds-running", reconcile.StepFailed,
			&fakeSettle{dispatched: true, settled: false}, api.RunPhaseRunning},
		{"failed-settled-is-failed", reconcile.StepFailed,
			&fakeSettle{dispatched: true, settled: true}, api.RunPhaseFailed},
		{"non-a2a-run-not-held", reconcile.StepSucceeded,
			&fakeSettle{dispatched: false}, api.RunPhaseSucceeded},
		{"nil-settlement-is-noregression", reconcile.StepSucceeded,
			nil, api.RunPhaseSucceeded},
		// A still-running step is never a finalize candidate: the settlement read
		// must not fire and must not perturb the projection.
		{"running-step-untouched", reconcile.StepRunning,
			&fakeSettle{dispatched: true, settled: false}, api.RunPhaseRunning},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := newRun()
			c := fake.NewClientBuilder().WithScheme(newScheme(t)).
				WithObjects(run).WithStatusSubresource(&api.Run{}).Build()
			r := &Reconciler{
				Client: c,
				Source: fakeSource{step: tc.step, found: true},
				Now:    func() metav1.Time { return fixedNow },
			}
			if tc.settle != nil {
				r.Settlement = *tc.settle
			}
			if _, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "run-1", Namespace: "default"},
			}); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if got := getRun(t, c).Status.Phase; got != tc.wantPhase {
				t.Fatalf("Phase = %q, want %q", got, tc.wantPhase)
			}
		})
	}
}

// A Settlement read error surfaces so controller-runtime requeues with backoff,
// rather than the projector reading a stalled settlement as terminal.
func TestReconcile_S3SettlementErrorRequeues(t *testing.T) {
	run := newRun()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(run).WithStatusSubresource(&api.Run{}).Build()
	r := &Reconciler{
		Client:     c,
		Source:     fakeSource{step: reconcile.StepSucceeded, found: true},
		Settlement: fakeSettle{err: errors.New("connection reset")},
		Now:        func() metav1.Time { return fixedNow },
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "run-1", Namespace: "default"},
	}); err == nil {
		t.Fatal("a settlement read error must surface so the reconciler requeues")
	}
}

// TestReconcileEmitsEventPerPhaseTransition proves a Normal Event fires on each
// phase change (ISI-4381 polish) and NOT on a same-phase or no-op reconcile —
// so `kubectl describe run` shows the lifecycle and the event stream mirrors the
// phase transitions, no more.
func TestReconcileEmitsEventPerPhaseTransition(t *testing.T) {
	run := newRun()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(run).WithStatusSubresource(&api.Run{}).Build()
	src := &mutableSource{found: true}
	rec := record.NewFakeRecorder(32)
	r := &Reconciler{Client: c, Source: src, Recorder: rec, Now: func() metav1.Time { return fixedNow }}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "run-1", Namespace: "default"}}

	// Pending → Claiming(claiming_sandbox) → Claiming(dispatching) → Running.
	for _, step := range []reconcile.Step{
		reconcile.StepPending, reconcile.StepClaimingSandbox,
		reconcile.StepDispatching, reconcile.StepRunning, reconcile.StepRunning,
	} {
		src.step = step
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("step %q: reconcile: %v", step, err)
		}
	}
	// Phase changes: ""→Pending, Pending→Claiming, Running. The dispatching step
	// stays Claiming (no event) and the repeated Running is a no-op (no event).
	want := []string{"PhasePending", "PhaseClaiming", "PhaseRunning"}
	var got []string
	for {
		select {
		case e := <-rec.Events:
			got = append(got, eventReason(e))
			continue
		default:
		}
		break
	}
	if len(got) != len(want) {
		t.Fatalf("emitted %d events %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event[%d] reason = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// eventReason pulls the reason token out of a FakeRecorder event string, whose
// shape is "<Type> <Reason> <Message>".
func eventReason(e string) string {
	fields := strings.Fields(e)
	if len(fields) < 2 {
		return e
	}
	return fields[1]
}

func getRun(t *testing.T, c client.Client) api.Run {
	t.Helper()
	var got api.Run
	if err := c.Get(context.Background(), types.NamespacedName{Name: "run-1", Namespace: "default"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	return got
}

func TestReconcileProjectsDurableStep(t *testing.T) {
	run := newRun()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(run).WithStatusSubresource(&api.Run{}).Build()

	if _, err := reconcileOnce(t, c, fakeSource{step: reconcile.StepRunning, found: true}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got api.Run
	if err := c.Get(context.Background(), types.NamespacedName{Name: "run-1", Namespace: "default"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Phase != api.RunPhaseRunning {
		t.Errorf("Phase = %q, want Running", got.Status.Phase)
	}
	if got.Status.ObservedGeneration != 4 {
		t.Errorf("ObservedGeneration = %d, want 4", got.Status.ObservedGeneration)
	}
	if meta.FindStatusCondition(got.Status.Conditions, ConditionReady) == nil {
		t.Errorf("Ready condition not written")
	}
}

func TestReconcileKeysOnWorkItemRef(t *testing.T) {
	run := newRun() // spec.workItemRef = "wi-abc-123", uid = "uid-run-1"
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(run).WithStatusSubresource(&api.Run{}).Build()
	src := &capturingSource{step: reconcile.StepRunning, found: true}

	r := &Reconciler{Client: c, Source: src, Now: func() metav1.Time { return fixedNow }}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "run-1", Namespace: "default"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if src.gotID != "wi-abc-123" {
		t.Errorf("StepSource keyed on %q, want the spec.workItemRef %q (not the uid)", src.gotID, "wi-abc-123")
	}
}

func TestReconcileNoClaimRowIsPending(t *testing.T) {
	run := newRun()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(run).WithStatusSubresource(&api.Run{}).Build()

	// found=false: the Run is admitted but not yet enrolled in coord.
	if _, err := reconcileOnce(t, c, fakeSource{found: false}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got api.Run
	_ = c.Get(context.Background(), types.NamespacedName{Name: "run-1", Namespace: "default"}, &got)
	if got.Status.Phase != api.RunPhasePending {
		t.Errorf("Phase = %q, want Pending", got.Status.Phase)
	}
}

func TestReconcileSourceErrorRequeues(t *testing.T) {
	run := newRun()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(run).WithStatusSubresource(&api.Run{}).Build()

	_, err := reconcileOnce(t, c, fakeSource{err: errors.New("db stall")})
	if err == nil {
		t.Fatalf("expected error to trigger requeue, got nil")
	}
}

func TestReconcileMissingRunIsNoop(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithStatusSubresource(&api.Run{}).Build()

	res, err := reconcileOnce(t, c, fakeSource{step: reconcile.StepRunning, found: true})
	if err != nil {
		t.Fatalf("missing Run should not error, got %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("missing Run should not requeue, got %+v", res)
	}
}

// TestReconcileIsIdempotent proves a second pass with the same durable step does
// not rewrite status (no ResourceVersion churn), which keeps a hot requeue loop
// from hammering the API server.
func TestReconcileIsIdempotent(t *testing.T) {
	run := newRun()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(run).WithStatusSubresource(&api.Run{}).Build()
	src := fakeSource{step: reconcile.StepSucceeded, found: true}

	if _, err := reconcileOnce(t, c, src); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	var afterFirst api.Run
	_ = c.Get(context.Background(), types.NamespacedName{Name: "run-1", Namespace: "default"}, &afterFirst)

	if _, err := reconcileOnce(t, c, src); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	var afterSecond api.Run
	_ = c.Get(context.Background(), types.NamespacedName{Name: "run-1", Namespace: "default"}, &afterSecond)

	if afterFirst.ResourceVersion != afterSecond.ResourceVersion {
		t.Errorf("status rewritten on no-op reconcile: rv %s -> %s",
			afterFirst.ResourceVersion, afterSecond.ResourceVersion)
	}
	if cond := meta.FindStatusCondition(afterSecond.Status.Conditions, ConditionReady); cond == nil ||
		cond.Status != metav1.ConditionTrue {
		t.Errorf("expected Ready=True for Succeeded, got %+v", cond)
	}
}

// TestReconcileNonTerminalRequeues proves a non-terminal projection schedules a
// bounded resync so a coord step advanced without a Run CR event (ISI-4195) is
// still re-read and projected — the projector watches only the Run CR, so this
// requeue is the sole self-heal path for that race.
func TestReconcileNonTerminalRequeues(t *testing.T) {
	run := newRun()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(run).WithStatusSubresource(&api.Run{}).Build()

	res, err := reconcileOnce(t, c, fakeSource{step: reconcile.StepClaimingSandbox, found: true})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != DefaultResync {
		t.Errorf("non-terminal RequeueAfter = %v, want %v", res.RequeueAfter, DefaultResync)
	}
}

// TestReconcileTerminalDoesNotRequeue proves a terminal projection is absorbing:
// no requeue, so steady-state cost is zero.
func TestReconcileTerminalDoesNotRequeue(t *testing.T) {
	run := newRun()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(run).WithStatusSubresource(&api.Run{}).Build()

	res, err := reconcileOnce(t, c, fakeSource{step: reconcile.StepSucceeded, found: true})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("terminal RequeueAfter = %v, want 0", res.RequeueAfter)
	}
}

// TestReconcileNoOpStillRequeuesWhileNonTerminal is the direct ISI-4195
// regression: the SECOND pass over an unchanged non-terminal step (the exact
// shape of a stuck Claiming projection) must STILL requeue. A bare nil on the
// idempotent path would kill the only loop that re-reads coord and self-heals.
func TestReconcileNoOpStillRequeuesWhileNonTerminal(t *testing.T) {
	run := newRun()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(run).WithStatusSubresource(&api.Run{}).Build()
	src := fakeSource{step: reconcile.StepClaimingSandbox, found: true}

	if _, err := reconcileOnce(t, c, src); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	res, err := reconcileOnce(t, c, src) // no status change: DeepEqual short-circuit path
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if res.RequeueAfter != DefaultResync {
		t.Errorf("no-op non-terminal RequeueAfter = %v, want %v", res.RequeueAfter, DefaultResync)
	}
}

// TestReconcileResyncOverride proves the Resync field overrides the default.
func TestReconcileResyncOverride(t *testing.T) {
	run := newRun()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(run).WithStatusSubresource(&api.Run{}).Build()
	r := &Reconciler{Client: c, Source: fakeSource{step: reconcile.StepRunning, found: true},
		Now: func() metav1.Time { return fixedNow }, Resync: 5 * time.Second}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "run-1", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 5*time.Second {
		t.Errorf("RequeueAfter = %v, want 5s", res.RequeueAfter)
	}
}
