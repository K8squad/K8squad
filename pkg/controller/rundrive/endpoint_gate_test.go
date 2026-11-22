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
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestEndpointGateSerializesPerEndpoint pins the ISI-5037 core: one slot per
// endpoint — the second run is refused with the holder named, the holder's
// own re-acquire (re-drive/lap/re-attach) is a no-op grant, and a release
// frees the slot for the waiter.
func TestEndpointGateSerializesPerEndpoint(t *testing.T) {
	g := NewEndpointGate(1)
	const ep = "http://10.0.0.185:11434/v1"

	if _, ok := g.Acquire(ep, "run-1", 0, PriorityKey{}); !ok {
		t.Fatal("first acquire must be granted")
	}
	if _, ok := g.Acquire(ep, "run-1", 0, PriorityKey{}); !ok {
		t.Fatal("same-run re-acquire must be an idempotent grant (re-drive/lap/reattach)")
	}
	holder, ok := g.Acquire(ep, "run-2", 0, PriorityKey{})
	if ok {
		t.Fatal("second run must be refused while the slot is held")
	}
	if holder != "run-1" {
		t.Fatalf("refusal must name the holder, got %q", holder)
	}
	// A DIFFERENT endpoint is an independent slot.
	if _, ok := g.Acquire("http://other:11434/v1", "run-2", 0, PriorityKey{}); !ok {
		t.Fatal("a different endpoint must not contend with the held one")
	}

	g.ReleaseByRun("run-1")
	if _, ok := g.Acquire(ep, "run-2", 0, PriorityKey{}); !ok {
		t.Fatal("release must free the slot for the waiting run")
	}
}

// TestEndpointGateCapacity: the env-tunable slot count lets a concurrent-capable
// endpoint admit N runs before refusing N+1.
func TestEndpointGateCapacity(t *testing.T) {
	g := NewEndpointGate(2)
	const ep = "http://vllm:8000/v1"
	for i := 1; i <= 2; i++ {
		if _, ok := g.Acquire(ep, fmt.Sprintf("run-%d", i), 0, PriorityKey{}); !ok {
			t.Fatalf("acquire run-%d within capacity must be granted", i)
		}
	}
	if _, ok := g.Acquire(ep, "run-3", 0, PriorityKey{}); ok {
		t.Fatal("run-3 beyond capacity must be refused")
	}
	g.ReleaseByRun("run-1")
	if _, ok := g.Acquire(ep, "run-3", 0, PriorityKey{}); !ok {
		t.Fatal("a freed slot must admit the next run")
	}
}

// TestEndpointGateReleaseIdempotent: releasing a run that holds nothing is a
// no-op, and releasing does not disturb other holders.
func TestEndpointGateReleaseIdempotent(t *testing.T) {
	g := NewEndpointGate(1)
	const ep = "http://10.0.0.185:11434/v1"
	g.ReleaseByRun("ghost") // must not panic or corrupt
	if _, ok := g.Acquire(ep, "run-1", 0, PriorityKey{}); !ok {
		t.Fatal("acquire after no-op release must be granted")
	}
	g.ReleaseByRun("run-1")
	g.ReleaseByRun("run-1") // double release is fine
	if _, ok := g.Acquire(ep, "run-2", 0, PriorityKey{}); !ok {
		t.Fatal("slot must be free after idempotent releases")
	}
}

// TestEndpointGateConcurrentAcquire: the map is mutex-guarded — concurrent
// acquires for one slot must grant exactly one winner.
func TestEndpointGateConcurrentAcquire(t *testing.T) {
	g := NewEndpointGate(1)
	const ep = "http://10.0.0.185:11434/v1"
	var wg sync.WaitGroup
	grants := make(chan string, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if _, ok := g.Acquire(ep, id, 0, PriorityKey{}); ok {
				grants <- id
			}
		}(fmt.Sprintf("run-%d", i))
	}
	wg.Wait()
	close(grants)
	var winners []string
	for id := range grants {
		winners = append(winners, id)
	}
	if len(winners) != 1 {
		t.Fatalf("exactly one concurrent acquire must win the single slot, got %v", winners)
	}
}

