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
)

// TestEndpointGateSerializesPerEndpoint pins the ISI-5037 core: one slot per
// endpoint — the second run is refused with the holder named, the holder's
// own re-acquire (re-drive/lap/re-attach) is a no-op grant, and a release
// frees the slot for the waiter.
func TestEndpointGateSerializesPerEndpoint(t *testing.T) {
	g := NewEndpointGate(1)
	const ep = "http://10.0.0.185:11434/v1"

	if _, ok := g.Acquire(ep, "run-1"); !ok {
		t.Fatal("first acquire must be granted")
	}
	if _, ok := g.Acquire(ep, "run-1"); !ok {
		t.Fatal("same-run re-acquire must be an idempotent grant (re-drive/lap/reattach)")
	}
	holder, ok := g.Acquire(ep, "run-2")
	if ok {
		t.Fatal("second run must be refused while the slot is held")
	}
	if holder != "run-1" {
		t.Fatalf("refusal must name the holder, got %q", holder)
	}
	// A DIFFERENT endpoint is an independent slot.
	if _, ok := g.Acquire("http://other:11434/v1", "run-2"); !ok {
		t.Fatal("a different endpoint must not contend with the held one")
	}

	g.ReleaseByRun("run-1")
	if _, ok := g.Acquire(ep, "run-2"); !ok {
		t.Fatal("release must free the slot for the waiting run")
	}
}

// TestEndpointGateCapacity: the env-tunable slot count lets a concurrent-capable
// endpoint admit N runs before refusing N+1.
func TestEndpointGateCapacity(t *testing.T) {
	g := NewEndpointGate(2)
	const ep = "http://vllm:8000/v1"
	for i := 1; i <= 2; i++ {
		if _, ok := g.Acquire(ep, fmt.Sprintf("run-%d", i)); !ok {
			t.Fatalf("acquire run-%d within capacity must be granted", i)
		}
	}
	if _, ok := g.Acquire(ep, "run-3"); ok {
		t.Fatal("run-3 beyond capacity must be refused")
	}
	g.ReleaseByRun("run-1")
	if _, ok := g.Acquire(ep, "run-3"); !ok {
		t.Fatal("a freed slot must admit the next run")
	}
}

// TestEndpointGateReleaseIdempotent: releasing a run that holds nothing is a
// no-op, and releasing does not disturb other holders.
func TestEndpointGateReleaseIdempotent(t *testing.T) {
	g := NewEndpointGate(1)
	const ep = "http://10.0.0.185:11434/v1"
	g.ReleaseByRun("ghost") // must not panic or corrupt
	if _, ok := g.Acquire(ep, "run-1"); !ok {
		t.Fatal("acquire after no-op release must be granted")
	}
	g.ReleaseByRun("run-1")
	g.ReleaseByRun("run-1") // double release is fine
	if _, ok := g.Acquire(ep, "run-2"); !ok {
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
			if _, ok := g.Acquire(ep, id); ok {
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
	if _, ok := g.Acquire(ep, "run-1"); !ok {
		t.Fatal("first acquire must be granted under clamped capacity")
	}
	if _, ok := g.Acquire(ep, "run-2"); ok {
		t.Fatal("clamped capacity must behave as 1 (strict serialization)")
	}
}
