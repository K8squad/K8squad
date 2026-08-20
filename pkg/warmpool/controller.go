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

// controller.go — the §9.2 WarmPool controller (Story 3.4 / ISI-2885): the
// reconciler loop that finally CONSUMES the sizing policy's Target()
// (sizing.go, Story 3.5) and drives the physical pool toward it. This is the
// gap ISI-2876 named — "no controller consumes Target()" — closed.
//
// One Tick per registered key:
//
//	target = autoscaler.Reconcile(key, λ(key), class)   ← the POLICY (opaque)
//	live   = pool warming + ready for key               ← the MECHANISM state
//	live < target → Boot (target - live) fresh sandboxes (scale UP immediate)
//	live > target → ScaleDown (live - target) Ready entries (scale DOWN only
//	                after the autoscaler's stabilization band committed it)
//
// Design notes, each pinned by a test in controller_test.go:
//
//   - Scale-up is IMMEDIATE (never starve a burst — §9.2); scale-down is
//     damped by the Autoscaler's stabilization ticks, so this loop simply
//     follows the effective target — the hysteresis lives in the policy
//     object, not duplicated here.
//   - Warming entries COUNT toward live: a replenish in flight must not
//     stack a second boot on the next tick (double-boot is the naive twin
//     the falsification catches).
//   - Bound entries do NOT count: a claimed sandbox is the Run's, not pool
//     capacity — and §9.3 teardown-and-replace means the controller
//     replenishes a fresh replacement on the next tick after a Release.
//   - The bind-miss trigger (Pool.SetBindMiss → ReplenishKey) gives the
//     empty-pool AC its scale-up NOW, not at the next tick.
//   - Single-owner: the loop runs under the operator's leader election
//     (arch §5.2 — "one owner, no racing resizers"); Run(ctx) is the
//     manager-runnable form of that loop.
//
// λ (claim pressure) is an injected PressureSource, not a metric scrape —
// the OTel spine is Epic 13's gap (ISI-2891) and this mechanism must be
// testable without it. Production wires the source to the
// warmpool.claim.pressure gauge (obs §5.3); tests use step functions.
package warmpool

import (
	"context"
	"sync"
	"time"
)

// PressureSource supplies the live claim rate λ (claims/second) for a key —
// the autoscale INPUT (FR-C4: λ is an input to every reconcile, never a
// baked constant). Return 0 when no signal exists yet (target floors at
// min).
type PressureSource func(key PoolKey) float64

// StaticPressure is the constant-λ source (the pre-Epic-13 default and the
// load-regime step values of sizing.go).
func StaticPressure(lambda float64) PressureSource {
	return func(PoolKey) float64 { return lambda }
}

// ManagedKey is one pool the controller reconciles: its sizing key, its run
// class (§9.2 hybrid — batch keys size to 0), and its pressure source.
type ManagedKey struct {
	Key      PoolKey
	Class    RunClass
	Pressure PressureSource
}

// Controller reconciles the physical pool toward the sizing policy's
// effective targets. It owns no clock of its own beyond the Tick cadence —
// Tick is exported so tests (and the falsification harness) drive discrete
// steps; Run wraps Tick in the interval loop for production.
type Controller struct {
	pool       *Pool
	autoscaler *Autoscaler
	keys       []ManagedKey
	byKey      map[PoolKey]ManagedKey

	// maxBootPerTick bounds the boots one reconcile may start (burst
	// guard; default 4 — the max-cap of the v1 policy already bounds the
	// target itself, this bounds the per-tick fan-out).
	maxBootPerTick int

	mu sync.Mutex // serializes Ticks against concurrent ReplenishKey
}

// NewController returns a controller driving pool through autoscaler for the
// given managed keys. It also registers itself as the pool's bind-miss
// scale-up trigger (Pool.SetBindMiss) — the empty-pool AC.
func NewController(pool *Pool, autoscaler *Autoscaler, keys ...ManagedKey) *Controller {
	if len(keys) == 0 {
		panic("warmpool.NewController: at least one ManagedKey is required")
	}
	c := &Controller{
		pool:           pool,
		autoscaler:     autoscaler,
		keys:           keys,
		byKey:          make(map[PoolKey]ManagedKey, len(keys)),
		maxBootPerTick: 4,
	}
	for _, mk := range keys {
		if mk.Pressure == nil {
			mk.Pressure = StaticPressure(0)
		}
		c.byKey[mk.Key] = mk
	}
	pool.SetBindMiss(func(key PoolKey, _ RunClass) { c.ReplenishKey(context.Background(), key) })
	return c
}

