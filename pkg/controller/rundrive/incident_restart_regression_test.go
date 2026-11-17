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
	"fmt"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/reconcile"
)

// ---------------------------------------------------------------------------
// ISI-7502 (ISI-7498 WS-C): the 16:28:48Z incident, end to end.
//
// The production failure: four seats dispatched concurrently against ONE BYO
// model endpoint that admits a single slot. One seat took the slot; the other
// three parked WaitingForEndpointSlot. Mid-wait the operator lost leader
// election, restarted, and reaped the running seat's sandbox pod. On the fresh
// operator's first reconcile pass EVERY seat terminal-Failed — the parked
// waiters were failed purely for waiting, and the reaped seat was failed for a
// transient infra death (its intake-minted Run carried no spec.retryPolicy, so
// the zero-retry default went straight to FailEnter).
//
// WS-A (ISI-7500, infra retry budget + failure cause) and WS-B (ISI-7501,
// restart-stable endpoint-slot FIFO) fix that. These tests pin the composed
// behaviour so the incident cannot regress: after a restart + sandbox die-off,
// every seat must end Succeeded, or honestly queued / WaitingForEndpointSlot —
// NEVER Failed purely for waiting, and NEVER Failed for a transient death.
// ---------------------------------------------------------------------------

// seat is one of the four concurrent dispatchers: its own Driver and Claims,
// all sharing the single-slot EndpointGate that is the real contention point.
type seat struct {
	name   string
	uid    string
	claims *fakeClaims
	store  *fakeMachineStore
	driver *Driver
}

const incidentEndpoint = "https://api.deepseek.com/v1"

// newIncidentSeat builds a seat parked at the claiming_sandbox edge (todo,
// acquirable) — the pre-bind endpoint-slot hold decides who drives and who
// queues. Settle is wired dispatched:false so that, should this seat take a
// death path, it is classified INFRA-class (the parked-waiter / reaped-sandbox
// shape), earning the WS-A retry budget rather than a zero-budget terminal fail.
func newIncidentSeat(name, uid, workItem string, cl client.Client, gate *EndpointGate) *seat {
	claims := &fakeClaims{found: true, state: ClaimState{
		Step: reconcile.StepClaimingSandbox, Fence: 1, Holder: "ksquad-operator",
		RunID: uid, ItemState: "todo"},
		acquireOK: true, acquireFence: 1,
		renewOK: true,
		retryOK: true, retryNewFence: 2,
		failOK:               true, // allowed, but the incident guard asserts it is never taken
		gateResumeDispatchOK: true,
	}
	store := &fakeMachineStore{step: reconcile.StepClaimingSandbox, fence: 1, advanceOK: true}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: &fakeMachineEffects{}})
	d.Sandbox = &fakeReleaser{}
	d.BindClear = &fakeBindClearer{}
	d.EndpointGate = gate
	d.EndpointLookup = &fakeEndpointLookup{endpoint: incidentEndpoint, max: 1}
	return &seat{name: name, uid: uid, claims: claims, store: store, driver: d}
}

func (s *seat) drive(t *testing.T) time.Duration {
	t.Helper()
	rq, err := runOnce(t, s.driver, types.NamespacedName{Namespace: "default", Name: s.name})
	if err != nil {
		t.Fatalf("%s drive: %v", s.name, err)
	}
	return rq
}

func (s *seat) reread(t *testing.T, cl client.Client) api.Run {
	t.Helper()
	var got api.Run
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: s.name}, &got); err != nil {
		t.Fatalf("re-read %s: %v", s.name, err)
	}
	return got
}

func (s *seat) parked(t *testing.T, cl client.Client) bool {
	t.Helper()
	got := s.reread(t, cl)
	cond := meta.FindStatusCondition(got.Status.Conditions, ConditionEndpointSlotWait)
	return cond != nil && cond.Status == metav1.ConditionTrue
}

