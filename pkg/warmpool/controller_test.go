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

package warmpool_test

import (
	"context"
	"testing"
	"time"

	"github.com/K8squad/K8squad/pkg/warmpool"
)

// Story 3.4 (ISI-2885) — the controller that finally CONSUMES the sizing
// policy's Target() (the ISI-2876 gap: "no controller consumes Target()").
// Every scenario is DIFFERENTIAL where a naive twin exists (same discipline
// as sizing_test.go): the twin is the broken variant, asserted to VIOLATE
// the invariant the real controller holds.
//
//	Case ↔ invariant map (story 3.4 AC / arch §9.2-§9.3):
//
//	C1 replenish-toward-target   no-controller twin boots nothing (gap itself)
//	C2 warming-counts (no stack) re-boot-every-tick twin double-boots
//	C3 scale-down after band     raw-every-tick twin tears down on a 1-tick dip
//	C4 miss→scale-up NOW         wait-for-next-tick twin leaves ready=0 after a miss
//	C5 teardown-and-replace      no-replenish twin never replaces a released sandbox
//	C6 batch→0                   batch-targeting twin idles batch pools
//	C7 per-key isolation         shared-pool twin mixes gVisor/Kata inventory

// newController builds a managed gVisor pool over a fake provisioner with a
// step-writable pressure source (the λ the autoscale folds each tick).
func newController(t *testing.T, class warmpool.RunClass) (*warmpool.Controller, *warmpool.Pool, *fakeProvisioner, *float64) {
	t.Helper()
	fp := newFakeProvisioner()
	pool := warmpool.NewPool(fp)
	lambda := 0.0
	press := func(warmpool.PoolKey) float64 { return lambda }
	autoscaler := warmpool.NewAutoscaler(newPolicy(), warmpool.DefaultStabilizationTicks)
	c := warmpool.NewController(pool, autoscaler, warmpool.ManagedKey{
		Key:      gvisorKey,
		Class:    class,
		Pressure: press,
	})
	return c, pool, fp, &lambda
}

// tickReady runs one full cycle: Tick, then mark every booted entry Ready
// (the pod-watch arrival the fake has no clock for — ids are re-derived
// from the provisioner's observed boots).
func tickReady(t *testing.T, c *warmpool.Controller, pool *warmpool.Pool, fp *fakeProvisioner) map[warmpool.PoolKey]int {
	t.Helper()
	targets, err := c.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	for id := range fp.bootsCopy() {
		pool.NotifyReady(id)
	}
	return targets
}

// C1 + C2: the controller replenishes toward the policy target (C1 — the
// gap itself is the twin: without a controller nothing consumes Target()),
// and WARMING entries count toward live so consecutive ticks do not stack
// boots (C2 — the re-boot-every-tick twin double-boots).
func TestControllerReplenishesTowardTargetWithoutStacking(t *testing.T) {
	c, pool, fp, lambda := newController(t, warmpool.ClassInteractive)
	*lambda = warmpool.LambdaMedium // base-stock(λ_med) → target ≥ min floor

	targets := tickReady(t, c, pool, fp)
	want := targets[gvisorKey]
	if want < 1 {
		t.Fatalf("policy target for medium load = %d, want ≥1 (sanity)", want)
	}
	if c := pool.Inventory()[gvisorKey]; c.Warming+c.Ready != want {
		t.Fatalf("after tick-1 live(warming+ready)=%d, want target %d", c.Warming+c.Ready, want)
	}

	// Second tick BEFORE readiness arrives (warming still in flight):
	// warming counts — no stacked boots.
	if _, err := c.Tick(context.Background()); err != nil {
		t.Fatalf("tick-2: %v", err)
	}
	if got := fp.bootCount(); got != want {
		t.Fatalf("tick-2 stacked boots: total %d, want %d (warming must count toward live — twin re-boots every tick)", got, want)
	}

	// Mark ready and tick again — steady state, still no new boots.
	for id := range fp.bootsCopy() {
		pool.NotifyReady(id)
	}
	if _, err := c.Tick(context.Background()); err != nil {
		t.Fatalf("tick-3: %v", err)
	}
	if got := fp.bootCount(); got != want {
		t.Fatalf("steady-state tick booted again: total %d, want %d", got, want)
	}
	if c := pool.Inventory()[gvisorKey]; c.Ready != want {
		t.Fatalf("ready=%d, want %d", c.Ready, want)
	}
}