// SetMaxBootPerTick overrides the per-tick boot fan-out bound (tests).
func (c *Controller) SetMaxBootPerTick(n int) { c.maxBootPerTick = n }

// liveFor reads the pool's replenish-relevant count for key: Warming +
// Ready. Bound sandboxes are excluded (claimed capacity is not pool
// capacity; §9.3 replaces them after Release).
func liveFor(inv map[PoolKey]Counts, key PoolKey) int {
	return inv[key].Warming + inv[key].Ready
}

// Tick runs one reconcile pass over every managed key and returns the
// effective targets it drove (keyed by pool key — deterministic iteration:
// managed order is preserved, not map order).
func (c *Controller) Tick(ctx context.Context) (map[PoolKey]int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	inv := c.pool.Inventory()
	targets := make(map[PoolKey]int, len(c.keys))
	var firstErr error

	for _, mk := range c.keys {
		lambda := mk.Pressure(mk.Key)
		target, err := c.autoscaler.Reconcile(mk.Key, lambda, mk.Class)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		targets[mk.Key] = target

		live := liveFor(inv, mk.Key)
		switch {
		case live < target:
			// Scale UP immediately: boot the deficit, bounded per tick.
			deficit := target - live
			if deficit > c.maxBootPerTick {
				deficit = c.maxBootPerTick
			}
			for i := 0; i < deficit; i++ {
				if _, err := c.pool.Boot(ctx, mk.Key); err != nil {
					if firstErr == nil {
						firstErr = err
					}
				}
			}
		case live > target:
			// Scale DOWN: drain the surplus Ready entries (oldest first).
			// The autoscaler already held this lower target for the whole
			// stabilization band — committing it here is not thrashing.
			if n := live - target; n > 0 {
				c.pool.ScaleDown(ctx, mk.Key, n)
			}
		}
	}
	return targets, firstErr
}

// ReplenishKey is the immediate scale-up path the pool's bind-miss trigger
// invokes: one single-key pass that tops the key back up toward its CURRENT
// effective target. It does not fold a new pressure reading (the miss
// itself is the pressure event; the next Tick re-derives λ properly) and
// never scales DOWN — a miss is by definition upward.
func (c *Controller) ReplenishKey(ctx context.Context, key PoolKey) {
	c.mu.Lock()
	defer c.mu.Unlock()

	mk, ok := c.byKey[key]
	if !ok {
		return // not a managed key (unregistered cold-boot) — nothing to do
	}
	target := c.autoscaler.Current(key)
	if target <= 0 {
		return
	}
	inv := c.pool.Inventory()
	if deficit := target - liveFor(inv, key); deficit > 0 {
		if deficit > c.maxBootPerTick {
			deficit = c.maxBootPerTick
		}
		for i := 0; i < deficit; i++ {
			_, _ = c.pool.Boot(ctx, mk.Key) // best-effort: Tick retries
		}
	}
}

// Run drives Tick on interval until ctx is done — the manager-runnable
// form (the operator's leader-elected goroutine). Returns nil on ctx
// cancellation; a Tick error is logged-and-continued (level-triggered: the
// next tick re-derives everything from durable pool state).
func (c *Controller) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			_, _ = c.Tick(ctx)
		}
	}
}

// Targets returns the current EFFECTIVE target per key (the autoscaler's
// post-stabilization view) — the controller's desired state, for status
// surfaces and tests.
func (c *Controller) Targets() map[PoolKey]int {
	out := make(map[PoolKey]int, len(c.keys))
	for _, mk := range c.keys {
		out[mk.Key] = c.autoscaler.Current(mk.Key)
	}
	return out
}

// ManagedKeys returns the keys under management in registration order.
func (c *Controller) ManagedKeys() []ManagedKey {
	out := make([]ManagedKey, len(c.keys))
	copy(out, c.keys)
	return out
}
