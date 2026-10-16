/*
Copyright 2026 The K8sAgentMesh Authors.

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

package mesh_test

// gateway_band_test.go — the P1.3 growth-band pins (chapter §5.3/§9.2,
// ISI-5824): LRU idle selection, saturation growth toward max, transient
// cold-overflow at max, the pool.saturated signal, grown-slot shrink, and the
// P1.2 back-compat refusal for band-less pools. Together with
// rolepool_test.go's 12/5 math these are the issue's "selection/saturation"
// proof obligations.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/K8sAgentMesh/K8sAgentMesh/a2a"
	"github.com/K8sAgentMesh/K8sAgentMesh/mesh"
)

// sinkRecorder captures the gateway's lifecycle-sink events (the
// pool.saturated signal is observed here until the P1.4 NATS entity lands).
type sinkRecorder struct {
	mu     sync.Mutex
	events []string
}

func (s *sinkRecorder) OnLifecycle(ev, _, _ string, _ error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
}

func (s *sinkRecorder) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.events))
	copy(out, s.events)
	return out
}

func hasEvent(evs []string, want string) bool {
	for _, e := range evs {
		if e == want {
			return true
		}
	}
	return false
}

// bandGateway starts a gateway with the P1.3 band and a short-cadence sweep.
func bandGateway(t *testing.T, min, max int, shrink time.Duration, sink *sinkRecorder) (*mesh.Gateway, *fakeAdapter, *sinkRecorder) {
	t.Helper()
	fa := newFakeAdapter()
	rec := sink
	if rec == nil {
		rec = &sinkRecorder{}
	}
	gw, err := mesh.NewGateway(mesh.GatewayConfig{
		Adapter:  fa,
		Envelope: gatewayEnv(),
		Min:      min,
		Max:      max,
		// Short bounds so the sweep is observable without wall-clock waits;
		// HibernateToStop stays LONG so the P1.2 tier never interferes with
		// the band assertions.
		GrowIdleShrink: shrink,
		Lifecycle: mesh.LifecycleConfig{
			CheckInterval:   5 * time.Millisecond,
			HibernateToStop: 10 * time.Second,
		},
		Sink: rec,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := gw.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return gw, fa, rec
}

func routeTask(t *testing.T, ctx context.Context, gw *mesh.Gateway) func() {
	t.Helper()
	stream, err := gw.Route(ctx, "", a2a.Task{A2ATaskID: time.Now().Format("150405.000000000")})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	return func() { _ = stream.Close() }
}

func countStates(gw *mesh.Gateway) (standing int, busy int, grown int) {
	for _, p := range gw.Processes() {
		if p.State != mesh.StateStopped {
			standing++
		}
		if p.Busy {
			busy++
		}
		if p.Grown {
			grown++
		}
	}
	return
}

// Idle selection tie-breaks LRU (§5.3): with two idle processes the one idle
// LONGEST takes the next route — wear spreads across the set instead of
// hammering spawn order.
func TestBand_LRUIdleSelection(t *testing.T) {
	gw, fa, _ := bandGateway(t, 2, 3, time.Hour, nil)

	// First route takes the oldest (equal clocks → first spawn-order pick).
	r1 := routeTask(t, context.Background(), gw)
	r1() // settle → that process is idle again, but its lastUsed is NOW.
	time.Sleep(5 * time.Millisecond)

	// Second route must take the OTHER process: the first one is the most
	// recently used; LRU prefers the one idle longer.
	r2 := routeTask(t, context.Background(), gw)
	r2()

	fa.mu.Lock()
	defer fa.mu.Unlock()
	if len(fa.routed) != 2 || fa.routed[0] == fa.routed[1] {
		t.Fatalf("routed = %v, want two DISTINCT handles LRU-ordered", fa.routed)
	}
}

// Saturation grows the standing set toward Max (§5.3 tier 1→2): a route that
// finds no idle process and no stopped slot spawns a KEPT standing slot, up
// to Max.
func TestBand_SaturationGrowsTowardMax(t *testing.T) {
	gw, _, _ := bandGateway(t, 2, 3, time.Hour, nil)
	ctx := context.Background()

	// Two routes saturate the floor; the third GROWS the standing set.
	_ = routeTask(t, ctx, gw)
	_ = routeTask(t, ctx, gw)
	_ = routeTask(t, ctx, gw)

	standing, busy, grown := countStates(gw)
	if standing != 3 || busy != 3 || grown != 1 {
		t.Fatalf("standing=%d busy=%d grown=%d, want 3/3/1 (grow toward max)", standing, busy, grown)
	}

	// A fourth route saturates AT max: served by transient cold-overflow,
	// NOT another kept slot (§5.3 tier 3).
	_ = routeTask(t, ctx, gw)
	standing, _, _ = countStates(gw)
	if standing != 3 {
		t.Fatalf("standing=%d after transient overflow, want 3 (max is the ceiling for KEPT slots)", standing)
	}
}

// At Max the overflow process is TRANSIENT: it serves the burst and is torn
// down when the task settles — never kept as standing warmth. The
// pool.saturated signal fires (§8.1) on every saturated route.
func TestBand_TransientOverflowAtMax(t *testing.T) {
	gw, fa, rec := bandGateway(t, 2, 3, time.Hour, nil)
	ctx := context.Background()

	_ = routeTask(t, ctx, gw) // idle
	_ = routeTask(t, ctx, gw) // idle
	_ = routeTask(t, ctx, gw) // grow → 3 standing
	transient := routeTask(t, ctx, gw)

	if !hasEvent(rec.snapshot(), "pool.saturated") {
		t.Fatal("pool.saturated signal never emitted")
	}

	standing, _, _ := countStates(gw)
	fa.mu.Lock()
	spawned := len(fa.handles)
	fa.mu.Unlock()
	if standing != 3 || spawned != 4 {
		t.Fatalf("standing=%d spawned=%d, want 3 standing with a 4th TRANSIENT process", standing, spawned)
	}

	// The burst clears: the transient process is stopped, the 3 kept slots
	// stay standing (busy until their own tasks settle).
	transient()
	fa.mu.Lock()
	stopped := len(fa.stopped)
	fa.mu.Unlock()
	if stopped != 1 {
		t.Fatalf("adapter stops = %d after transient settle, want exactly the 1 transient", stopped)
	}
	standing, _, _ = countStates(gw)
	if standing != 3 {
		t.Fatalf("standing=%d after transient teardown, want 3 kept slots", standing)
	}
}

// A grown slot shrinks after GrowIdleShrink idle (§9.2: the band decays back
// toward min once the burst clears) — far faster than the general
// HibernateToStop bound the floor slots keep.
func TestBand_GrownSlotShrinksAfterIdle(t *testing.T) {
	gw, fa, _ := bandGateway(t, 2, 3, 30*time.Millisecond, nil)
	ctx := context.Background()

	r1 := routeTask(t, ctx, gw)
	r2 := routeTask(t, ctx, gw)
	r3 := routeTask(t, ctx, gw) // grow → 3 standing, 1 grown
	r1()
	r2()
	r3() // all settled: every slot idle, grown clock running

	waitFor(t, 2*time.Second, func() bool {
		standing, _, _ := countStates(gw)
		return standing == 2
	}, "grown slot never shrank back toward min after its idle bound")

	// Exactly the grown slot was reclaimed (the floor slots' 10s
	// HibernateToStop never fired in this window).
	fa.mu.Lock()
	stopped := len(fa.stopped)
	fa.mu.Unlock()
	if stopped != 1 {
		t.Fatalf("adapter stops = %d, want exactly the 1 grown slot", stopped)
	}
}

// A band-less pool (Max <= Min — the P1.1/P1.2 shape) keeps its exact P1.2
// saturation semantics: refuse with ErrNoIdleProcess, no grow, no overflow
// (additive-only D9.3 — the band activates only when declared).
func TestBand_BandlessSaturationStillRefuses(t *testing.T) {
	gw, _, _ := bandGateway(t, 2, 0, time.Hour, nil) // Max clamps to Min
	ctx := context.Background()

	_ = routeTask(t, ctx, gw)
	_ = routeTask(t, ctx, gw)
	_, err := gw.Route(ctx, "", a2a.Task{A2ATaskID: "t3"})
	if !errors.Is(err, mesh.ErrNoIdleProcess) {
		t.Fatalf("err = %v, want ErrNoIdleProcess for a band-less saturated pool", err)
	}
}

// NewGateway clamps the band: Max below Min collapses to Min (no band).
func TestBand_MaxClampsToMin(t *testing.T) {
	gw, err := mesh.NewGateway(mesh.GatewayConfig{Adapter: newFakeAdapter(), Envelope: gatewayEnv(), Min: 3, Max: 1})
	if err != nil {
		t.Fatal(err)
	}
	procs := gw.Processes() // unstarted gateway: zero slots, but the clamp is in the config
	if len(procs) != 0 {
		t.Fatalf("unstarted gateway holds %d slots, want 0", len(procs))
	}
	// The clamp is observable through behavior: a Max<Min pool refuses at
	// saturation instead of growing (covered above); here just pin that the
	// constructor accepts the inverted band without error and Start works.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := gw.Start(ctx); err != nil {
		t.Fatal(err)
	}
}
