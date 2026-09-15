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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/coord"
	"github.com/K8squad/K8squad/pkg/reconcile"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeClaims struct {
	state    ClaimState
	found    bool
	stateErr error

	laps    int
	lapsErr error

	acquireFence int64
	acquireOK    bool
	acquireErr   error
	acquireCalls []string

	renewOK    bool
	renewErr   error
	renewCalls int

	retryNewFence int64
	retryOK       bool
	retryErr      error
	retryCalls    []string

	failOK   bool
	failErr  error
	failCall bool

	cancelFinishOK   bool
	cancelFinishErr  error
	cancelFinishCall bool

	cancelDue    []string
	cancelDueErr error

	requeueOK   bool
	requeueErr  error
	requeueCall bool
}

func (f *fakeClaims) State(context.Context, string) (ClaimState, bool, error) {
	return f.state, f.found, f.stateErr
}
func (f *fakeClaims) LapsUsed(context.Context, string) (int, error) { return f.laps, f.lapsErr }
func (f *fakeClaims) Acquire(_ context.Context, workItemID, runID, agentName string) (int64, bool, error) {
	f.acquireCalls = append(f.acquireCalls, workItemID+"/"+runID+"/"+agentName)
	return f.acquireFence, f.acquireOK, f.acquireErr
}
func (f *fakeClaims) Renew(_ context.Context, workItemID, runID string, fence int64) (bool, error) {
	f.renewCalls++
	return f.renewOK, f.renewErr
}
func (f *fakeClaims) RetryEnter(_ context.Context, workItemID, runID string, fence int64) (int64, bool, error) {
	f.retryCalls = append(f.retryCalls, fmt.Sprintf("%s/%s/%d", workItemID, runID, fence))
	return f.retryNewFence, f.retryOK, f.retryErr
}
func (f *fakeClaims) FailEnter(_ context.Context, workItemID, runID string, fence int64) (bool, error) {
	f.failCall = true
	return f.failOK, f.failErr
}
func (f *fakeClaims) CancelEnter(_ context.Context, workItemID, runID string, fence int64) (bool, error) {
	return true, nil
}
func (f *fakeClaims) CancelFinish(_ context.Context, workItemID, runID string, fence int64) (bool, error) {
	f.cancelFinishCall = true
	return f.cancelFinishOK, f.cancelFinishErr
}
func (f *fakeClaims) CancelDue(context.Context) ([]string, error) { return f.cancelDue, f.cancelDueErr }
func (f *fakeClaims) RequeuePaused(context.Context, string) (bool, error) {
	f.requeueCall = true
	return f.requeueOK, f.requeueErr
}

type fakePauses struct {
	pendingAt   time.Time
	pendingHas  bool
	pendingErr  error
	recorded    []string
	recordErr   error
	recordInfo  coord.PauseInfo
	recordCalls int
}

func (f *fakePauses) Pending(context.Context, string) (time.Time, bool, error) {
	return f.pendingAt, f.pendingHas, f.pendingErr
}
func (f *fakePauses) Record(_ context.Context, workItemID, runID string, _ *time.Duration) (coord.PauseInfo, error) {
	f.recordCalls++
	f.recorded = append(f.recorded, workItemID+"/"+runID)
	return f.recordInfo, f.recordErr
}

// fakeMachineStore is a controllable machineStore: it drives the machine's
// Step()/Fence()/Advance() surface from a scripted step sequence.
type fakeMachineStore struct {
	step     reconcile.Step
	fence    int64
	advances int
	// advanceOK: when false Advance never commits (the spin-guard shape).
	advanceOK bool
	err       error
}

func (s *fakeMachineStore) Step() reconcile.Step { return s.step }
func (s *fakeMachineStore) Fence() int64         { return s.fence }
func (s *fakeMachineStore) Advance(expected, next reconcile.Step, fence *int64) bool {
	s.advances++
	if !s.advanceOK {
		return false
	}
	if s.step != expected {
		return false
	}
	s.step = next
	return true
}
func (s *fakeMachineStore) Reclaim(int64) bool { return true }
func (s *fakeMachineStore) SetStep(step reconcile.Step) {
	s.step = step
}
func (s *fakeMachineStore) AuditRows() int { return 0 }
func (s *fakeMachineStore) OutboxRows() int {
	return 0
}
func (s *fakeMachineStore) Err() error { return s.err }

type fakeMachineEffects struct {
	err        error
	binds      int
	dispatches []string
	collects   int
	terminals  []reconcile.Step
}

func (e *fakeMachineEffects) BindSandbox(string, bool)   { e.binds++ }
func (e *fakeMachineEffects) Dispatch(id string, _ bool) { e.dispatches = append(e.dispatches, id) }
func (e *fakeMachineEffects) Collect(string, string, bool) {
	e.collects++
}
func (e *fakeMachineEffects) Terminal(s reconcile.Step) { e.terminals = append(e.terminals, s) }
func (e *fakeMachineEffects) Err() error                { return e.err }