// TestEndpointGateClampsCapacity: a zero/negative slot count clamps to strict
// serialization (never a gate that admits nobody).
func TestEndpointGateClampsCapacity(t *testing.T) {
	g := NewEndpointGate(0)
	const ep = "http://10.0.0.185:11434/v1"
	if _, ok := g.Acquire(ep, "run-1", 0, PriorityKey{}); !ok {
		t.Fatal("first acquire must be granted under clamped capacity")
	}
	if _, ok := g.Acquire(ep, "run-2", 0, PriorityKey{}); ok {
		t.Fatal("clamped capacity must behave as 1 (strict serialization)")
	}
}

// TestEndpointGateFIFOAdmission pins the ISI-5594 core: when a slot frees it is
// granted to the longest-waiting run (lowest enqueue seq), NOT to whichever
// late arrival happens to poll first — deterministic, starvation-free.
func TestEndpointGateFIFOAdmission(t *testing.T) {
	g := NewEndpointGate(1)
	const ep = "https://api.deepseek.com/v1"

	// run-1 holds the only slot; run-2 then run-3 queue, in that wait order.
	if _, ok := g.Acquire(ep, "run-1", 0, PriorityKey{}); !ok {
		t.Fatal("run-1 must take the free slot")
	}
	if _, ok := g.Acquire(ep, "run-2", 0, PriorityKey{}); ok {
		t.Fatal("run-2 must queue behind the held slot")
	}
	if _, ok := g.Acquire(ep, "run-3", 0, PriorityKey{}); ok {
		t.Fatal("run-3 must queue behind the held slot")
	}

	g.ReleaseByRun("run-1")

	// The slot is free, but run-3 (the later arrival) polls FIRST. FIFO must
	// hold the slot for run-2 and refuse run-3 even though a slot is free.
	if holder, ok := g.Acquire(ep, "run-3", 0, PriorityKey{}); ok {
		t.Fatal("FIFO: run-3 must not jump ahead of the longer-waiting run-2")
	} else if holder != "run-2" {
		t.Fatalf("refusal should name the run ahead in line (run-2), got %q", holder)
	}
	if _, ok := g.Acquire(ep, "run-2", 0, PriorityKey{}); !ok {
		t.Fatal("FIFO: the longest-waiting run-2 must win the freed slot")
	}
	// Now run-2 holds; run-3 (still queued) wins the next free slot.
	g.ReleaseByRun("run-2")
	if _, ok := g.Acquire(ep, "run-3", 0, PriorityKey{}); !ok {
		t.Fatal("run-3 must win once it is the front of the queue")
	}
}

// TestEndpointGatePerEndpointConcurrency: the per-Acquire maxConcurrent (the
// endpoint's declared capacity) overrides the gate default, independently per
// endpoint, and a non-positive value falls back to the default.
func TestEndpointGatePerEndpointConcurrency(t *testing.T) {
	g := NewEndpointGate(1) // default strict-serialize
	const capable = "https://api.deepseek.com/v1"
	const ollama = "http://10.0.0.185:11434/v1"

	// capable endpoint declares 2 → admits two before refusing a third.
	if _, ok := g.Acquire(capable, "run-1", 2, PriorityKey{}); !ok {
		t.Fatal("run-1 within declared capacity (2) must be granted")
	}
	if _, ok := g.Acquire(capable, "run-2", 2, PriorityKey{}); !ok {
		t.Fatal("run-2 within declared capacity (2) must be granted")
	}
	if _, ok := g.Acquire(capable, "run-3", 2, PriorityKey{}); ok {
		t.Fatal("run-3 beyond declared capacity (2) must be refused")
	}

	// A different endpoint with a non-positive declaration falls back to the
	// gate default (1) — independent of the capable endpoint's two holders.
	if _, ok := g.Acquire(ollama, "run-4", 0, PriorityKey{}); !ok {
		t.Fatal("run-4 on an independent endpoint must take its own slot")
	}
	if _, ok := g.Acquire(ollama, "run-5", 0, PriorityKey{}); ok {
		t.Fatal("run-5 must be refused: the fallback default is strict-serialize")
	}
}

