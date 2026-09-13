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
	"errors"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/reconcile"
)

// ---------------------------------------------------------------------------
// §6.2 claim gate (ISI-4183): a Run drives a work item only while holding
// its checkout. These are the unit proofs of the half the k8squad-test E2E
// run found missing (ticket dispatched, Run succeeded, lane stuck in todo).
// ---------------------------------------------------------------------------

const gateRunUID = "11111111-1111-1111-1111-111111111111"

func gateHarness(t *testing.T, claims *fakeClaims, store *fakeMachineStore) (*Driver, client.ObjectKey) {
	t.Helper()
	run := newTestRun(gateRunUID, "51510000-0000-0000-0000-000000000e7e")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).
		WithIndex(&api.Run{}, workItemField,
			func(obj client.Object) []string { return []string{obj.(*api.Run).Spec.WorkItemRef} }).
		Build()
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: &fakeMachineEffects{}})
	return d, client.ObjectKeyFromObject(run)
}

// The M1.3 contract: an enrolled todo item (unheld, pending) is ACQUIRED
// first — the todo→in_progress lane advance is the claimer's co-committed
// write — and only then driven.
func TestClaimGateAcquiresThenDrives(t *testing.T) {
	claims := &fakeClaims{found: true, state: ClaimState{Step: reconcile.StepPending, Fence: 0, ItemState: "todo"},
		acquireOK: true, acquireFence: 1}
	store := &fakeMachineStore{step: reconcile.StepPending, fence: 1, advanceOK: true}
	d, key := gateHarness(t, claims, store)

	if _, err := runOnce(t, d, key); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if len(claims.acquireCalls) != 1 || claims.acquireCalls[0] != "51510000-0000-0000-0000-000000000e7e/"+gateRunUID {
		t.Fatalf("acquire calls = %v, want exactly [51510000-0000-0000-0000-000000000e7e/%s]", claims.acquireCalls, gateRunUID)
	}
	if store.step != reconcile.StepSucceeded {
		t.Fatalf("durable step = %q, want succeeded (the drive follows the acquire)", store.step)
	}
}

// A guard refusal (a live foreign lease appeared, or the item left the todo
// lane) parks the Run on a bounded requeue — never a claim-less drive.
func TestClaimGateRefusalRequeuesWithoutDriving(t *testing.T) {
	claims := &fakeClaims{found: true, state: ClaimState{Step: reconcile.StepPending, Fence: 0, ItemState: "todo"},
		acquireOK: false}
	store := &fakeMachineStore{step: reconcile.StepPending, fence: 0, advanceOK: true}
	d, key := gateHarness(t, claims, store)

	rq, err := runOnce(t, d, key)
	if err != nil {
		t.Fatalf("a guard refusal must not error: %v", err)
	}
	if rq == 0 {
		t.Fatal("a guard refusal must requeue a bounded step, not stop")
	}
	if store.step != reconcile.StepPending {
		t.Fatalf("unclaimed item was driven to %q — the gate must hold", store.step)
	}
}

// An acquire infrastructure failure is an honest controller error, never a
// silent claim-less drive.
func TestClaimGateAcquireErrorSurfaces(t *testing.T) {
	claims := &fakeClaims{found: true, acquireErr: errors.New("conn refused"),
		state: ClaimState{Step: reconcile.StepPending, Fence: 0, ItemState: "todo"}}
	store := &fakeMachineStore{step: reconcile.StepPending, fence: 0, advanceOK: true}
	d, key := gateHarness(t, claims, store)

	if _, err := runOnce(t, d, key); err == nil {
		t.Fatal("the acquire error must surface as the reconcile error")
	}
	if store.step != reconcile.StepPending {
		t.Fatalf("durable step = %q, want pending", store.step)
	}
}

// A checkout already recorded under THIS run with a live lease (the crash
// window between the acquire commit and the machine advance, and every
// steady mid-run pass) RENEWS — the §6.2 heartbeat — and drives on WITHOUT
// re-acquiring: the gate is idempotent.
func TestClaimGateHeldBySelfRenewsWithoutReacquire(t *testing.T) {
	lease := time.Now().Add(time.Minute)
	claims := &fakeClaims{found: true, renewOK: true, state: ClaimState{
		Step: reconcile.StepClaimingSandbox, Fence: 4,
		Holder: OperatorPrincipal, RunID: gateRunUID, LeaseExpiresAt: &lease,
		ItemState: "in_progress"}}
	store := &fakeMachineStore{step: reconcile.StepClaimingSandbox, fence: 4, advanceOK: true}
	d, key := gateHarness(t, claims, store)

	if _, err := runOnce(t, d, key); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if len(claims.acquireCalls) != 0 {
		t.Fatalf("a self-held checkout must not re-acquire; calls = %v", claims.acquireCalls)
	}
	if claims.renewCalls != 1 {
		t.Fatalf("renew calls = %d, want 1 (the lease heartbeat)", claims.renewCalls)
	}
	if store.step != reconcile.StepSucceeded {
		t.Fatalf("durable step = %q, want succeeded", store.step)
	}
}

// A LIVE foreign lease owns the machine: the acquire attempt is made (the
// free-or-expired guard lives in the claimer, not the driver) and REFUSED —
// our Run requeues a bounded step, never a claim-less drive, never a steal.
func TestClaimGateForeignLiveLeaseWaits(t *testing.T) {
	lease := time.Now().Add(time.Minute)
	claims := &fakeClaims{found: true, state: ClaimState{
		Step: reconcile.StepClaimingSandbox, Fence: 4,
		Holder: OperatorPrincipal, RunID: "99999999-9999-9999-9999-999999999999",
		LeaseExpiresAt: &lease, ItemState: "in_progress"}, acquireOK: false}
	store := &fakeMachineStore{step: reconcile.StepClaimingSandbox, fence: 4, advanceOK: true}
	d, key := gateHarness(t, claims, store)

	rq, err := runOnce(t, d, key)
	if err != nil {
		t.Fatalf("contention must not error: %v", err)
	}
	if rq == 0 {
		t.Fatal("contention must requeue a bounded step")
	}
	if len(claims.acquireCalls) != 1 {
		t.Fatalf("the acquire must be attempted (guard inside); calls = %v", claims.acquireCalls)
	}
	if store.step != reconcile.StepClaimingSandbox {
		t.Fatalf("a foreign-held item was driven to %q — the gate must hold", store.step)
	}
}

// A LAPSED foreign lease is a fenced zombie: the gate attempts the acquire
// (whose free-or-expired guard admits it) and drives when it lands.
func TestClaimGateLapsedForeignLeaseIsAcquirable(t *testing.T) {
	claims := &fakeClaims{found: true, state: ClaimState{
		Step: reconcile.StepClaimingSandbox, Fence: 4,
		Holder: "gone-run", RunID: "99999999-9999-9999-9999-999999999999",
		LeaseExpiresAt: ptrTimeAgo(time.Minute), ItemState: "in_progress"},
		acquireOK: true, acquireFence: 5}
	store := &fakeMachineStore{step: reconcile.StepClaimingSandbox, fence: 4, advanceOK: true}
	d, key := gateHarness(t, claims, store)

	if _, err := runOnce(t, d, key); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if len(claims.acquireCalls) != 1 {
		t.Fatalf("a lapsed foreign lease must be acquired over; calls = %v", claims.acquireCalls)
	}
	if store.step != reconcile.StepSucceeded {
		t.Fatalf("durable step = %q, want succeeded", store.step)
	}
}

func ptrTimeAgo(d time.Duration) *time.Time {
	t := time.Now().Add(-d)
	return &t
}