// TestFourSeatSingleSlotRestartMidWaitNeverFailsSeats reproduces the full
// incident across the real driver seams and asserts the pass criteria.
func TestFourSeatSingleSlotRestartMidWaitNeverFailsSeats(t *testing.T) {
	// Four intake-minted Runs (nil RetryPolicy), one per seat, all BYO the same
	// single-slot endpoint. One fake client holds all four.
	runs := make([]*api.Run, 4)
	for i := range runs {
		r := newTestRun(
			fmt.Sprintf("aaaaaaaa-0000-0000-0000-00000000000%d", i+1),
			fmt.Sprintf("10000000-0000-0000-0000-00000000000%d", i+1),
		)
		r.Name = fmt.Sprintf("seat-%d", i+1)
		runs[i] = r
	}
	objs := make([]client.Object, len(runs))
	for i, r := range runs {
		objs[i] = r
	}
	cl := fake.NewClientBuilder().WithScheme(goneScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&api.Run{}).
		WithIndex(&api.Run{}, workItemField,
			func(obj client.Object) []string { return []string{obj.(*api.Run).Spec.WorkItemRef} }).
		Build()

	gate := NewEndpointGate(1)
	seats := make([]*seat, 4)
	for i := range seats {
		seats[i] = newIncidentSeat(runs[i].Name,
			string(runs[i].UID), runs[i].Spec.WorkItemRef, cl, gate)
	}

	// --- Phase 1: concurrent dispatch, single slot -------------------------
	// seat-1 wins the slot and drives; seat-2..4 park WaitingForEndpointSlot.
	if adv := seats[0].drive(t); adv != 0 {
		t.Fatalf("seat-1 should drive the admitted slot, not requeue on slot-wait (rq=%v)", adv)
	}
	if seats[0].store.advances == 0 {
		t.Fatal("the admitted seat-1 must drive the machine past the claiming edge")
	}
	if _, ok := gate.Acquire(incidentEndpoint, seats[0].uid, 1); !ok {
		t.Fatal("seat-1 must hold the only endpoint slot after driving")
	}
	for _, s := range seats[1:] {
		if rq := s.drive(t); rq != endpointSlotWaitDelay {
			t.Fatalf("%s must park at the slot-wait cadence (rq=%v, want %v)", s.name, rq, endpointSlotWaitDelay)
		}
		if s.store.advances != 0 {
			t.Fatalf("%s must not bind/drive past the busy slot (%d advances)", s.name, s.store.advances)
		}
		if !s.parked(t, cl) {
			t.Fatalf("%s must stamp WaitingForEndpointSlot=True while queued", s.name)
		}
		if s.claims.failCall {
			t.Fatalf("%s must NOT terminal-fail merely for waiting on the slot", s.name)
		}
	}

	// --- Phase 2: operator restart + sandbox die-off mid-wait --------------
	// The restart empties the in-process gate (a brand-new gate); reassign it
	// to every seat's driver, exactly as a fresh operator process would.
	gate = NewEndpointGate(1)
	for _, s := range seats {
		s.driver.EndpointGate = gate
	}

	// The running seat (seat-1) had its sandbox pod reaped by the dying
	// operator: its Run now carries a sandboxRef to a pod absent from the API,
	// its checkout is in-flight at dispatching with a still-live lease, and its
	// follow never settled failed (parked/transient, not a business failure).
	// This is a transient INFRA death — WS-A must spend a retry lap, not a seat.
	got1 := seats[0].reread(t, cl)
	got1.Status.SandboxRef = &api.ObjectRef{Namespace: "default", Name: gonePod}
	if err := cl.Status().Update(context.Background(), &got1); err != nil {
		t.Fatalf("stamp gone sandboxRef on seat-1: %v", err)
	}
	seats[0].claims.state = ClaimState{
		Step: reconcile.StepDispatching, Fence: 1, Holder: "ksquad-operator",
		RunID: seats[0].uid, LeaseExpiresAt: ptrTime(time.Now().Add(10 * time.Minute)),
		ItemState: "in_progress"}
	seats[0].store.step = reconcile.StepDispatching
	seats[0].driver.Settle = &fakeFollowSettle{dispatched: false} // never dispatched ⇒ infra, not business

	// Fresh operator's first pass. seat-1 (reaped sandbox) must take a retry
	// lap, never terminal-fail.
	seats[0].drive(t)
	if seats[0].claims.failCall {
		t.Fatal("seat-1: a transient sandbox die-off after restart must NOT terminal-fail the seat (ISI-7498 WS-A)")
	}
	if len(seats[0].claims.retryCalls) != 1 {
		t.Fatalf("seat-1: the infra death must spend a retry lap within the WS-A budget, got retry=%v", seats[0].claims.retryCalls)
	}

	// The queued seats re-drive on the fresh gate: the next in line takes the
	// freed slot and drives; the rest re-park. None may fail for waiting.
	seats[1].driver.Settle = &fakeFollowSettle{dispatched: false}
	if adv := seats[1].drive(t); adv != 0 {
		t.Fatalf("seat-2 should take the freed slot and drive after restart (rq=%v)", adv)
	}
	if seats[1].store.advances == 0 {
		t.Fatal("seat-2 must drive the machine once admitted on the fresh gate")
	}
	for _, s := range seats[2:] {
		s.driver.Settle = &fakeFollowSettle{dispatched: false}
		if rq := s.drive(t); rq != endpointSlotWaitDelay {
			t.Fatalf("%s must re-park behind seat-2 after restart (rq=%v)", s.name, rq)
		}
		if !s.parked(t, cl) {
			t.Fatalf("%s must keep WaitingForEndpointSlot=True after restart", s.name)
		}
	}

	// --- Phase 3: incident pass criteria -----------------------------------
	// The regression guard: on the fresh operator's pass NOT ONE seat was
	// terminal-failed — not the reaped runner, not any parked waiter.
	for _, s := range seats {
		if s.claims.failCall {
			t.Fatalf("%s terminal-failed on the post-restart pass — the 16:28:48Z regression (ISI-7498)", s.name)
		}
		got := s.reread(t, cl)
		if got.Status.Phase == api.RunPhaseFailed {
			t.Fatalf("%s reached RunPhaseFailed after restart — must be Succeeded/queued, never Failed for waiting or a transient death", s.name)
		}
	}
}