// TestEndpointGateFIFOSurvivesRestart pins the ISI-7501 core: a 2-slot endpoint
// admits 2 concurrent holders and queues the rest, and the queue's FIFO order
// survives an operator restart that empties the in-process gate — because the
// FIFO key is the durable per-run wait-start the caller passes (reconstructed
// from the Run CR), not an in-process counter that vanishes with the process.
func TestEndpointGateFIFOSurvivesRestart(t *testing.T) {
	const ep = "https://api.deepseek.com/v1"
	// Durable wait-start timestamps as they would read off each Run's
	// WaitingForEndpointSlot condition (monotone: seat1 waited first).
	base := time.Unix(1_700_000_000, 0)
	since := map[string]time.Time{
		"seat1": base,
		"seat2": base.Add(1 * time.Second),
		"seat3": base.Add(2 * time.Second),
		"seat4": base.Add(3 * time.Second),
	}

	// Pre-restart: a 2-slot endpoint admits seat1+seat2, queues seat3+seat4.
	g := NewEndpointGate(1) // default is strict; the endpoint DECLARES 2.
	for _, id := range []string{"seat1", "seat2"} {
		if _, ok := g.Acquire(ep, id, 2, PriorityKey{WaitSince: since[id]}); !ok {
			t.Fatalf("%s within the 2-slot capacity must be granted", id)
		}
	}
	for _, id := range []string{"seat3", "seat4"} {
		if _, ok := g.Acquire(ep, id, 2, PriorityKey{WaitSince: since[id]}); ok {
			t.Fatalf("%s beyond the 2-slot capacity must queue", id)
		}
	}
	if front := g.frontWaiter(ep); front != "seat3" {
		t.Fatalf("pre-restart FIFO front must be seat3, got %q", front)
	}

	// Simulate the operator restart: a BRAND-NEW gate (all in-process holder
	// and queue state gone). The reattach/re-drive re-registers the two
	// holders, then the two waiters re-drive in REVERSE order (reconcile order
	// after a roll is nondeterministic) — each passing its durable wait-start.
	g = NewEndpointGate(1)
	for _, id := range []string{"seat1", "seat2"} {
		if _, ok := g.Acquire(ep, id, 2, PriorityKey{WaitSince: since[id]}); !ok {
			t.Fatalf("%s must re-take its slot after restart (reattach)", id)
		}
	}
	for _, id := range []string{"seat4", "seat3"} { // reverse re-drive order
		if _, ok := g.Acquire(ep, id, 2, PriorityKey{WaitSince: since[id]}); ok {
			t.Fatalf("%s must re-queue after restart (2 slots held)", id)
		}
	}

	// A slot frees. Despite seat4 having re-registered FIRST post-restart, the
	// earlier-waiting seat3 must win — FIFO order was reconstructed from the
	// durable keys, not from post-restart arrival order. (Under the old
	// in-process-counter key, seat4 would have won and this fails.)
	g.ReleaseByRun("seat1")
	if _, ok := g.Acquire(ep, "seat4", 2, PriorityKey{WaitSince: since["seat4"]}); ok {
		t.Fatal("FIFO after restart: seat4 must not jump ahead of the earlier-waiting seat3")
	}
	if _, ok := g.Acquire(ep, "seat3", 2, PriorityKey{WaitSince: since["seat3"]}); !ok {
		t.Fatal("FIFO after restart: the earliest-waiting seat3 must win the freed slot")
	}
}

