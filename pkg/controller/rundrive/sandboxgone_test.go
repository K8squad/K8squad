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
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/reconcile"
)

// ISI-4310: a Run whose bound sandbox pod is NotFound mid-flight must leave
// the endless dispatch-retry loop through the death path — teardown + marker
// clear + status ref clear, then the §8 retry lap (or terminal failure when
// the retry budget is spent).

const (
	goneRunUID = "33333333-3333-3333-3333-333333333333"
	gonePod    = "sandbox-62dc3c9a459b6f43"
)

// fakeBindClearer records ClearSandboxBind calls (the durable marker seam).
type fakeBindClearer struct {
	cleared []string
	err     error
}

func (f *fakeBindClearer) ClearSandboxBind(_ context.Context, runID string) error {
	f.cleared = append(f.cleared, runID)
	return f.err
}

// goneSandboxFixture builds a driver whose Run is in flight at dispatching,
// holds its checkout with a LIVE lease (so the §5.3 lease-death detector must
// NOT fire — that is exactly the ISI-4310 shape the HeartbeatSweeper keeps
// renewed), and whose status.sandboxRef names a pod that does not exist.
func goneSandboxFixture(t *testing.T, mutate func(*api.Run, *fakeClaims)) (*Driver, *fakeClaims, *fakeReleaser, *fakeBindClearer, client.Client) {
	t.Helper()
	run := newTestRun(goneRunUID, "10000000-0000-0000-0000-000000000001")
	run.Status.SandboxRef = &api.ObjectRef{Name: gonePod, Namespace: "default"}
	claims := &fakeClaims{
		found: true,
		state: ClaimState{
			Step:   reconcile.StepDispatching,
			Fence:  5,
			Holder: "ksquad-operator",
			RunID:  goneRunUID,
			// Live lease: the whole point is that dead() alone never fires.
			LeaseExpiresAt: ptrTime(time.Now().Add(10 * time.Minute)),
			ItemState:      "in_progress",
		},
		laps:    0,
		renewOK: true,
		retryOK: true, retryNewFence: 6,
		failOK: true,
	}
	if mutate != nil {
		mutate(run, claims)
	}
	cl := fake.NewClientBuilder().
		WithScheme(goneScheme(t)).
		WithObjects(run).
		WithStatusSubresource(&api.Run{}).
		WithIndex(&api.Run{}, workItemField,
			func(obj client.Object) []string { return []string{obj.(*api.Run).Spec.WorkItemRef} }).
		Build()
	releaser := &fakeReleaser{}
	clearer := &fakeBindClearer{}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{
		store:   &fakeMachineStore{step: reconcile.StepDispatching, fence: 5, advanceOK: true},
		effects: &fakeMachineEffects{},
	})
	d.Sandbox = releaser
	d.BindClear = clearer
	return d, claims, releaser, clearer, cl
}

func goneScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := newScheme(t)
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	return s
}

func ptrTime(t time.Time) *time.Time { return &t }

func ptrInt32(v int32) *int32 { return &v }

// The gone pod routes through the death path: pool release, durable marker
// clear, status.sandboxRef nulled, and a §6.3 fence-first RetryEnter — within
// the retry budget, with backoff requeue.
func TestGoneSandboxEntersRetryLap(t *testing.T) {
	d, claims, releaser, clearer, cl := goneSandboxFixture(t, func(run *api.Run, _ *fakeClaims) {
		run.Spec.RetryPolicy = &api.RetryPolicy{MaxRetries: ptrInt32(1)}
	})

	rq, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"})
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if rq <= 0 {
		t.Fatalf("requeue = %v, want backoff > 0 (retry lap)", rq)
	}
	if len(claims.retryCalls) != 1 || claims.retryCalls[0] != "10000000-0000-0000-0000-000000000001/"+goneRunUID+"/5" {
		t.Fatalf("RetryEnter calls = %v, want one fence-first re-entry at fence 5", claims.retryCalls)
	}
	if claims.failCall {
		t.Fatal("FailEnter must not fire while the retry budget holds")
	}
	if len(releaser.released) != 1 || releaser.released[0] != goneRunUID {
		t.Fatalf("Sandbox.Release calls = %v, want [%s]", releaser.released, goneRunUID)
	}
	if len(clearer.cleared) != 1 || clearer.cleared[0] != goneRunUID {
		t.Fatalf("ClearSandboxBind calls = %v, want [%s]", clearer.cleared, goneRunUID)
	}
	var after api.Run
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "run-1"}, &after); err != nil {
		t.Fatalf("re-read run: %v", err)
	}
	if after.Status.SandboxRef != nil {
		t.Fatalf("status.sandboxRef = %+v, want nil (stale ref cleared)", after.Status.SandboxRef)
	}
}