type fakeRunner struct {
	store   *fakeMachineStore
	effects *fakeMachineEffects
	storeFn func(context.Context, *api.Run) (machineStore, error)
	err     error
}

func (r *fakeRunner) Store(ctx context.Context, run *api.Run) (machineStore, error) {
	if r.storeFn != nil {
		return r.storeFn(ctx, run)
	}
	if r.err != nil {
		return nil, r.err
	}
	return r.store, nil
}
func (r *fakeRunner) Effects(context.Context, *api.Run) (machineEffects, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.effects, nil
}

type fakeReleaser struct {
	released []string
	err      error
}

func (f *fakeReleaser) Release(_ context.Context, runID string) error {
	f.released = append(f.released, runID)
	return f.err
}

// newTestRun builds a Run CR with the test's identifiers.
func newTestRun(uid, workItem string) *api.Run {
	return &api.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "default", UID: types.UID(uid)},
		Spec: api.RunSpec{
			TeamRef:     api.ObjectRef{Name: "t"},
			ProjectRef:  api.ObjectRef{Name: "p"},
			WorkItemRef: workItem,
			// ISI-4237: the dispatch attribution the drive stamps on the
			// acquire — the board's "who is working this".
			Agents: []api.ObjectRef{{Name: "sam"}},
		},
	}
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := api.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return s
}

func newDriver(cl client.Client, claims Claims, pauses Pauses, runner Runner) *Driver {
	d := NewDriver(cl, claims, pauses, runner)
	d.Rand = func() float64 { return 0 } // deterministic: jitter = exp/2 floor... full? EqualJitter(0) → half+0 = half
	return d
}

func runOnce(t *testing.T, d *Driver, name types.NamespacedName) (requeueAfter time.Duration, err error) {
	t.Helper()
	res, err := d.Reconcile(context.Background(), ctrl.Request{NamespacedName: name})
	if err != nil {
		return 0, err
	}
	return res.RequeueAfter, nil
}

// ---------------------------------------------------------------------------
// Drive loop (3.1)
// ---------------------------------------------------------------------------

// TestDriveHappyPathToTerminal: an enrolled, healthy claim drives the machine
// to succeeded in one pass — no requeue, terminal effects recorded.
func TestDriveHappyPathToTerminal(t *testing.T) {
	run := newTestRun("11111111-1111-1111-1111-111111111111", "10000000-0000-0000-0000-000000000001")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).WithIndex(&api.Run{}, workItemField,
		func(obj client.Object) []string { return []string{obj.(*api.Run).Spec.WorkItemRef} }).Build()

	claims := &fakeClaims{found: true, state: ClaimState{Step: reconcile.StepPending, Fence: 1, ItemState: "todo"},
		acquireOK: true, acquireFence: 1}
	store := &fakeMachineStore{step: reconcile.StepPending, fence: 1, advanceOK: true}
	eff := &fakeMachineEffects{}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: eff})

	rq, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"})
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if rq != 0 {
		t.Fatalf("requeue = %v, want 0 (terminal)", rq)
	}
	if store.step != reconcile.StepSucceeded {
		t.Fatalf("durable step = %q, want succeeded", store.step)
	}
	if eff.binds == 0 || len(eff.dispatches) == 0 || eff.collects == 0 {
		t.Fatalf("effects not driven: binds=%d dispatches=%v collects=%d", eff.binds, eff.dispatches, eff.collects)
	}
	if len(claims.acquireCalls) != 1 ||
		claims.acquireCalls[0] != "10000000-0000-0000-0000-000000000001/11111111-1111-1111-1111-111111111111/sam" {
		t.Fatalf("§6.2 acquire calls = %v, want exactly one for the driven run with its agent (ISI-4237)", claims.acquireCalls)
	}
}

// TestDriveNotifiesProjectorOnStepAdvance: a drive that advances the durable
// step calls NotifyPhase with the driven Run so the status projector re-reads
// coord and projects the new phase at once (ISI-4381 Option A), instead of
// waiting for its non-terminal resync.
func TestDriveNotifiesProjectorOnStepAdvance(t *testing.T) {
	run := newTestRun("11111111-1111-1111-1111-111111111111", "10000000-0000-0000-0000-000000000001")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).WithIndex(&api.Run{}, workItemField,
		func(obj client.Object) []string { return []string{obj.(*api.Run).Spec.WorkItemRef} }).Build()

	claims := &fakeClaims{found: true, state: ClaimState{Step: reconcile.StepPending, Fence: 1, ItemState: "todo"},
		acquireOK: true, acquireFence: 1}
	store := &fakeMachineStore{step: reconcile.StepPending, fence: 1, advanceOK: true}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: &fakeMachineEffects{}})
	var kicked []string
	d.NotifyPhase = func(r *api.Run) { kicked = append(kicked, string(r.UID)) }

	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if len(kicked) != 1 || kicked[0] != string(run.UID) {
		t.Fatalf("NotifyPhase calls = %v, want one for the driven run (step advanced pending→succeeded)", kicked)
	}
}