// TestEndpointGateEvictsStaleWaiter: a front-of-queue waiter that stops polling
// past the TTL (vanished without a terminal release) must not wedge the slot —
// it is evicted so the next live waiter is admitted.
func TestEndpointGateEvictsStaleWaiter(t *testing.T) {
	g := NewEndpointGate(1)
	g.waiterTTL = time.Minute
	clock := time.Unix(0, 0)
	g.now = func() time.Time { return clock }
	const ep = "https://api.deepseek.com/v1"

	if _, ok := g.Acquire(ep, "run-1", 0, PriorityKey{}); !ok {
		t.Fatal("run-1 must take the free slot")
	}
	// run-2 queues first as an ISSUE-class waiter (top rank, ISI-7499), then
	// run-3 queues later as a discussion-class waiter.
	if _, ok := g.Acquire(ep, "run-2", 0, PriorityKey{Class: ClassIssue}); ok {
		t.Fatal("run-2 must queue")
	}
	if _, ok := g.Acquire(ep, "run-3", 0, PriorityKey{Class: ClassDiscussion}); ok {
		t.Fatal("run-3 must queue")
	}
	g.ReleaseByRun("run-1")

	// run-2 goes silent; advance past the TTL and let only run-3 keep polling.
	// TTL eviction is key-agnostic: no rank immortalizes a vanished waiter.
	clock = clock.Add(2 * time.Minute)
	if _, ok := g.Acquire(ep, "run-3", 0, PriorityKey{Class: ClassDiscussion}); !ok {
		t.Fatal("a stale top-ranked waiter (run-2) must be evicted so run-3 is admitted")
	}
}

// ISI-7483: a holder whose run is terminal or gone must lose its slot so the
// FIFO waiters proceed; a checker error must never reclaim (fail-safe).
func TestEndpointGateReclaimHoldersEvictsDeadHolder(t *testing.T) {
	g := NewEndpointGate(1)
	const ep = "https://api.deepseek.com/v1"
	if _, ok := g.Acquire(ep, "holder", 1, PriorityKey{}); !ok {
		t.Fatalf("holder acquire refused on an empty gate")
	}
	if _, ok := g.Acquire(ep, "waiter", 1, PriorityKey{}); ok {
		t.Fatalf("waiter admitted into a full slot")
	}
	evicted := g.ReclaimHoldersSince(func(runID string, _ time.Time) (bool, error) {
		if runID != "holder" {
			t.Fatalf("liveness asked about unknown run %q", runID)
		}
		return false, nil // terminal
	})
	if len(evicted) != 1 || evicted[0].RunID != "holder" || evicted[0].Endpoint != ep {
		t.Fatalf("evicted = %+v, want exactly the dead holder on %s", evicted, ep)
	}
	if _, ok := g.Acquire(ep, "waiter", 1, PriorityKey{}); !ok {
		t.Fatalf("waiter still refused after the dead holder was reclaimed")
	}
}

// ISI-7483: a liveness-checker error is a maybe — the holder keeps its slot.
func TestEndpointGateReclaimHoldersFailsSafeOnCheckerError(t *testing.T) {
	g := NewEndpointGate(1)
	const ep = "http://10.0.0.185:11434"
	g.Acquire(ep, "holder", 1, PriorityKey{})
	g.Acquire(ep, "waiter", 1, PriorityKey{})
	if ev := g.ReclaimHoldersSince(func(string, time.Time) (bool, error) { return false, fmt.Errorf("list runs: boom") }); len(ev) != 0 {
		t.Fatalf("checker error reclaimed %d slots; want fail-safe keep", len(ev))
	}
	if _, ok := g.Acquire(ep, "waiter", 1, PriorityKey{}); ok {
		t.Fatalf("waiter admitted after a fail-unsafe reclaim")
	}
}

// ISI-7483: a live holder is never reclaimed.
func TestEndpointGateReclaimHoldersKeepsLiveHolder(t *testing.T) {
	g := NewEndpointGate(1)
	g.Acquire("ep", "holder", 1, PriorityKey{})
	g.Acquire("ep", "waiter", 1, PriorityKey{})
	if ev := g.ReclaimHoldersSince(func(string, time.Time) (bool, error) { return true, nil }); len(ev) != 0 {
		t.Fatalf("live holder reclaimed: %+v", ev)
	}
}