// TestSingleSlotFourSeatFIFOSurvivesRestart complements the gate-level
// TestEndpointGateFIFOSurvivesRestart (which exercises a 2-slot endpoint) with
// the incident's exact shape: ONE slot, FOUR seats. The FIFO key is each seat's
// durable wait-start (read off its WaitingForEndpointSlot condition), so an
// operator restart that empties the gate must not let a late re-registrant jump
// the queue — the earliest waiter still wins the freed slot. Under the old
// in-process-counter key this ordering was lost across a roll.
func TestSingleSlotFourSeatFIFOSurvivesRestart(t *testing.T) {
	const ep = incidentEndpoint
	base := time.Unix(1_700_000_000, 0)
	since := map[string]time.Time{
		"seat1": base,                      // took the slot first
		"seat2": base.Add(1 * time.Second), // queued first
		"seat3": base.Add(2 * time.Second),
		"seat4": base.Add(3 * time.Second),
	}

	// Pre-restart: seat1 holds the single slot; seat2..4 queue FIFO.
	g := NewEndpointGate(1)
	if _, ok := g.Acquire(ep, "seat1", 1, since["seat1"]); !ok {
		t.Fatal("seat1 must take the only slot")
	}
	for _, id := range []string{"seat2", "seat3", "seat4"} {
		if _, ok := g.Acquire(ep, id, 1, since[id]); ok {
			t.Fatalf("%s must queue behind the single held slot", id)
		}
	}
	if front := g.frontWaiter(ep); front != "seat2" {
		t.Fatalf("pre-restart FIFO front must be seat2, got %q", front)
	}

	// Operator restart: a brand-new empty gate. The holder reattaches; the
	// waiters re-drive in REVERSE order (post-roll reconcile order is
	// nondeterministic), each passing its durable wait-start.
	g = NewEndpointGate(1)
	if _, ok := g.Acquire(ep, "seat1", 1, since["seat1"]); !ok {
		t.Fatal("seat1 must re-take its slot after restart (reattach)")
	}
	for _, id := range []string{"seat4", "seat3", "seat2"} { // reverse re-drive
		if _, ok := g.Acquire(ep, id, 1, since[id]); ok {
			t.Fatalf("%s must re-queue after restart (slot held)", id)
		}
	}

	// seat1 releases. Despite seat4 re-registering first post-restart, the
	// earliest-waiting seat2 must win — FIFO order was reconstructed from the
	// durable keys, not post-restart arrival order.
	g.ReleaseByRun("seat1")
	if _, ok := g.Acquire(ep, "seat4", 1, since["seat4"]); ok {
		t.Fatal("FIFO after restart: seat4 must not jump ahead of the earlier-waiting seat2/seat3")
	}
	if _, ok := g.Acquire(ep, "seat3", 1, since["seat3"]); ok {
		t.Fatal("FIFO after restart: seat3 must not jump ahead of the earlier-waiting seat2")
	}
	if _, ok := g.Acquire(ep, "seat2", 1, since["seat2"]); !ok {
		t.Fatal("FIFO after restart: the earliest-waiting seat2 must win the freed slot")
	}
	// seat2 now holds; the next free slot goes to seat3, then seat4 — order intact.
	g.ReleaseByRun("seat2")
	if _, ok := g.Acquire(ep, "seat4", 1, since["seat4"]); ok {
		t.Fatal("FIFO after restart: seat4 must still wait behind seat3")
	}
	if _, ok := g.Acquire(ep, "seat3", 1, since["seat3"]); !ok {
		t.Fatal("FIFO after restart: seat3 must win once it is the front of the queue")
	}
}