// TestDriveDoesNotNotifyWhenStepUnchanged: a bounded drive that commits no
// transition (spin-guard shape — Advance never lands) must not wake the
// projector, since there is no new phase to project.
func TestDriveDoesNotNotifyWhenStepUnchanged(t *testing.T) {
	run := newTestRun("11111111-1111-1111-1111-111111111111", "10000000-0000-0000-0000-000000000001")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).WithIndex(&api.Run{}, workItemField,
		func(obj client.Object) []string { return []string{obj.(*api.Run).Spec.WorkItemRef} }).Build()

	claims := &fakeClaims{found: true, state: ClaimState{Step: reconcile.StepPending, Fence: 1, ItemState: "todo"},
		acquireOK: true, acquireFence: 1}
	store := &fakeMachineStore{step: reconcile.StepPending, fence: 1, advanceOK: false} // never commits
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: &fakeMachineEffects{}})
	notified := false
	d.NotifyPhase = func(*api.Run) { notified = true }

	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if notified {
		t.Fatalf("NotifyPhase called on a no-commit drive; want no kick when the durable step is unchanged")
	}
}

// TestDriveNotEnrolledIsANoOp: a Run whose work item has no claim row is left
// alone — the driver never invents coordination state (ADR-001).
func TestDriveNotEnrolledIsANoOp(t *testing.T) {
	run := newTestRun("11111111-1111-1111-1111-111111111111", "ffffffff-ffff-ffff-ffff-ffffffffffff")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).Build()
	claims := &fakeClaims{found: false}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{})

	rq, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"})
	if err != nil || rq != 0 {
		t.Fatalf("not-enrolled drive: rq=%v err=%v, want 0/nil", rq, err)
	}
}

// TestDriveTerminalStepIsAbsorbing: terminal durable steps are never re-driven.
func TestDriveTerminalStepIsAbsorbing(t *testing.T) {
	run := newTestRun("11111111-1111-1111-1111-111111111111", "10000000-0000-0000-0000-000000000001")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).Build()
	claims := &fakeClaims{found: true, state: ClaimState{Step: reconcile.StepSucceeded, Fence: 3}}
	runner := &fakeRunner{store: &fakeMachineStore{}, effects: &fakeMachineEffects{}}
	d := newDriver(cl, claims, &fakePauses{}, runner)

	if rq, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err != nil || rq != 0 {
		t.Fatalf("terminal drive: rq=%v err=%v", rq, err)
	}
	if runner.store.advances != 0 {
		t.Fatalf("terminal Run was driven (%d advances)", runner.store.advances)
	}
}

// TestDriveLapThreading: retry laps used ⇒ the machine dispatches a FRESH
// task id (run#lapN+1), never the first-attempt id.
func TestDriveLapThreading(t *testing.T) {
	run := newTestRun("11111111-1111-1111-1111-111111111111", "10000000-0000-0000-0000-000000000001")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).Build()
	claims := &fakeClaims{found: true, state: ClaimState{Step: reconcile.StepClaimingSandbox, Fence: 2, ItemState: "in_progress"},
		laps: 2, acquireOK: true, acquireFence: 2}
	store := &fakeMachineStore{step: reconcile.StepClaimingSandbox, fence: 2, advanceOK: true}
	eff := &fakeMachineEffects{}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: eff})

	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err != nil {
		t.Fatalf("drive: %v", err)
	}
	want := fmt.Sprintf("%s#lap%d", reconcile.RunID, 3)
	if len(eff.dispatches) == 0 || eff.dispatches[0] != want {
		t.Fatalf("dispatch id = %v, want [%s] (fresh retry lap)", eff.dispatches, want)
	}
}

// TestDriveEffectsErrorRequeues: a sticky infrastructure error on the effects
// seam surfaces as a reconcile error (controller-runtime backoff), never as
// "applied".
func TestDriveEffectsErrorRequeues(t *testing.T) {
	run := newTestRun("11111111-1111-1111-1111-111111111111", "10000000-0000-0000-0000-000000000001")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).Build()
	claims := &fakeClaims{found: true, state: ClaimState{Step: reconcile.StepPending, Fence: 1, ItemState: "todo"},
		acquireOK: true, acquireFence: 1}
	store := &fakeMachineStore{step: reconcile.StepPending, fence: 1, advanceOK: true}
	eff := &fakeMachineEffects{err: errors.New("db down")}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: eff})

	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err == nil {
		t.Fatal("effects error must surface as a reconcile error")
	}
}

// TestDriveSpinGuardRequeues: a store whose Advance cannot commit (fence raced)
// exhausts MaxPasses and requeues short instead of wedging.
func TestDriveSpinGuardRequeues(t *testing.T) {
	run := newTestRun("11111111-1111-1111-1111-111111111111", "10000000-0000-0000-0000-000000000001")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).Build()
	claims := &fakeClaims{found: true, state: ClaimState{Step: reconcile.StepPending, Fence: 1, ItemState: "todo"},
		acquireOK: true, acquireFence: 1}
	store := &fakeMachineStore{step: reconcile.StepPending, fence: 1, advanceOK: false}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: &fakeMachineEffects{}})

	rq, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"})
	if err != nil {
		t.Fatalf("spin-guard drive: %v", err)
	}
	if rq != continueDelay {
		t.Fatalf("requeue = %v, want continueDelay (%v)", rq, continueDelay)
	}
}

