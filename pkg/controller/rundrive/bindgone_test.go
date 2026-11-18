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
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/reconcile"
)

// ISI-7538 regression: an unsettled lap whose sandbox pod died mid-run
// (node loss, eviction, restart reap) leaves the drive at
// claiming_sandbox with no status ref, and the machine bind re-queues the
// same "sandbox pod not found" error forever — the run never terminalizes
// and its BYO endpoint slot stays held by a corpse holder that can never
// settle. The sticky errSandboxGone surfaced by the bind effect must route
// through the death path: stale bind cleared, sandbox teardown,
// fence-first RetryEnter within the budget — never the generic requeue
// loop.

// bindGoneFixture builds the live-wedge shape: a Run parked at
// claiming_sandbox (the journal flap across the operator crash regressed
// running back to claiming), holding its checkout with a LIVE lease (the
// HeartbeatSweeper keeps renewing it), no status.sandboxRef (never
// observed — so the ISI-4310 gone-pod probe cannot fire), and a machine
// whose bind effect carries the sticky gone-sandbox sentinel: exactly
// what ProdEffects.BindSandbox surfaces when the credential writer cannot
// place the task-io Secret on a deleted pod.
func bindGoneFixture(t *testing.T, mutate func(*api.Run, *fakeClaims)) (*Driver, *fakeClaims, *fakeReleaser, *fakeBindClearer) {
	t.Helper()
	run := newTestRun(goneRunUID, "10000000-0000-0000-0000-000000000001")
	run.Status.SandboxRef = nil
	claims := &fakeClaims{
		found: true,
		state: ClaimState{
			Step:   reconcile.StepClaimingSandbox,
			Fence:  5,
			Holder: "ksquad-operator",
			RunID:  goneRunUID,
			// Live lease: the 3.2 lease-death detector must NOT fire — the
			// bind sentinel alone owns this death route.
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
	store := &fakeMachineStore{step: reconcile.StepClaimingSandbox, fence: 5, advanceOK: true}
	effects := &fakeMachineEffects{err: errSandboxGone}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: effects})
	d.Sandbox = releaser
	d.BindClear = clearer
	return d, claims, releaser, clearer
}

// Within the retry budget the bind-time gone sandbox enters the death
// path: marker cleared, sandbox released for the retry lap, fence-first
// RetryEnter at the observed fence — and NO error to controller-runtime
// (the generic requeue loop is the bug).
func TestBindGoneSandboxAtClaimingEntersDeathPath(t *testing.T) {
	d, claims, releaser, clearer := bindGoneFixture(t, func(run *api.Run, _ *fakeClaims) {
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
}

// Outside the retry budget the corpse terminalizes (FailEnter) — which is
// what releases the endpoint and agent slots and bounds the queue stall.
func TestBindGoneSandboxFailsTerminallyWhenBudgetSpent(t *testing.T) {
	d, claims, _, _ := bindGoneFixture(t, func(run *api.Run, fc *fakeClaims) {
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
}