// TestEndpointGateClassBeatsPriorityBeatsFIFO pins the ISI-7499 D2 ordering
// in one sweep: admission is (class, priority, wait-start) — an issue-class
// run outranks every discussion-class run regardless of priority or wait
// time; within a class the lower numeric priority admits first even when it
// waited LONGER; within an identical (class, priority) pair the earlier
// wait-start keeps its place against poll jitter.
func TestEndpointGateClassBeatsPriorityBeatsFIFO(t *testing.T) {
	g := NewEndpointGate(1)
	const ep = "https://api.deepseek.com/v1"
	base := time.Unix(1_700_000_000, 0)

	// run-hold (discussion, priority 0, earliest wait-start) holds the only
	// slot first-come — the non-preemptive baseline.
	if _, ok := g.Acquire(ep, "run-hold", 0, PriorityKey{Class: ClassDiscussion, Priority: 0, WaitSince: base}); !ok {
		t.Fatal("run-hold must take the free slot")
	}
	// Queue with durable wait-starts (arrival order), keys chosen to test
	// every ordering dimension:
	//   run-disc-0  : discussion, priority 0, waited longest of all waiters
	//   run-issue-5a: issue, priority 5,    waited 1s
	//   run-issue-1 : issue, priority 1,    waited 2s (best key, latest arrival)
	//   run-issue-5b: issue, priority 5,    waited 3s (same key as 5a, later)
	if _, ok := g.Acquire(ep, "run-disc-0", 0, PriorityKey{Class: ClassDiscussion, Priority: 0, WaitSince: base.Add(1 * time.Second)}); ok {
		t.Fatal("run-disc-0 must queue behind the held slot")
	}
	if _, ok := g.Acquire(ep, "run-issue-5a", 0, PriorityKey{Class: ClassIssue, Priority: 5, WaitSince: base.Add(2 * time.Second)}); ok {
		t.Fatal("run-issue-5a must queue behind the held slot")
	}
	if _, ok := g.Acquire(ep, "run-issue-1", 0, PriorityKey{Class: ClassIssue, Priority: 1, WaitSince: base.Add(3 * time.Second)}); ok {
		t.Fatal("run-issue-1 must queue behind the held slot")
	}
	if _, ok := g.Acquire(ep, "run-issue-5b", 0, PriorityKey{Class: ClassIssue, Priority: 5, WaitSince: base.Add(4 * time.Second)}); ok {
		t.Fatal("run-issue-5b must queue behind the held slot")
	}

	// Expected admission order: run-issue-1 (issue class, lowest priority —
	// despite being the LATEST arrival), run-issue-5a (priority 5, earlier
	// wait-start than 5b), run-issue-5b, run-disc-0 (discussion class ranks
	// below every issue — despite waiting longest).
	order := []string{"run-issue-1", "run-issue-5a", "run-issue-5b", "run-disc-0"}

	cur := "run-hold"
	for i, want := range order {
		g.ReleaseByRun(cur) // terminal release of the previous lap's holder
		// Poll jitter: every still-queued run polls BEFORE the rightful
		// winner — none may be admitted ahead of it.
		for _, id := range order[i+1:] {
			if _, ok := g.Acquire(ep, id, 0, gateTestKey(id, base)); ok {
				t.Fatalf("step %d: %s must not be admitted ahead of %s", i, id, want)
			}
		}
		if _, ok := g.Acquire(ep, want, 0, gateTestKey(want, base)); !ok {
			t.Fatalf("step %d: %s must be admitted (class > priority > FIFO)", i, want)
		}
		cur = want
	}
}