// Outside the retry budget the gone pod is terminal: FailEnter, no retry lap.
func TestGoneSandboxFailsTerminallyWhenBudgetSpent(t *testing.T) {
	d, claims, _, clearer, _ := goneSandboxFixture(t, func(run *api.Run, fc *fakeClaims) {
		run.Spec.RetryPolicy = &api.RetryPolicy{MaxRetries: ptrInt32(1)}
		fc.laps = 1 // budget spent
	})

	rq, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"})
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if rq != 0 {
		t.Fatalf("requeue = %v, want 0 (terminal)", rq)
	}
	if !claims.failCall {
		t.Fatal("FailEnter must fire when the retry budget is spent")
	}
	if len(claims.retryCalls) != 0 {
		t.Fatalf("RetryEnter calls = %v, want none", claims.retryCalls)
	}
	if len(clearer.cleared) != 1 {
		t.Fatalf("ClearSandboxBind calls = %v, want exactly one (cleanup happens either way)", clearer.cleared)
	}
}

// A pod that still exists never triggers the death path: the machine drives.
func TestLiveSandboxPodDrivesNormally(t *testing.T) {
	d, claims, releaser, clearer, cl := goneSandboxFixture(t, nil)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: gonePod, Namespace: "default"},
		Status: corev1.PodStatus{PodIP: "10.0.0.9"}}
	if err := cl.Create(context.Background(), pod); err != nil {
		t.Fatalf("seed pod: %v", err)
	}

	rq, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"})
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if len(claims.retryCalls) != 0 || claims.failCall {
		t.Fatalf("death path fired on a live pod: retry=%v fail=%v", claims.retryCalls, claims.failCall)
	}
	if len(releaser.released) != 0 || len(clearer.cleared) != 0 {
		t.Fatalf("teardown fired on a live pod: released=%v cleared=%v", releaser.released, clearer.cleared)
	}
	_ = rq // the machine may requeue (non-terminal) — only the death path matters here
	var after api.Run
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "run-1"}, &after); err != nil {
		t.Fatalf("re-read run: %v", err)
	}
	if after.Status.SandboxRef == nil || after.Status.SandboxRef.Name != gonePod {
		t.Fatalf("status.sandboxRef = %+v, want untouched (%s)", after.Status.SandboxRef, gonePod)
	}
}

// No bound ref (pre-bind ordering, sandbox-less lanes): no death path.
func TestGoneSandboxSkippedWithoutRef(t *testing.T) {
	d, claims, _, clearer, _ := goneSandboxFixture(t, func(run *api.Run, _ *fakeClaims) {
		run.Status.SandboxRef = nil
	})

	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if len(claims.retryCalls) != 0 || claims.failCall || len(clearer.cleared) != 0 {
		t.Fatalf("death path fired without a sandboxRef: retry=%v fail=%v cleared=%v",
			claims.retryCalls, claims.failCall, clearer.cleared)
	}
}