// TestDriveClaimStateReadError: an infra read failure requeues with the error.
func TestDriveClaimStateReadError(t *testing.T) {
	run := newTestRun("11111111-1111-1111-1111-111111111111", "10000000-0000-0000-0000-000000000001")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).Build()
	d := newDriver(cl, &fakeClaims{found: true, stateErr: errors.New("conn refused")},
		&fakePauses{}, &fakeRunner{})

	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err == nil {
		t.Fatal("claim read error must surface")
	}
}

// ---------------------------------------------------------------------------
// §6.2 checkout acquire at drive start (M1.3 / ISI-4183)
// ---------------------------------------------------------------------------

// TestDriveAcquireErrorSurfaces: an acquire infrastructure failure is a
// reconcile error (controller-runtime backoff), never a silent drive without
// custody.
func TestDriveAcquireErrorSurfaces(t *testing.T) {
	run := newTestRun("11111111-1111-1111-1111-111111111111", "10000000-0000-0000-0000-000000000001")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).Build()
	claims := &fakeClaims{found: true, state: ClaimState{Step: reconcile.StepPending, ItemState: "todo"},
		acquireErr: errors.New("claim db down")}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{})

	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err == nil {
		t.Fatal("acquire error must surface as a reconcile error")
	}
}

// TestDriveAcquireContendedRequeuesShort: a guard rejection (live foreign
// lease, or the lane raced us) requeues short — nothing changed, the next
// pass re-reads the world. The machine must NOT drive without custody.
func TestDriveAcquireContendedRequeuesShort(t *testing.T) {
	run := newTestRun("11111111-1111-1111-1111-111111111111", "10000000-0000-0000-0000-000000000001")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).Build()
	claims := &fakeClaims{found: true, state: ClaimState{Step: reconcile.StepPending, ItemState: "todo"},
		acquireOK: false}
	store := &fakeMachineStore{step: reconcile.StepPending, advanceOK: true}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: &fakeMachineEffects{}})

	rq, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"})
	if err != nil {
		t.Fatalf("contended acquire: %v", err)
	}
	if rq != continueDelay {
		t.Fatalf("requeue = %v, want %v", rq, continueDelay)
	}
	if store.advances != 0 {
		t.Fatalf("machine drove without custody (%d advances)", store.advances)
	}
}

// TestDriveNotClaimableLaneAbsorbs: backlog (§13: parked, never claimable),
// in_review and done lanes offer no custody path — the drive absorbs (no
// requeue poll, no machine drive) and the resync backstop owns any later
// re-drive.
func TestDriveNotClaimableLaneAbsorbs(t *testing.T) {
	run := newTestRun("11111111-1111-1111-1111-111111111111", "10000000-0000-0000-0000-000000000001")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).Build()
	store := &fakeMachineStore{step: reconcile.StepPending, advanceOK: true}
	for _, lane := range []string{"backlog", "in_review", "done", ""} {
		claims := &fakeClaims{found: true, state: ClaimState{Step: reconcile.StepPending, ItemState: lane}}
		d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: &fakeMachineEffects{}})
		rq, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"})
		if err != nil || rq != 0 {
			t.Fatalf("lane %q: rq=%v err=%v, want 0/nil (absorb)", lane, rq, err)
		}
		if len(claims.acquireCalls) != 0 {
			t.Fatalf("lane %q: acquire attempted (%v)", lane, claims.acquireCalls)
		}
	}
	if store.advances != 0 {
		t.Fatalf("machine drove an unclaimable item (%d advances)", store.advances)
	}
}

// TestDriveHeldByRunRenewsAndSkipsAcquire: a checkout already stamped to THIS
// run with a live lease is renewed (the §6.2 heartbeat, every drive pass) and
// NOT re-acquired — the fence bump of a redundant acquire would churn custody.
func TestDriveHeldByRunRenewsAndSkipsAcquire(t *testing.T) {
	uid := "11111111-1111-1111-1111-111111111111"
	run := newTestRun(uid, "10000000-0000-0000-0000-000000000001")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).Build()
	future := time.Now().Add(time.Hour)
	claims := &fakeClaims{found: true, state: ClaimState{
		Step: reconcile.StepPending, Fence: 4, Holder: "ksquad-operator", RunID: uid,
		LeaseExpiresAt: &future, ItemState: "in_progress"}, renewOK: true}
	store := &fakeMachineStore{step: reconcile.StepPending, fence: 4, advanceOK: true}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: &fakeMachineEffects{}})

	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err != nil {
		t.Fatalf("held drive: %v", err)
	}
	if len(claims.acquireCalls) != 0 {
		t.Fatalf("held checkout re-acquired: %v", claims.acquireCalls)
	}
	if claims.renewCalls != 1 {
		t.Fatalf("renew calls = %d, want 1", claims.renewCalls)
	}
	if store.step != reconcile.StepSucceeded {
		t.Fatalf("durable step = %q, want succeeded", store.step)
	}
}

