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
//     the falsification catches). Run-RESERVED warmings (a cold claim's
//     dedicated boot) do NOT: they are the claim's own sandbox, not pool
//     warmth.
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

	// capacity is the SUPPLY-side ceiling input (ISI-4315): cluster idle
	// CPU, read fresh every tick. nil = no capacity awareness (pre-ISI-
	// 4315 behavior).
	capacity CapacitySource

	// warmRequestMilli is one warm pod's CPU request (milliCPUs) — the
	// unit the cap arithmetic counts in. <= 0 disables capping.
	warmRequestMilli int64

	// warmBudgetMilli bounds the TOTAL idle-warm CPU commitment (0 = no
	// budget ceiling; WarmCapacityConfig defaults it to a share of
	// allocatable).
	warmBudgetMilli int64

	// warmHeadroomMilli is cluster slack the pool keeps FREE for the next
	// Run cold boot instead of filling with warmth.
	warmHeadroomMilli int64

	// warmBootDeadline reaps unbound Warming boots older than it (ISI-
	// 4315: persistent Pending backlogs on saturated clusters). <= 0 =
	// DefaultWarmBootDeadline.
	warmBootDeadline time.Duration

	mu sync.Mutex // serializes Ticks against concurrent ReplenishKey
}

// CapacitySource reports the cluster's scheduler-fit CPU slack —
// Σ(node allocatable) − Σ(pod requests), milliCPUs — the supply-side input
// to the warm-target cap (ISI-4315). Production reads nodes+pods through
// the kube client once per tick; tests return step functions. An error
// means "unknown", and the controller FAILS OPEN that tick (keeps the
// policy target) rather than zeroing warmth on a transient read failure.
type CapacitySource func(ctx context.Context) (freeMilli int64, err error)

// WarmCapacityConfig is the supply-side cap configuration the operator
// wiring hands the controller (SetCapacity). Zero fields take safe
// defaults; requestMilli <= 0 disables capping entirely.
type WarmCapacityConfig struct {
	// RequestMilli is one warm pod's CPU request in milliCPUs (e.g. 500).
	RequestMilli int64
	// BudgetMilli caps the total idle-warm CPU commitment cluster-wide;
	// 0 = no budget ceiling.
	BudgetMilli int64
	// HeadroomMilli is slack kept free for the next Run cold boot; < 0 or
	// 0 defaults to RequestMilli (exactly one spare Run slot).
	HeadroomMilli int64
	// BootDeadline reaps stale unbound Warming boots; 0 = the default.
	BootDeadline time.Duration
}

// DefaultWarmBootDeadline is the default reap age for unbound Warming
// boots (ISI-4315). Generous vs the measured gVisor replenish p95 of
// ~1.7 s (ISI-2294) and the Kata placeholder of 15 s: a boot this old is
// Pending on a saturated cluster, not slowly pulling an image — destroy it
// so it stops holding queue position and pool-live count.
const DefaultWarmBootDeadline = 120 * time.Second

// SetCapacity installs the supply-side ceiling (ISI-4315). After this call
// every Tick caps the policy target at what the cluster can actually spare
// — idle warmth YIELDS to project Runs instead of saturating workers — and
// reaps stale warm boots.
func (c *Controller) SetCapacity(src CapacitySource, cfg WarmCapacityConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.capacity = src
	c.warmRequestMilli = cfg.RequestMilli
	c.warmBudgetMilli = cfg.BudgetMilli
	if cfg.HeadroomMilli > 0 {
		c.warmHeadroomMilli = cfg.HeadroomMilli
	} else {
		c.warmHeadroomMilli = cfg.RequestMilli
	}
	if cfg.BootDeadline > 0 {
		c.warmBootDeadline = cfg.BootDeadline
	} else {
		c.warmBootDeadline = DefaultWarmBootDeadline
	}
}

// WarmCapacityCap returns the supply-side ceiling on one key's UNBOUND
// warm count (Warming + Ready) given live cluster slack — the pure
// arithmetic behind Tick's cap (ISI-4315), pinned by controller tests:
//
//   - freeCap: keep every current unbound warm pod PLUS as many more as
//     fit in the slack beyond headroom; when slack drops below headroom
//     the cap sinks BELOW the current count and the tick sheds surplus
//     warmth (Run-sized steps), handing CPU back to project Runs.
//   - budgetCap: idle warmth may never commit more than BudgetMilli of
//     CPU cluster-wide, regardless of apparent slack.
//   - requestMilli <= 0 (unknown pod size) fails OPEN (returns the current
//     count): an unknown unit cannot be counted, and zeroing warmth on a
//     misconfiguration would cold-start every claim.
func WarmCapacityCap(unboundWarm int, freeMilli, headroomMilli, budgetMilli, requestMilli int64) int {
	if requestMilli <= 0 {
		return unboundWarm
	}
	if headroomMilli < 0 {
		headroomMilli = 0
	}
	allowed := int64(unboundWarm) + floorDiv(freeMilli-headroomMilli, requestMilli)
	if budgetMilli > 0 {
		if budgetCap := budgetMilli / requestMilli; budgetCap < allowed {
			allowed = budgetCap
		}
	}
	if allowed < 0 {
		return 0
	}
	return int(allowed)
}