// gateTestKey maps the priority-ordering test ids to their durable keys.
func gateTestKey(id string, base time.Time) PriorityKey {
	switch id {
	case "run-hold", "run-disc-0":
		return PriorityKey{Class: ClassDiscussion, Priority: 0, WaitSince: base.Add(1 * time.Second)}
	case "run-issue-1":
		return PriorityKey{Class: ClassIssue, Priority: 1, WaitSince: base.Add(3 * time.Second)}
	case "run-issue-5a":
		return PriorityKey{Class: ClassIssue, Priority: 5, WaitSince: base.Add(2 * time.Second)}
	case "run-issue-5b":
		return PriorityKey{Class: ClassIssue, Priority: 5, WaitSince: base.Add(4 * time.Second)}
	}
	return PriorityKey{}
}

// TestEndpointGateNeverPreemptsHolder pins the ISI-7499 D4 non-preemption
// invariant: a later, higher-ranked arrival NEVER evicts a holder — the
// holder keeps its slot (and its idempotent re-acquire keeps succeeding)
// until its own terminal ReleaseByRun, and TTL expiry prunes only waiters.
func TestEndpointGateNeverPreemptsHolder(t *testing.T) {
	g := NewEndpointGate(1)
	g.waiterTTL = time.Minute
	clock := time.Unix(0, 0)
	g.now = func() time.Time { return clock }
	const ep = "https://api.deepseek.com/v1"

	// The LOWEST-ranked class holds the slot.
	if _, ok := g.Acquire(ep, "run-low", 0, PriorityKey{Class: ClassDiscussion, Priority: 9}); !ok {
		t.Fatal("run-low must take the free slot")
	}
	// A top-ranked issue arrival is refused and must name the holder.
	if holder, ok := g.Acquire(ep, "run-top", 0, PriorityKey{Class: ClassIssue, Priority: 0}); ok {
		t.Fatal("run-top must queue, not preempt run-low")
	} else if holder != "run-low" {
		t.Fatalf("refusal must name the holder run-low, got %q", holder)
	}
	// The holder's re-drive/retry-lap re-acquire stays an idempotent grant
	// while better-ranked runs are queued against it.
	if _, ok := g.Acquire(ep, "run-low", 0, PriorityKey{Class: ClassDiscussion, Priority: 9}); !ok {
		t.Fatal("holder re-acquire must remain an idempotent grant (non-preemption)")
	}
	if len(g.holders[ep]) != 1 {
		t.Fatalf("exactly one holder expected, got %d", len(g.holders[ep]))
	}
	// Waiters polling past the TTL are evicted; the HOLDER is untouched —
	// TTL applies to waiters only; no rank or timeout evicts a live holder.
	clock = clock.Add(5 * time.Minute)
	if _, ok := g.Acquire(ep, "run-top", 0, PriorityKey{Class: ClassIssue, Priority: 0}); ok {
		t.Fatal("run-top still must not preempt run-low even past waiter TTL")
	}
	if _, held := g.holders[ep]["run-low"]; !held {
		t.Fatal("run-low must still hold the slot: TTL never evicts a holder")
	}
	// Only the holder's own terminal release frees the slot for run-top.
	g.ReleaseByRun("run-low")
	if _, ok := g.Acquire(ep, "run-top", 0, PriorityKey{Class: ClassIssue, Priority: 0}); !ok {
		t.Fatal("after the holder's terminal release, run-top must be admitted")
	}
}

