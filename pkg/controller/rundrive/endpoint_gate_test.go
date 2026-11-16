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
		if _, ok := g.Acquire(ep, id, 2, PriorityKey{Since: since[id]}); !ok {
			t.Fatalf("%s within the 2-slot capacity must be granted", id)
		}
	}
	for _, id := range []string{"seat3", "seat4"} {
		if _, ok := g.Acquire(ep, id, 2, PriorityKey{Since: since[id]}); ok {
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
		if _, ok := g.Acquire(ep, id, 2, PriorityKey{Since: since[id]}); !ok {
			t.Fatalf("%s must re-take its slot after restart (reattach)", id)
		}
	}
	for _, id := range []string{"seat4", "seat3"} { // reverse re-drive order
		if _, ok := g.Acquire(ep, id, 2, PriorityKey{Since: since[id]}); ok {
			t.Fatalf("%s must re-queue after restart (2 slots held)", id)
		}
	}

	// A slot frees. Despite seat4 having re-registered FIRST post-restart, the
	// earlier-waiting seat3 must win — FIFO order was reconstructed from the
	// durable keys, not from post-restart arrival order. (Under the old
	// in-process-counter key, seat4 would have won and this fails.)
	g.ReleaseByRun("seat1")
	if _, ok := g.Acquire(ep, "seat4", 2, PriorityKey{Since: since["seat4"]}); ok {
		t.Fatal("FIFO after restart: seat4 must not jump ahead of the earlier-waiting seat3")
	}
	if _, ok := g.Acquire(ep, "seat3", 2, PriorityKey{Since: since["seat3"]}); !ok {
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
	// run-2 queues first (becomes FIFO front), then run-3 queues.
	if _, ok := g.Acquire(ep, "run-2", 0, PriorityKey{}); ok {
		t.Fatal("run-2 must queue")
	}
	if _, ok := g.Acquire(ep, "run-3", 0, PriorityKey{}); ok {
		t.Fatal("run-3 must queue")
	}
	g.ReleaseByRun("run-1")

	// run-2 goes silent; advance past the TTL and let only run-3 keep polling.
	clock = clock.Add(2 * time.Minute)
	if _, ok := g.Acquire(ep, "run-3", 0, PriorityKey{}); !ok {
		t.Fatal("a stale front waiter (run-2) must be evicted so run-3 is admitted")
	}
}

// TestEndpointGatePriorityAdmission pins the ISI-7499 C3 core: class beats
// priority beats FIFO. A higher-class late arrival jumps ahead of a
// lower-class earlier waiter; within the same class a higher-priority waiter
// jumps ahead; within the same (class, priority) the earlier Since wins.
func TestEndpointGatePriorityAdmission(t *testing.T) {
	g := NewEndpointGate(1)
	const ep = "https://api.deepseek.com/v1"
	base := time.Unix(1_700_000_000, 0)

	// run-holder takes the only slot.
	if _, ok := g.Acquire(ep, "run-holder", 0, PriorityKey{Since: base}); !ok {
		t.Fatal("run-holder must take the free slot")
	}

	// Three waiters arrive in order: low-class (class=0, prio=0), then
	// high-class (class=2, prio=0), then mid-priority (class=1, prio=5).
	// FIFO-only ordering would put run-low first; priority ordering must
	// put run-high first, run-mid second, run-low last.
	if _, ok := g.Acquire(ep, "run-low", 0, PriorityKey{Class: 0, Priority: 0, Since: base.Add(1 * time.Second)}); ok {
		t.Fatal("run-low must queue (slot held)")
	}
	if _, ok := g.Acquire(ep, "run-high", 0, PriorityKey{Class: 2, Priority: 0, Since: base.Add(2 * time.Second)}); ok {
		t.Fatal("run-high must queue (slot held)")
	}
	if _, ok := g.Acquire(ep, "run-mid", 0, PriorityKey{Class: 1, Priority: 5, Since: base.Add(3 * time.Second)}); ok {
		t.Fatal("run-mid must queue (slot held)")
	}

	// Slot freed. run-high (class=2) must win over run-mid (class=1) and
	// run-low (class=0) despite being the second to arrive.
	g.ReleaseByRun("run-holder")
	if holder, ok := g.Acquire(ep, "run-low", 0, PriorityKey{Class: 0, Since: base.Add(1 * time.Second)}); ok {
		t.Fatal("class: run-low must not win while higher-class run-high is queued")
	} else if holder != "run-high" {
		t.Fatalf("refusal must name the highest-class front waiter (run-high), got %q", holder)
	}
	if _, ok := g.Acquire(ep, "run-high", 0, PriorityKey{Class: 2, Since: base.Add(2 * time.Second)}); !ok {
		t.Fatal("class: run-high (highest class) must win the freed slot")
	}

	// run-high now holds. Free it; run-mid (class=1, prio=5) beats run-low
	// (class=0, prio=0) despite run-low arriving earlier.
	g.ReleaseByRun("run-high")
	if holder, ok := g.Acquire(ep, "run-low", 0, PriorityKey{Class: 0, Since: base.Add(1 * time.Second)}); ok {
		t.Fatal("priority: run-low must not win while higher-class run-mid is queued")
	} else if holder != "run-mid" {
		t.Fatalf("refusal must name the higher-class front waiter (run-mid), got %q", holder)
	}
	if _, ok := g.Acquire(ep, "run-mid", 0, PriorityKey{Class: 1, Priority: 5, Since: base.Add(3 * time.Second)}); !ok {
		t.Fatal("priority: run-mid (class=1,prio=5) must beat run-low (class=0)")
	}

	// run-mid now holds; free it; run-low is finally alone and wins.
	g.ReleaseByRun("run-mid")
	if _, ok := g.Acquire(ep, "run-low", 0, PriorityKey{Class: 0, Since: base.Add(1 * time.Second)}); !ok {
		t.Fatal("FIFO tail: run-low must win when it is the only remaining waiter")
	}
}

// TestEndpointGateNonPreemption asserts the invariant that a holder is NEVER
// evicted when a higher-priority waiter arrives or when it re-acquires.
// A higher-priority run can only jump to the front of the WAIT queue; it
// cannot displace a run that already holds a slot.
func TestEndpointGateNonPreemption(t *testing.T) {
	g := NewEndpointGate(1)
	const ep = "https://api.deepseek.com/v1"
	base := time.Unix(1_700_000_000, 0)

	// run-low holds the only slot with the lowest priority.
	if _, ok := g.Acquire(ep, "run-low", 0, PriorityKey{Class: 0, Priority: 0, Since: base}); !ok {
		t.Fatal("run-low must take the free slot")
	}

	// run-high arrives with the highest priority and queues.
	if _, ok := g.Acquire(ep, "run-high", 0, PriorityKey{Class: 99, Priority: 99, Since: base.Add(1 * time.Second)}); ok {
		t.Fatal("run-high must queue; it must not preempt the holder")
	}

	// run-low re-acquires (re-drive / lap): must still be a holder, not evicted.
	if _, ok := g.Acquire(ep, "run-low", 0, PriorityKey{Class: 0, Priority: 0, Since: base}); !ok {
		t.Fatal("non-preemption: run-low re-acquire must still be an idempotent grant (not evicted by run-high)")
	}

	// run-high polls again; run-low still holds, so run-high must still be refused.
	if holder, ok := g.Acquire(ep, "run-high", 0, PriorityKey{Class: 99, Priority: 99, Since: base.Add(1 * time.Second)}); ok {
		t.Fatal("non-preemption: run-high must not win while run-low still holds")
	} else if holder != "run-low" {
		t.Fatalf("refusal must name the holder (run-low), got %q", holder)
	}

	// Only after run-low explicitly releases does run-high win the slot.
	g.ReleaseByRun("run-low")
	if _, ok := g.Acquire(ep, "run-high", 0, PriorityKey{Class: 99, Priority: 99, Since: base.Add(1 * time.Second)}); !ok {
		t.Fatal("run-high must win the slot after the holder releases")
	}
}