// TestDriveRenewLostRequeuesShort: a failed renew (custody fenced/reclaimed
// under us mid-life) requeues short instead of driving on a stale fence.
func TestDriveRenewLostRequeuesShort(t *testing.T) {
	uid := "11111111-1111-1111-1111-111111111111"
	run := newTestRun(uid, "10000000-0000-0000-0000-000000000001")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).Build()
	future := time.Now().Add(time.Hour)
	claims := &fakeClaims{found: true, state: ClaimState{
		Step: reconcile.StepPending, Fence: 4, Holder: "ksquad-operator", RunID: uid,
		LeaseExpiresAt: &future, ItemState: "in_progress"}, renewOK: false}
	store := &fakeMachineStore{step: reconcile.StepPending, advanceOK: true}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: &fakeMachineEffects{}})

	rq, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"})
	if err != nil {
		t.Fatalf("lost renew: %v", err)
	}
	if rq != continueDelay {
		t.Fatalf("requeue = %v, want %v", rq, continueDelay)
	}
	if store.advances != 0 {
		t.Fatalf("machine drove on a lost checkout (%d advances)", store.advances)
	}
}

// TestDriveExpiredOwnLeaseReAcquires: a checkout stamped to this run whose
// lease lapsed at a NON-in-flight step (no 3.2 death) is re-acquired — the
// free-or-expired guard accepts it and the lease is refreshed.
func TestDriveExpiredOwnLeaseReAcquires(t *testing.T) {
	uid := "11111111-1111-1111-1111-111111111111"
	run := newTestRun(uid, "10000000-0000-0000-0000-000000000001")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).Build()
	claims := &fakeClaims{found: true, state: ClaimState{
		Step: reconcile.StepPending, Fence: 2, Holder: "ksquad-operator", RunID: uid,
		LeaseExpiresAt: leaseAgo(time.Minute), ItemState: "in_progress"}, acquireOK: true, acquireFence: 3}
	store := &fakeMachineStore{step: reconcile.StepPending, advanceOK: true}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: &fakeMachineEffects{}})

	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err != nil {
		t.Fatalf("re-acquire drive: %v", err)
	}
	if len(claims.acquireCalls) != 1 {
		t.Fatalf("re-acquire calls = %v, want 1", claims.acquireCalls)
	}
	if store.step != reconcile.StepSucceeded {
		t.Fatalf("durable step = %q, want succeeded", store.step)
	}
}

// ---------------------------------------------------------------------------
// Death detection + retry lap (3.2)
// ---------------------------------------------------------------------------

func leaseAgo(d time.Duration) *time.Time {
	t := time.Now().Add(-d)
	return &t
}

// TestDeathDetectedRetriesWithinBudget: an in-flight Run with an expired lease
// under a holder tears its sandbox down, enters the retry lap (fence-first),
// and requeues on backoff.
func TestDeathDetectedRetriesWithinBudget(t *testing.T) {
	run := newTestRun("11111111-1111-1111-1111-111111111111", "10000000-0000-0000-0000-000000000001")
	max := int32(3)
	run.Spec.RetryPolicy = &api.RetryPolicy{MaxRetries: &max, BackoffSeconds: nil}
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).Build()

	claims := &fakeClaims{
		found: true,
		state: ClaimState{Step: reconcile.StepRunning, Fence: 7, Holder: "agent-1",
			LeaseExpiresAt: leaseAgo(time.Minute)},
		laps: 1, retryOK: true, retryNewFence: 8,
	}
	rel := &fakeReleaser{}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{})
	d.Sandbox = rel

	rq, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"})
	if err != nil {
		t.Fatalf("retry drive: %v", err)
	}
	if rq == 0 {
		t.Fatal("retry lap must requeue on backoff")
	}
	if len(claims.retryCalls) != 1 || claims.retryCalls[0] != "10000000-0000-0000-0000-000000000001/11111111-1111-1111-1111-111111111111/7" {
		t.Fatalf("RetryEnter calls = %v", claims.retryCalls)
	}
	if len(rel.released) != 1 || rel.released[0] != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("dead sandbox not released: %v", rel.released)
	}
}

// TestDeathOutsideBudgetFails: no retry budget left ⇒ terminal FailEnter.
func TestDeathOutsideBudgetFails(t *testing.T) {
	run := newTestRun("11111111-1111-1111-1111-111111111111", "10000000-0000-0000-0000-000000000001")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).Build()
	claims := &fakeClaims{
		found: true,
		state: ClaimState{Step: reconcile.StepRunning, Fence: 2, Holder: "a",
			LeaseExpiresAt: leaseAgo(time.Minute)},
		laps: 2, failOK: true, // MaxRetries nil ⇒ 0 budget, laps 2 ≥ 0
	}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{})
	d.Sandbox = &fakeReleaser{}

	rq, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"})
	if err != nil || rq != 0 {
		t.Fatalf("fail-enter drive: rq=%v err=%v", rq, err)
	}
	if !claims.failCall {
		t.Fatal("budget exhausted death must FailEnter")
	}
}