// TestEndpointGatePriorityOrderSurvivesRestart extends the ISI-7501 restart
// contract to the full ISI-7499 key: after a leader restart the in-process
// queue is empty, and the rebuilt order is a deterministic function of the
// durable keys — class and priority order survives arbitrary reattach
// jitter, and equal-key runs keep their durable wait-start order even when
// they re-register in reverse (the ISI-7501 property, now under a
// priority-ordered queue). The pre-restart holder is preserved because its
// unsettled lap re-attaches first, ahead of the waiters' re-poll timers.
func TestEndpointGatePriorityOrderSurvivesRestart(t *testing.T) {
	const ep = "https://api.deepseek.com/v1"
	base := time.Unix(1_700_000_000, 0)
	// Durable keys (what runPriorityKey derives from the Run CRs):
	//   holder : discussion, prio 0, since T0 (holds the slot pre-restart)
	//   issue-1: issue, prio 1, since T3 (best key, LATEST wait-start)
	//   issue-2: issue, prio 2, since T1 (waited longest, worse priority)
	//   disc-0 : discussion, prio 0, since T2
	keys := map[string]PriorityKey{
		"holder":  {Class: ClassDiscussion, Priority: 0, WaitSince: base},
		"issue-1": {Class: ClassIssue, Priority: 1, WaitSince: base.Add(3 * time.Second)},
		"issue-2": {Class: ClassIssue, Priority: 2, WaitSince: base.Add(1 * time.Second)},
		"disc-0":  {Class: ClassDiscussion, Priority: 0, WaitSince: base.Add(2 * time.Second)},
	}

	replay := func(jitter []string) (order []string) {
		g := NewEndpointGate(1) // fresh gate = post-restart state
		// ReattachFollows re-runs the unsettled laps first: the HOLDER
		// re-acquires at leader-elect, before any waiter re-poll timer.
		if _, ok := g.Acquire(ep, "holder", 0, keys["holder"]); !ok {
			t.Fatal("reattached holder must re-take the free slot")
		}
		for _, id := range jitter {
			if _, ok := g.Acquire(ep, id, 0, keys[id]); ok {
				t.Fatalf("%s must not preempt the re-attached holder", id)
			}
		}
		// Drain: release the current holder, poll in the same arbitrary
		// order, record who wins each freed slot. Already-admitted runs are
		// done (their next lap would be an idempotent re-grant, not a fresh
		// admission), so they do not poll again.
		admitted := map[string]bool{}
		for cur := "holder"; len(order) < 3; {
			g.ReleaseByRun(cur)
			cur = ""
			for _, id := range jitter {
				if admitted[id] {
					continue
				}
				if _, ok := g.Acquire(ep, id, 0, keys[id]); ok {
					order = append(order, id)
					admitted[id] = true
					cur = id
					break
				}
			}
			if cur == "" {
				t.Fatalf("no waiter admitted after release (order so far %v)", order)
			}
		}
		return order
	}

	// Radically different reattach jitters must rebuild the SAME admission
	// order: issue-1 (class+priority beat issue-2's longer wait), issue-2
	// (priority beats disc-0's class), disc-0.
	want := []string{"issue-1", "issue-2", "disc-0"}
	a := replay([]string{"disc-0", "issue-2", "issue-1"})
	b := replay([]string{"issue-1", "disc-0", "issue-2"})
	if fmt.Sprint(a) != fmt.Sprint(want) || fmt.Sprint(b) != fmt.Sprint(want) {
		t.Fatalf("restart rebuild must be deterministic from keys: want %v, got %v and %v", want, a, b)
	}
}

// ISI-7687: a holder's acquisition timestamp is frozen at FIRST grant — the
// idempotent re-acquire of a re-driven lap must not reset the bring-up
// clock (the 116h hold was exactly a wedged holder re-acquiring its own
// permit as a no-op on every pass).
func TestEndpointGateHolderSinceFrozenOnReacquire(t *testing.T) {
	g := NewEndpointGate(1)
	t0 := time.Now()
	g.now = func() time.Time { return t0 }
	if _, ok := g.Acquire("ep", "holder", 1, PriorityKey{}); !ok {
		t.Fatalf("acquire refused on a free gate")
	}
	g.now = func() time.Time { return t0.Add(2 * time.Hour) }
	if _, ok := g.Acquire("ep", "holder", 1, PriorityKey{}); !ok {
		t.Fatalf("idempotent re-acquire refused")
	}
	var seen time.Time
	if ev := g.ReclaimHoldersSince(func(_ string, heldSince time.Time) (bool, error) {
		seen = heldSince
		return true, nil
	}); len(ev) != 0 {
		t.Fatalf("live holder evicted")
	}
	if !seen.Equal(t0) {
		t.Fatalf("heldSince = %v, want frozen first-grant %v", seen, t0)
	}
}