// A checkout held by a FOREIGN run is never reclaimed by the gone-sandbox
// path — that holder's own driver owns the death handling.
func TestGoneSandboxSkippedWhenCheckoutForeign(t *testing.T) {
	d, claims, releaser, clearer, cl := goneSandboxFixture(t, func(_ *api.Run, fc *fakeClaims) {
		fc.state.RunID = "44444444-4444-4444-4444-444444444444"
		fc.state.ItemState = "done" // no custody path: the drive absorbs
	})

	if rq, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err != nil || rq != 0 {
		t.Fatalf("foreign-checkout drive: rq=%v err=%v, want 0/nil (absorbed)", rq, err)
	}
	if len(claims.retryCalls) != 0 || claims.failCall || len(releaser.released) != 0 || len(clearer.cleared) != 0 {
		t.Fatalf("death path fired on a foreign checkout: retry=%v fail=%v released=%v cleared=%v",
			claims.retryCalls, claims.failCall, releaser.released, clearer.cleared)
	}
	var after api.Run
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "run-1"}, &after); err != nil {
		t.Fatalf("re-read run: %v", err)
	}
	if after.Status.SandboxRef == nil {
		t.Fatal("status.sandboxRef must be untouched when the checkout is foreign")
	}
}

// At claiming_sandbox the gone-sandbox check does not fire (the reattach bind
// advances the machine first); the FOLLOWING pass at dispatching catches it —
// one bounded extra pass, never a wedged claim.
func TestGoneSandboxSkippedAtClaimingStep(t *testing.T) {
	d, claims, _, clearer, _ := goneSandboxFixture(t, func(_ *api.Run, fc *fakeClaims) {
		fc.state.Step = reconcile.StepClaimingSandbox
	})
	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if len(claims.retryCalls) != 0 || claims.failCall || len(clearer.cleared) != 0 {
		t.Fatalf("death path fired at claiming_sandbox: retry=%v fail=%v cleared=%v",
			claims.retryCalls, claims.failCall, clearer.cleared)
	}
}

// A ClearSandboxBind infrastructure failure surfaces as a reconcile error
// (controller-runtime backoff); the next pass re-detects and redoes — the
// recovery is idempotent, so no state is lost.
func TestGoneSandboxClearErrorSurfaces(t *testing.T) {
	d, _, _, clearer, _ := goneSandboxFixture(t, func(run *api.Run, _ *fakeClaims) {
		run.Spec.RetryPolicy = &api.RetryPolicy{MaxRetries: ptrInt32(1)}
	})
	clearer.err = errBoom

	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err == nil {
		t.Fatal("marker-clear failure must surface as a reconcile error")
	}
}

var errBoom = &boomErr{}

type boomErr struct{}

func (*boomErr) Error() string { return "db down" }

// The durable marker clear: DELETE + sandbox_bind_cleared audit row in ONE
// transaction; a Run with no marker is an idempotent no-op (no audit row).
func TestProdClaimsClearSandboxBind(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c := NewProdClaims(db, OperatorPrincipal)

	// First call: the marker exists — delete it, audit it, commit.
	mock.ExpectBegin()
	mock.ExpectQuery("DELETE FROM coord.sandbox_bind").
		WithArgs(goneRunUID).
		WillReturnRows(sqlmock.NewRows([]string{"work_item_id", "sandbox_ref"}).
			AddRow("11111111-1111-1111-1111-111111111111", gonePod))
	mock.ExpectExec("INSERT INTO coord.audit_log").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	// Second call: no marker — commit the no-op, write NO audit row.
	mock.ExpectBegin()
	mock.ExpectQuery("DELETE FROM coord.sandbox_bind").
		WithArgs(goneRunUID).
		WillReturnRows(sqlmock.NewRows([]string{"work_item_id", "sandbox_ref"}))
	mock.ExpectCommit()

	if err := c.ClearSandboxBind(context.Background(), goneRunUID); err != nil {
		t.Fatalf("clear (marker present): %v", err)
	}
	if err := c.ClearSandboxBind(context.Background(), goneRunUID); err != nil {
		t.Fatalf("clear (idempotent no-op): %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
	if err := c.ClearSandboxBind(context.Background(), ""); err != nil {
		t.Fatalf("empty runID must be a silent no-op, got %v", err)
	}
}