// TestDeathRetryRacedRequeuesShort: a lost RetryEnter race (fence moved)
// requeues short to re-read the world.
func TestDeathRetryRacedRequeuesShort(t *testing.T) {
	max := int32(3)
	run := newTestRun("11111111-1111-1111-1111-111111111111", "10000000-0000-0000-0000-000000000001")
	run.Spec.RetryPolicy = &api.RetryPolicy{MaxRetries: &max}
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).Build()
	claims := &fakeClaims{
		found: true,
		state: ClaimState{Step: reconcile.StepCollecting, Fence: 1, Holder: "a",
			LeaseExpiresAt: leaseAgo(time.Second)},
		retryOK: false, // someone else reclaimed first
	}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{})

	rq, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"})
	if err != nil {
		t.Fatalf("raced retry: %v", err)
	}
	if rq != continueDelay {
		t.Fatalf("requeue = %v, want %v", rq, continueDelay)
	}
}

// TestDeathNotDetectedWithoutExpiry: unheld or unexpired leases never trigger
// the death path — the machine just drives.
func TestDeathNotDetectedWithoutExpiry(t *testing.T) {
	run := newTestRun("11111111-1111-1111-1111-111111111111", "10000000-0000-0000-0000-000000000001")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).Build()

	future := time.Now().Add(time.Hour)
	uid := "11111111-1111-1111-1111-111111111111"
	for _, cs := range []ClaimState{
		{Step: reconcile.StepRunning, Fence: 1, Holder: "ksquad-operator", RunID: uid,
			LeaseExpiresAt: &future, ItemState: "in_progress"}, // ours + live: renew
		{Step: reconcile.StepRunning, Fence: 1, ItemState: "todo"},                                                   // unheld: acquire
		{Step: reconcile.StepPending, Fence: 1, Holder: "a", LeaseExpiresAt: leaseAgo(time.Hour), ItemState: "todo"}, // not in flight: re-acquire
	} {
		claims := &fakeClaims{found: true, state: cs, acquireOK: true, acquireFence: 1, renewOK: true}
		store := &fakeMachineStore{step: cs.Step, fence: 1, advanceOK: true}
		d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: &fakeMachineEffects{}})
		if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err != nil {
			t.Fatalf("drive(%+v): %v", cs, err)
		}
		if len(claims.retryCalls) != 0 || claims.failCall {
			t.Fatalf("death path fired for %+v", cs)
		}
	}
}

// TestBackoffForPolicyAndEnvelope: the retry delay honours the spec base and
// stays inside the equal-jitter envelope [exp/2, exp].
func TestBackoffForPolicyAndEnvelope(t *testing.T) {
	secs := int32(10)
	run := newTestRun("u", "w")
	run.Spec.RetryPolicy = &api.RetryPolicy{BackoffSeconds: &secs}
	d := newDriver(nil, &fakeClaims{}, &fakePauses{}, &fakeRunner{})

	d.Rand = func() float64 { return 0 } // floor
	if got := d.BackoffFor(run, 1); got != 5*time.Second {
		t.Fatalf("backoff floor = %v, want 5s", got)
	}
	d.Rand = func() float64 { return 1 } // ceiling
	if got := d.BackoffFor(run, 1); got != 10*time.Second {
		t.Fatalf("backoff ceiling = %v, want 10s", got)
	}
	// default base (1s), attempt 1: [500ms, 1s]
	if got := d.BackoffFor(newTestRun("u", "w"), 1); got != time.Second {
		t.Fatalf("default-base ceiling = %v, want 1s", got)
	}
	// exponential growth is capped
	d.Rand = func() float64 { return 0 }
	if got := d.BackoffFor(run, 60); got != backoffCap/2 {
		t.Fatalf("capped backoff = %v, want %v", got, backoffCap/2)
	}
}

// TestMaxRetriesPolicyTable pins the budget reader.
func TestMaxRetriesPolicyTable(t *testing.T) {
	if got := maxRetries(newTestRun("u", "w")); got != 0 {
		t.Fatalf("nil policy budget = %d, want 0", got)
	}
	zero := int32(0)
	r := newTestRun("u", "w")
	r.Spec.RetryPolicy = &api.RetryPolicy{MaxRetries: &zero}
	if got := maxRetries(r); got != 0 {
		t.Fatalf("explicit-zero budget = %d, want 0", got)
	}
	three := int32(3)
	r.Spec.RetryPolicy.MaxRetries = &three
	if got := maxRetries(r); got != 3 {
		t.Fatalf("budget = %d, want 3", got)
	}
}

// ---------------------------------------------------------------------------
// Rate-limit park + resume (3.7)
// ---------------------------------------------------------------------------

