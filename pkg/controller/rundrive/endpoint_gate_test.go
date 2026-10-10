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

	if _, ok := g.Acquire(ep, "run-1", 0); !ok {
		t.Fatal("first acquire must be granted")
	}
	if _, ok := g.Acquire(ep, "run-1", 0); !ok {
		t.Fatal("same-run re-acquire must be an idempotent grant (re-drive/lap/reattach)")
	}
	holder, ok := g.Acquire(ep, "run-2", 0)
	if ok {
		t.Fatal("second run must be refused while the slot is held")
	}
	if holder != "run-1" {
		t.Fatalf("refusal must name the holder, got %q", holder)
	}
	// A DIFFERENT endpoint is an independent slot.
	if _, ok := g.Acquire("http://other:11434/v1", "run-2", 0); !ok {
		t.Fatal("a different endpoint must not contend with the held one")
	}

	g.ReleaseByRun("run-1")
	if _, ok := g.Acquire(ep, "run-2", 0); !ok {
		t.Fatal("release must free the slot for the waiting run")
	}
}

// TestEndpointGateCapacity: the env-tunable slot count lets a concurrent-capable
// endpoint admit N runs before refusing N+1.
func TestEndpointGateCapacity(t *testing.T) {
	g := NewEndpointGate(2)
	const ep = "http://vllm:8000/v1"
	for i := 1; i <= 2; i++ {
		if _, ok := g.Acquire(ep, fmt.Sprintf("run-%d", i), 0); !ok {
			t.Fatalf("acquire run-%d within capacity must be granted", i)
		}
	}
	if _, ok := g.Acquire(ep, "run-3", 0); ok {
		t.Fatal("run-3 beyond capacity must be refused")
	}
	g.ReleaseByRun("run-1")
	if _, ok := g.Acquire(ep, "run-3", 0); !ok {
		t.Fatal("a freed slot must admit the next run")
	}
}

// TestEndpointGateReleaseIdempotent: releasing a run that holds nothing is a
// no-op, and releasing does not disturb other holders.
func TestEndpointGateReleaseIdempotent(t *testing.T) {
	g := NewEndpointGate(1)
	const ep = "http://10.0.0.185:11434/v1"
	g.ReleaseByRun("ghost") // must not panic or corrupt
	if _, ok := g.Acquire(ep, "run-1", 0); !ok {
		t.Fatal("acquire after no-op release must be granted")
	}
	g.ReleaseByRun("run-1")
	g.ReleaseByRun("run-1") // double release is fine
	if _, ok := g.Acquire(ep, "run-2", 0); !ok {
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
			if _, ok := g.Acquire(ep, id, 0); ok {
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
	if _, ok := g.Acquire(ep, "run-1", 0); !ok {
		t.Fatal("first acquire must be granted under clamped capacity")
	}
	if _, ok := g.Acquire(ep, "run-2", 0); ok {
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
	if _, ok := g.Acquire(ep, "run-1", 0); !ok {
		t.Fatal("run-1 must take the free slot")
	}
	if _, ok := g.Acquire(ep, "run-2", 0); ok {
		t.Fatal("run-2 must queue behind the held slot")
	}
	if _, ok := g.Acquire(ep, "run-3", 0); ok {
		t.Fatal("run-3 must queue behind the held slot")
	}

	g.ReleaseByRun("run-1")

	// The slot is free, but run-3 (the later arrival) polls FIRST. FIFO must
	// hold the slot for run-2 and refuse run-3 even though a slot is free.
	if holder, ok := g.Acquire(ep, "run-3", 0); ok {
		t.Fatal("FIFO: run-3 must not jump ahead of the longer-waiting run-2")
	} else if holder != "run-2" {
		t.Fatalf("refusal should name the run ahead in line (run-2), got %q", holder)
	}
	if _, ok := g.Acquire(ep, "run-2", 0); !ok {
		t.Fatal("FIFO: the longest-waiting run-2 must win the freed slot")
	}
	// Now run-2 holds; run-3 (still queued) wins the next free slot.
	g.ReleaseByRun("run-2")
	if _, ok := g.Acquire(ep, "run-3", 0); !ok {
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
	if _, ok := g.Acquire(capable, "run-1", 2); !ok {
		t.Fatal("run-1 within declared capacity (2) must be granted")
	}
	if _, ok := g.Acquire(capable, "run-2", 2); !ok {
		t.Fatal("run-2 within declared capacity (2) must be granted")
	}
	if _, ok := g.Acquire(capable, "run-3", 2); ok {
		t.Fatal("run-3 beyond declared capacity (2) must be refused")
	}

	// A different endpoint with a non-positive declaration falls back to the
	// gate default (1) — independent of the capable endpoint's two holders.
	if _, ok := g.Acquire(ollama, "run-4", 0); !ok {
		t.Fatal("run-4 on an independent endpoint must take its own slot")
	}
	if _, ok := g.Acquire(ollama, "run-5", 0); ok {
		t.Fatal("run-5 must be refused: the fallback default is strict-serialize")
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

	if _, ok := g.Acquire(ep, "run-1", 0); !ok {
		t.Fatal("run-1 must take the free slot")
	}
	// run-2 queues first (becomes FIFO front), then run-3 queues.
	if _, ok := g.Acquire(ep, "run-2", 0); ok {
		t.Fatal("run-2 must queue")
	}
	if _, ok := g.Acquire(ep, "run-3", 0); ok {
		t.Fatal("run-3 must queue")
	}
	g.ReleaseByRun("run-1")

	// run-2 goes silent; advance past the TTL and let only run-3 keep polling.
	clock = clock.Add(2 * time.Minute)
	if _, ok := g.Acquire(ep, "run-3", 0); !ok {
		t.Fatal("a stale front waiter (run-2) must be evicted so run-3 is admitted")
	}
}