// floorDiv divides a by b (b > 0) rounding DOWN — truncating division
// would round −500/500 up to 0 and hide exactly the sub-slot deficit the
// cap must shed (ISI-4315).
func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// capTarget folds the capacity ceiling into one key's effective target for
// this tick (supply-side, IMMEDIATE — the autoscaler's stabilization band
// governs the demand signal, but a hard capacity constraint must not wait
// three ticks while Runs sit Pending). The ceiling clamps DOWNWARD ONLY:
// slack can never raise the target above what the policy demands (the
// policy owns demand; capacity only vetoes). A capacity read error fails
// open. Returns the capped target.
func (c *Controller) capTarget(ctx context.Context, key PoolKey, target int) int {
	if c.capacity == nil || c.warmRequestMilli <= 0 {
		return target
	}
	freeMilli, err := c.capacity(ctx)
	if err != nil {
		return target // fail-open: unknown supply keeps the policy target
	}
	inv := c.pool.Inventory()
	if capped := WarmCapacityCap(liveFor(inv, key), freeMilli, c.warmHeadroomMilli, c.warmBudgetMilli, c.warmRequestMilli); capped < target {
		return capped
	}
	return target
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
		byKey:          make(map[PoolKey]ManagedKey, len(keys)),
		maxBootPerTick: 4,
	}
	// Default nil Pressure sources onto the copies the controller KEEPS:
	// Tick iterates c.keys and calls mk.Pressure — a nil left in c.keys by
	// a range-copy-only default nil-panics the leader-elected Run loop on
	// its first pass. A fresh slice is built so the caller's variadic
	// backing array is never mutated.
	def := make([]ManagedKey, len(keys))
	for i, mk := range keys {
		if mk.Pressure == nil {
			mk.Pressure = StaticPressure(0)
		}
		def[i] = mk
		c.byKey[mk.Key] = mk
	}
	c.keys = def
	pool.SetBindMiss(func(key PoolKey, _ RunClass) { c.ReplenishKey(context.Background(), key) })
	return c
}

// SetMaxBootPerTick overrides the per-tick boot fan-out bound (tests).
func (c *Controller) SetMaxBootPerTick(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.maxBootPerTick = n
}

// liveFor reads the pool's replenish-relevant count for key: Warming +
// Ready. Bound sandboxes are excluded (claimed capacity is not pool
// capacity; §9.3 replaces them after Release), and so are Reserved
// warmings — a claiming run's own dedicated boot materializing is that
// run's sandbox, not pool warmth; counting it would shrink the replenish
// deficit by one (and starve the floor entirely at MinReady=1).
func liveFor(inv map[PoolKey]Counts, key PoolKey) int {
	return inv[key].Warming + inv[key].Ready
}

// Tick runs one reconcile pass over every managed key and returns the
// effective targets it drove (keyed by pool key — deterministic iteration:
// managed order is preserved, not map order).
func (c *Controller) Tick(ctx context.Context) (map[PoolKey]int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := ctx.Err(); err != nil {
		// Shutting down: no boot fan-out on a dead context.
		return nil, err
	}

	// ISI-4315: reap stale unbound Warming boots FIRST. On a saturated
	// cluster replenish boots sit Pending/Insufficient-cpu indefinitely
	// (enforcement + adopt-or-reap keep re-arming them) — they count as
	// live forever, hold scheduler queue position, and steal freed CPU
	// slots. Destroying them below the deadline lets this tick's capacity
	// cap see the true warm count and the pool stop pretending to warm.
	for _, mk := range c.keys {
		if c.warmBootDeadline > 0 {
			c.pool.ReapStaleWarming(ctx, mk.Key, c.warmBootDeadline)
		}
	}

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
		// ISI-4315: the supply-side ceiling — even a policy-pinned target
		// (KSQUAD_WARM_POOL_TARGET=5 × 500m on 2×4-core workers) must not
		// saturate the cluster and starve the project Runs the pool exists
		// to serve. IMMEDIATE in both directions: no stabilization band on
		// a hard constraint.
		target = c.capTarget(ctx, mk.Key, target)
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
	if ctx.Err() != nil {
		return // shutting down: no boot fan-out on a dead context
	}
	target := c.autoscaler.Current(key)
	if target <= 0 {
		return
	}
	// ISI-4315: the bind-miss replenish obeys the supply-side ceiling too
	// — a cold boot just consumed capacity (or found none), and fanning
	// out warmth boots the cluster cannot fit is exactly the persistent
	// Pending backlog the cap exists to prevent.
	target = c.capTarget(ctx, key, target)
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
	c.mu.Lock()
	defer c.mu.Unlock()
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