// TestParkRecordsEpisodeAndNotifies: a Run parked on paused(rate_limited)
// with no pending episode gets exactly one recorded (nil Retry-After ⇒ the
// backoff policy) and the timer is kicked.
func TestParkRecordsEpisodeAndNotifies(t *testing.T) {
	run := newTestRun("11111111-1111-1111-1111-111111111111", "10000000-0000-0000-0000-000000000001")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).Build()
	claims := &fakeClaims{found: true, state: ClaimState{Step: reconcile.StepPausedRateLimited, Fence: 1}}
	pauses := &fakePauses{}
	d := newDriver(cl, claims, pauses, &fakeRunner{})
	notified := false
	d.Notify = func() { notified = true }

	rq, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"})
	if err != nil || rq != 0 {
		t.Fatalf("park: rq=%v err=%v — the timer owns the wake, no requeue poll", rq, err)
	}
	if pauses.recordCalls != 1 || len(pauses.recorded) != 1 {
		t.Fatalf("episode records = %d, want 1", pauses.recordCalls)
	}
	if !notified {
		t.Fatal("timer not notified of the new episode")
	}
}

// TestParkKeepsExistingEpisode: a pending episode is never re-recorded (its
// wake is already durable).
func TestParkKeepsExistingEpisode(t *testing.T) {
	run := newTestRun("11111111-1111-1111-1111-111111111111", "10000000-0000-0000-0000-000000000001")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).Build()
	claims := &fakeClaims{found: true, state: ClaimState{Step: reconcile.StepPaused, Fence: 1}}
	pauses := &fakePauses{pendingHas: true, pendingAt: time.Now().Add(time.Minute)}
	d := newDriver(cl, claims, pauses, &fakeRunner{})
	d.Notify = func() { t.Fatal("no new episode ⇒ no notify") }

	if rq, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err != nil || rq != 0 {
		t.Fatalf("park(existing): rq=%v err=%v", rq, err)
	}
	if pauses.recordCalls != 0 {
		t.Fatalf("existing episode re-recorded (%d)", pauses.recordCalls)
	}
}

// TestOnResumeDueRequeuesAndKicks: a claimed due wake re-enters dispatching
// and enqueues the owning Run through the resume channel.
func TestOnResumeDueRequeuesAndKicks(t *testing.T) {
	run := newTestRun("11111111-1111-1111-1111-111111111111", "10000000-0000-0000-0000-000000000001")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).WithIndex(&api.Run{}, workItemField,
		func(obj client.Object) []string { return []string{obj.(*api.Run).Spec.WorkItemRef} }).Build()
	claims := &fakeClaims{requeueOK: true}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{})

	d.OnResumeDue(context.Background(), []coord.ProdDuePause{{WorkItemID: "10000000-0000-0000-0000-000000000001", RunID: "11111111-1111-1111-1111-111111111111"}})

	if !claims.requeueCall {
		t.Fatal("due wake did not re-enter dispatching")
	}
	select {
	case ev := <-d.resumeCh:
		if ev.Object.GetName() != "run-1" {
			t.Fatalf("kicked wrong run: %v", ev.Object.GetName())
		}
	default:
		t.Fatal("resume kick not delivered")
	}
}

// TestOnResumeDueLostRaceIsQuiet: a requeue that lost its guard (already moved)
// does not kick.
func TestOnResumeDueLostRaceIsQuiet(t *testing.T) {
	run := newTestRun("11111111-1111-1111-1111-111111111111", "10000000-0000-0000-0000-000000000001")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).WithIndex(&api.Run{}, workItemField,
		func(obj client.Object) []string { return []string{obj.(*api.Run).Spec.WorkItemRef} }).Build()
	claims := &fakeClaims{requeueOK: false}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{})

	d.OnResumeDue(context.Background(), []coord.ProdDuePause{{WorkItemID: "10000000-0000-0000-0000-000000000001"}})

	select {
	case <-d.resumeCh:
		t.Fatal("lost requeue must not kick")
	default:
	}
}

// TestKickWorkItemChannelFullDrops: a full resume channel drops (the resync
// backstop owns catch-up) instead of blocking the wake.
func TestKickWorkItemChannelFullDrops(t *testing.T) {
	run := newTestRun("11111111-1111-1111-1111-111111111111", "10000000-0000-0000-0000-000000000001")
	run2 := newTestRun("22222222-2222-2222-2222-222222222222", "10000000-0000-0000-0000-000000000001")
	run2.Name = "run-2"
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run, run2).WithIndex(&api.Run{}, workItemField,
		func(obj client.Object) []string { return []string{obj.(*api.Run).Spec.WorkItemRef} }).Build()
	d := newDriver(cl, &fakeClaims{}, &fakePauses{}, &fakeRunner{})
	for i := 0; i < cap(d.resumeCh); i++ { // fill it
		d.resumeCh <- event.TypedGenericEvent[client.Object]{Object: run}
	}
	done := make(chan struct{})
	go func() { d.kickWorkItem(context.Background(), "10000000-0000-0000-0000-000000000001"); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("kickWorkItem blocked on a full channel")
	}
}