// ISI-7687: ReclaimHoldersSince hands the caller the frozen acquisition
// time so it can apply a bring-up/liveness grace — the pod-less-holder
// eviction seam.
func TestEndpointGateReclaimHoldersSinceEvictsDeadHolder(t *testing.T) {
	g := NewEndpointGate(1)
	t0 := time.Now().Add(-2 * time.Hour) // held long past any grace
	g.now = func() time.Time { return t0 }
	if _, ok := g.Acquire("ep", "holder", 1, PriorityKey{}); !ok {
		t.Fatalf("acquire refused")
	}
	if _, ok := g.Acquire("ep", "waiter", 1, PriorityKey{}); ok {
		t.Fatalf("waiter admitted onto a full gate")
	}
	if ev := g.ReclaimHoldersSince(func(runID string, heldSince time.Time) (bool, error) {
		if runID != "holder" {
			return true, nil
		}
		if !heldSince.Equal(t0) {
			t.Fatalf("heldSince = %v, want %v", heldSince, t0)
		}
		return false, nil // pod-less past grace
	}); len(ev) != 1 || ev[0].RunID != "holder" || ev[0].Endpoint != "ep" {
		t.Fatalf("evicted = %v, want [ep/holder]", ev)
	}
	if _, ok := g.Acquire("ep", "waiter", 1, PriorityKey{}); !ok {
		t.Fatalf("waiter still refused after pod-less holder reclaim")
	}
}

// ISI-7687 (review blocker): an evicted holder must NOT win the freed slot
// straight back. Production passes runPriorityKey(run) on every drive pass,
// and once the run was granted its WaitingForEndpointSlot condition is gone,
// so its key falls back to creationTimestamp — the EARLIEST wait-start of
// every contender — while it re-drives at the 2s continueDelay against the
// waiters' 10s poll. The reclaim therefore demotes the evicted run: its next
// Acquire re-registers it as a waiter that sorts strictly behind every
// non-demoted waiter, so the slot goes to the front of the real queue.
func TestEndpointGateEvictedHolderCannotJumpQueue(t *testing.T) {
	g := NewEndpointGate(1)
	const ep = "http://10.0.0.185:11434"
	t0 := time.Now()
	g.now = func() time.Time { return t0 }

	// The zombie holder acquired at t0; a waiter queued with an older
	// durable wait-start (it queued first — the ISI-7685 shape).
	holderKey := PriorityKey{Class: ClassIssue, Priority: UnsetPriority, WaitSince: t0.Add(-116 * time.Hour)}
	if _, ok := g.Acquire(ep, "zombie", 1, holderKey); !ok {
		t.Fatalf("holder acquire refused on a free gate")
	}
	waiterKey := PriorityKey{Class: ClassIssue, Priority: UnsetPriority, WaitSince: t0.Add(-116 * time.Hour).Add(time.Second)}
	if _, ok := g.Acquire(ep, "waiter", 1, waiterKey); ok {
		t.Fatalf("waiter admitted into a full slot")
	}

	// The sweep evicts the pod-less zombie past the grace.
	if ev := g.ReclaimHoldersSince(func(runID string, _ time.Time) (bool, error) {
		return runID != "zombie", nil // zombie dead, anything else live
	}); len(ev) != 1 || ev[0].RunID != "zombie" {
		t.Fatalf("evicted = %v, want [zombie]", ev)
	}

	// The zombie's own next drive pass (2s cadence, earliest creationTimestamp
	// key) must NOT re-capture the slot over the queued waiter.
	g.now = func() time.Time { return t0.Add(2 * time.Second) }
	if _, ok := g.Acquire(ep, "zombie", 1, holderKey); ok {
		t.Fatalf("evicted holder re-acquired the slot over a queued waiter (evict-then-re-grant)")
	}
	// The waiter's poll (10s cadence) is admitted.
	g.now = func() time.Time { return t0.Add(10 * time.Second) }
	if _, ok := g.Acquire(ep, "waiter", 1, waiterKey); !ok {
		t.Fatalf("waiter still refused after the zombie was reclaimed")
	}
}