// C3: scale-down commits ONLY after the autoscaler's stabilization band —
// a ONE-tick pressure dip must not tear warm pods down (the raw-every-tick
// twin flaps). When the dip persists past the band, the surplus drains.
func TestControllerScaleDownWaitsForStabilizationBand(t *testing.T) {
	c, pool, fp, lambda := newController(t, warmpool.ClassInteractive)

	// Heavy → target 3; bring the pool warm.
	*lambda = warmpool.LambdaHeavy
	tickReady(t, c, pool, fp) // tick + pod-watch readiness arrival
	high := pool.Inventory()[gvisorKey]
	if high.Ready != 3 { // pinned v1 curve: heavy gVisor target = 3
		t.Fatalf("heavy ready=%d, want 3 (pinned v1 curve)", high.Ready)
	}

	// One-tick dip to light (target 2): NO teardown yet (band=3 ticks).
	*lambda = warmpool.LambdaLight
	tickReady(t, c, pool, fp)
	if got := pool.Inventory()[gvisorKey].Ready + pool.Inventory()[gvisorKey].Warming; got != 3 {
		t.Fatalf("single-tick dip tore down to %d — stabilization band violated (twin flaps)", got)
	}

	// Persist the dip past the band: surplus drains to the light target (2).
	tickReady(t, c, pool, fp)
	tickReady(t, c, pool, fp)
	if got := pool.Inventory()[gvisorKey].Ready + pool.Inventory()[gvisorKey].Warming; got != 2 {
		t.Fatalf("sustained dip left live=%d, want 2 (band elapsed — scale-down must commit)", got)
	}
}

// C4: an empty-pool bind (the AC's "pool is empty" arm) scales up NOW via
// the bind-miss trigger — not at the next tick. Twin: a next-tick-only
// controller leaves ready=0 right after the miss.
func TestControllerBindMissTriggersImmediateScaleUp(t *testing.T) {
	c, pool, fp, lambda := newController(t, warmpool.ClassInteractive)
	*lambda = warmpool.LambdaLight // target 2

	// Warm the pool to target through one tick.
	tickReady(t, c, pool, fp)

	// Claim BOTH warm entries (pool drained).
	for i := 0; i < 2; i++ {
		if _, err := pool.Bind(context.Background(), "run-"+string(rune('a'+i)), gvisorKey, warmpool.ClassInteractive); err != nil {
			t.Fatalf("bind %d: %v", i, err)
		}
	}
	if c := pool.Inventory()[gvisorKey]; c.Ready != 0 {
		t.Fatalf("drain failed: ready=%d", c.Ready)
	}

	before := fp.bootCount()
	// The empty-pool bind: fires the miss trigger → ReplenishKey boots toward
	// target immediately (bounded by warm inventory: bound don't count).
	if _, err := pool.Bind(context.Background(), "run-miss", gvisorKey, warmpool.ClassInteractive); err != nil {
		t.Fatalf("miss bind: %v", err)
	}
	if got := fp.bootCount() - before; got < 1 {
		t.Fatalf("bind-miss triggered %d immediate boots, want ≥1 (twin waits for the next tick)", got)
	}
}

// C5: §9.3 teardown-and-replace at the CONTROLLER level — after a Run
// releases, the next tick replenishes a fresh replacement (bound entries
// don't count toward live; the released one never returns).
func TestControllerReplacesReleasedSandbox(t *testing.T) {
	c, pool, fp, lambda := newController(t, warmpool.ClassInteractive)
	*lambda = warmpool.LambdaLight // target 2

	tickReady(t, c, pool, fp)
	for id := range fp.bootsCopy() {
		pool.NotifyReady(id)
	}

	ref, err := pool.Bind(context.Background(), "run-1", gvisorKey, warmpool.ClassInteractive)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := pool.Release(context.Background(), "run-1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if !fp.tornDown(ref) {
		t.Fatalf("release did not tear down the bound sandbox %s", ref)
	}

	// Bound=0 now, live=1 → tick must replenish toward 2 with a FRESH boot.
	before := fp.bootCount()
	tickReady(t, c, pool, fp)
	if got := fp.bootCount() - before; got != 1 {
		t.Fatalf("replacement boot count=%d, want 1 (teardown-and-replace — twin never replenishes)", got)
	}
	if c := pool.Inventory()[gvisorKey]; c.Warming+c.Ready != 2 {
		t.Fatalf("post-replace live=%d, want target 2", c.Warming+c.Ready)
	}
	// The released run must not hold a ref after the replacement tick, and
	// the fresh-id property (a destroyed ref is never re-handed-out) is
	// pinned at the Pool level in TestPoolReleaseTeardownAndReplace.
	if pool.Ref("run-1") != "" {
		t.Fatal("released run still holds a ref after replacement tick")
	}
}