// ---------------------------------------------------------------------------
// Edge shapes
// ---------------------------------------------------------------------------

// TestDriverSkipsDeletingAndReflessRuns.
func TestDriverSkipsDeletingAndReflessRuns(t *testing.T) {
	now := metav1.Now()
	deleting := newTestRun("u1", "10000000-0000-0000-0000-000000000001")
	deleting.Name = "dying"
	deleting.Finalizers = []string{"ksquad.io/teardown"} // fake client refuses deletionTimestamp without one
	deleting.DeletionTimestamp = &now
	refless := newTestRun("u2", "")
	refless.Name = "refless"
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(deleting, refless).Build()
	claims := &fakeClaims{found: true, state: ClaimState{Step: reconcile.StepPending}}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: &fakeMachineStore{}, effects: &fakeMachineEffects{}})

	for _, name := range []string{"dying", "refless"} {
		if rq, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: name}); err != nil || rq != 0 {
			t.Fatalf("%s: rq=%v err=%v", name, rq, err)
		}
	}
}

// TestDriverMissingRunIsNotAnError.
func TestDriverMissingRunIsNotAnError(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	d := newDriver(cl, &fakeClaims{}, &fakePauses{}, &fakeRunner{})
	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "ghost"}); err != nil {
		t.Fatalf("missing run: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ISI-4435: follow-settlement drive guard
// ---------------------------------------------------------------------------

// fakeFollowSettle is a FollowSettleReader stub: it reports one fixed latest-lap
// settlement for every run.
type fakeFollowSettle struct {
	dispatched bool
	settled    bool
	outcome    string
	err        error
	calls      int
}

func (f *fakeFollowSettle) RunFollowOutcome(context.Context, string) (bool, bool, string, error) {
	f.calls++
	return f.dispatched, f.settled, f.outcome, f.err
}

// TestFollowSucceededSkipsDeathPath: a Run parked in-flight (collecting) whose a2a
// follow durably settled SUCCEEDED must NOT be treated as a death even though its
// lease lapsed (the operator was down while the agent finished) — the sandbox pod
// OnDone tears down at completion is the expected end state. The drive re-acquires
// and finalizes the machine to succeeded instead of entering the retry lap.
func TestFollowSucceededSkipsDeathPath(t *testing.T) {
	uid := "11111111-1111-1111-1111-111111111111"
	run := newTestRun(uid, "10000000-0000-0000-0000-000000000001")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).Build()
	claims := &fakeClaims{found: true, state: ClaimState{
		Step: reconcile.StepCollecting, Fence: 7, Holder: "ksquad-operator", RunID: uid,
		LeaseExpiresAt: leaseAgo(time.Minute), ItemState: "in_progress"},
		acquireOK: true, acquireFence: 8, laps: 1, retryOK: true}
	store := &fakeMachineStore{step: reconcile.StepCollecting, fence: 7, advanceOK: true}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: &fakeMachineEffects{}})
	d.Sandbox = &fakeReleaser{}
	d.Settle = &fakeFollowSettle{dispatched: true, settled: true, outcome: coord.SettleOutcomeSucceeded}

	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err != nil {
		t.Fatalf("settled-succeeded drive: %v", err)
	}
	if len(claims.retryCalls) != 0 || claims.failCall {
		t.Fatalf("settled-succeeded run entered the death path: retry=%v fail=%v", claims.retryCalls, claims.failCall)
	}
	if store.step != reconcile.StepSucceeded {
		t.Fatalf("durable step = %q, want succeeded", store.step)
	}
}

// TestFollowFailedStillTakesDeathPath: a settled-but-FAILED follow is NOT
// settledOK, so an expired lease still routes through the retry lap — the guard
// protects only the succeeded outcome, never masking a real failure.
func TestFollowFailedStillTakesDeathPath(t *testing.T) {
	uid := "11111111-1111-1111-1111-111111111111"
	run := newTestRun(uid, "10000000-0000-0000-0000-000000000001")
	max := int32(3)
	run.Spec.RetryPolicy = &api.RetryPolicy{MaxRetries: &max}
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).Build()
	claims := &fakeClaims{found: true, state: ClaimState{
		Step: reconcile.StepCollecting, Fence: 7, Holder: "ksquad-operator", RunID: uid,
		LeaseExpiresAt: leaseAgo(time.Minute), ItemState: "in_progress"},
		laps: 1, retryOK: true, retryNewFence: 8}
	store := &fakeMachineStore{step: reconcile.StepCollecting, fence: 7, advanceOK: true}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: &fakeMachineEffects{}})
	d.Sandbox = &fakeReleaser{}
	d.Settle = &fakeFollowSettle{dispatched: true, settled: true, outcome: coord.SettleOutcomeFailed}

	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err != nil {
		t.Fatalf("settled-failed drive: %v", err)
	}
	if len(claims.retryCalls) != 1 {
		t.Fatalf("settled-failed run did not enter the retry lap: retry=%v", claims.retryCalls)
	}
}