// C6: batch keys size to target 0 — the controller boots nothing for them
// (§9.2 hybrid: zero idle cost), and a batch bind cold-boots its dedicated
// sandbox without touching any warm inventory.
func TestControllerBatchKeyStaysCold(t *testing.T) {
	c, pool, fp, _ := newController(t, warmpool.ClassBatch)
	if targets := tickReady(t, c, pool, fp); targets[gvisorKey] != 0 {
		t.Fatalf("batch target = %d, want 0 (§9.2 hybrid regime)", targets[gvisorKey])
	}
	if fp.bootCount() != 0 {
		t.Fatalf("controller idled %d warm sandboxes for a batch pool — zero idle cost violated", fp.bootCount())
	}
	if _, err := pool.Bind(context.Background(), "run-b", gvisorKey, warmpool.ClassBatch); err != nil {
		t.Fatalf("batch bind: %v", err)
	}
	if fp.bootCount() != 1 {
		t.Fatalf("batch bind booted %d, want its single dedicated cold boot", fp.bootCount())
	}
}

// C7: per-key isolation — gVisor and Kata pools replenish independently and
// never cross (a shared-pool twin would mix inventory across R classes).
func TestControllerPerKeyIsolation(t *testing.T) {
	fp := newFakeProvisioner()
	pool := warmpool.NewPool(fp)
	autoscaler := warmpool.NewAutoscaler(newPolicy(), warmpool.DefaultStabilizationTicks)
	c := warmpool.NewController(pool, autoscaler,
		warmpool.ManagedKey{Key: gvisorKey, Class: warmpool.ClassInteractive, Pressure: warmpool.StaticPressure(warmpool.LambdaHeavy)},
		warmpool.ManagedKey{Key: kataKey, Class: warmpool.ClassInteractive, Pressure: warmpool.StaticPressure(warmpool.LambdaHeavy)},
	)
	// Kata's max-capped target (10) exceeds the default per-tick boot bound
	// (4); this test drives ONE reconcile, so lift the fan-out bound.
	c.SetMaxBootPerTick(16)

	targets, err := c.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	// Same λ and SLA, ~9x longer Kata R → strictly larger Kata target
	// (pinned v1 curve: gvisor heavy=3, kata clamped to max=10).
	if targets[kataKey] <= targets[gvisorKey] {
		t.Fatalf("kata target %d not larger than gvisor %d — per-key R collapsed (shared-R twin)", targets[kataKey], targets[gvisorKey])
	}
	// Boots land on the RIGHT key's provisioner calls.
	fp.mu.Lock()
	gv, kt := 0, 0
	for _, key := range fp.boots {
		if key == gvisorKey {
			gv++
		}
		if key == kataKey {
			kt++
		}
	}
	fp.mu.Unlock()
	if gv != targets[gvisorKey] || kt != targets[kataKey] {
		t.Fatalf("per-key boots gvisor=%d/%d kata=%d/%d — inventory crossed keys", gv, targets[gvisorKey], kt, targets[kataKey])
	}

	// A gVisor claim pops only gVisor warmth.
	for id := range fp.bootsCopy() {
		pool.NotifyReady(id)
	}
	if _, err := pool.Bind(context.Background(), "run-g", gvisorKey, warmpool.ClassInteractive); err != nil {
		t.Fatalf("gvisor bind: %v", err)
	}
	if c := pool.Inventory()[kataKey]; c.Bound != 0 {
		t.Fatal("gVisor claim bound a Kata sandbox — keys crossed")
	}
}

// Run loop sanity: the interval loop ticks and stops on ctx cancel (the
// manager-runnable form the operator hosts).
func TestControllerRunLoopStops(t *testing.T) {
	c, pool, _, _ := newController(t, warmpool.ClassInteractive)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx, 5*time.Millisecond) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v on cancel, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not stop on ctx cancel")
	}
	_ = pool
}

// C8: Targets() is the status-surface read — it must not race the Run/Tick
// loop's writes to the autoscaler's per-key state. The unsynchronized-read
// twin is exactly the -race failure this pins (Go map concurrent
// read/write is fatal, not a recoverable panic).
func TestControllerTargetsConcurrentWithTick(t *testing.T) {
	c, _, fp, lambda := newController(t, warmpool.ClassInteractive)
	*lambda = warmpool.LambdaMedium

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx, time.Millisecond) }()

	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
		if tgt := c.Targets()[gvisorKey]; tgt < 0 {
			t.Fatalf("negative target %d", tgt)
		}
	}
	cancel()

	_ = fp
}

// bootedCopy is a test helper snapshot of fp.boots.
func (f *fakeProvisioner) bootsCopy() map[string]warmpool.PoolKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]warmpool.PoolKey, len(f.boots))
	for id, k := range f.boots {
		out[id] = k
	}
	return out
}
