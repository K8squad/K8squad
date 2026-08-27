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

// Package warmpool is the warm-pool sizing POLICY (Story 3.5 / ISI-2530,
// arch §9.2 "policy-driven, not fixed" — FR-C4, NFR-SCALE2). It is the ONE
// place the warm-pool target N is derived; the Story 3.4 controller
// mechanism (claim-time bind, scale-up trigger, replenish-toward-target)
// consumes the target produced here opaquely.
//
// The policy is exactly the §9.2 / ISI-2113 sentence, a faithful Go port of
// the spike's reference implementation (ksquad docs/bmad/spikes/bench/
// pool_sizing.py, self-tested there and pinned here by the same worked
// values):
//
//   - the interactive target ready-buffer is the base-stock level
//     N = ceil(λ·R + z·sqrt(λ·R)) — λ = live claim-pressure signal
//     (`warmpool.claim.pressure`, obs §5.3), R = the key's OWN measured
//     replenish time (`warmpool.replenish.duration`), z = warm-hit service
//     level (1.6449 ≈ 95%). λ·R is cycle demand; z·sqrt(λ·R) is safety
//     stock for Poisson burstiness — dropping it under-provisions and
//     misses the warm-hit SLA under a claim burst.
//   - the target AUTOSCALES on the live pressure signal: λ is an input to
//     every reconcile, never a baked constant (FR-C4).
//   - idle cost is BOUNDED: the target is clamped to [min, max]; the max
//     cap stops a pressure spike from spawning runaway idle pods, the min
//     floor keeps interactive pools warm (min >= 1 — 0 Ready is cold).
//   - batch/non-interactive keys size to target=0 (§9.2 hybrid regime:
//     batch cold-starts, paying zero idle cost).
//   - each (RuntimeClass × AgentRuntime image) key is sized by ITS OWN
//     measured R — Kata's ~4x longer replenish forces a ~2–2.6x larger
//     pool than gVisor at the same λ and SLA.
//   - scale-down is damped by a stabilization band (an HPA-style window)
//     so a transient pressure dip does not thrash the pool; scale-up
//     remains immediate (never starve a burst).
//
// The shipped v1 gVisor curve is PINNED to the measured ISI-2294 numbers
// (repl_p95 = 1.716 s on cluster `observable-agentsandbox`, gVisor runsc
// sentry-verified): {light: 2, medium: 2, heavy: 3}, batch: 0 — locked by
// the differential falsification in sizing_test.go as a regression fence.
// Kata's R=15.0 s remains the spike's conservative PLACEHOLDER pending a
// nested-virt measurement; the policy is correct for Kata the moment the
// measured value lands.
package warmpool

import (
	"fmt"
	"math"
	"sort"
)

// Normal quantiles for common warm-hit service levels (one-sided) — the
// same tabulated values as the ISI-2113 reference implementation.
var zTable = map[float64]float64{
	0.90: 1.2816,
	0.95: 1.6449,
	0.99: 2.3263,
}

// RecommendBuffer returns the base-stock target ready-count for a warm
// pool — the ISI-2113 formula N = ceil(λL + z·sqrt(λL)), the direct Go
// port of pool_sizing.recommend_buffer.
//
// lambdaCPS    peak claim arrival rate (claims/second); must be >= 0.
// replenishS   teardown-and-replace time to a fresh Ready pod (seconds);
//
//	must be >= 0.
//
// serviceLevel target P(pool_hit=warm), 0 < sl < 1 (0.95 ≈ 95% warm-hit).
//
// The result floors at 1: a "warm" pool with 0 Ready pods is cold by
// definition (§9.2). Callers clamp to [min, max] via Policy.Target.
func RecommendBuffer(lambdaCPS, replenishS float64, serviceLevel float64) (int, error) {
	if lambdaCPS < 0 || replenishS < 0 {
		return 0, fmt.Errorf("warmpool: rates and durations must be non-negative (lambda=%v, replenish=%v)", lambdaCPS, replenishS)
	}
	if serviceLevel <= 0 || serviceLevel >= 1 {
		return 0, fmt.Errorf("warmpool: service_level must be in (0,1), got %v", serviceLevel)
	}
	z, ok := zTable[serviceLevel]
	if !ok {
		// Rational-approximation fallback (Acklam) so arbitrary SLAs work —
		// agrees with the table to <1e-2 at the standard levels.
		z = zFromServiceLevel(serviceLevel)
	}
	cycleDemand := lambdaCPS * replenishS // μ
	safety := z * math.Sqrt(cycleDemand)  // z·σ, Poisson σ = sqrt(μ)
	return int(math.Max(1, math.Ceil(cycleDemand+safety))), nil
}

// zFromServiceLevel approximates the one-sided normal inverse-CDF for sl in
// (0,1) using Acklam's rational approximation — the same algorithm (and
// coefficients) as the reference implementation's _z_from_sl.
func zFromServiceLevel(sl float64) float64 {
	a := [...]float64{-3.969683028665376e+01, 2.209460984245205e+02, -2.759285104469687e+02,
		1.383577518672690e+02, -3.066479806614716e+01, 2.506628277459239e+00}
	b := [...]float64{-5.447609879822406e+01, 1.615858368580409e+02, -1.556989798598866e+02,
		6.680131188771972e+01, -1.328068155288572e+01}
	c := [...]float64{-7.784894002430293e-03, -3.223964580411365e-01, -2.400758277161838e+00,
		-2.549732539343734e+00, 4.374664141464968e+00, 2.938163982698783e+00}
	d := [...]float64{7.784695709041462e-03, 3.224671290700398e-01, 2.445134137142996e+00,
		3.754408661907416e+00}
	const plow, phigh = 0.02425, 1 - 0.02425
	if sl < plow {
		q := math.Sqrt(-2 * math.Log(sl))
		return (((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) /
			((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	}
	if sl <= phigh {
		q := sl - 0.5
		r := q * q
		return (((((a[0]*r+a[1])*r+a[2])*r+a[3])*r+a[4])*r + a[5]) * q /
			(((((b[0]*r+b[1])*r+b[2])*r+b[3])*r+b[4])*r + 1)
	}
	q := math.Sqrt(-2 * math.Log(1-sl))
	return -(((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) /
		((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
}

// PoolKey is the §9.2 warm-pool key: (RuntimeClass × AgentRuntime image ×
// team namespace × capabilityHash). Every key is sized by its OWN measured
// replenish time; sharing one R across keys under-sizes slow runtimes
// (Kata) or over-sizes fast ones (gVisor).
//
// The namespace dimension is the ADR-044 step-9 tenancy fix: sandbox pods
// boot in the RUN'S TEAM NAMESPACE so per-Run Role binding, pod-level
// NetworkPolicy and quota all hold (§12.1) — pool inventory is therefore
// per-namespace by construction. The capabilityHash dimension (step 7)
// keeps identical capability envelopes sharing stock while a changed
// envelope (new toolchain pin, MCP filter) cold-starts fresh.
type PoolKey struct {
	RuntimeClass string
	Image        string
	// Namespace is the team namespace warm pods boot in. Empty falls back
	// to the provisioner default (pre-Epic-C callers only; classified
	// Runs always carry it).
	Namespace string
	// CapabilityHash is the Run's resolved capability-manifest hash
	// (ADR-044 step 5/7); empty = the bare, capability-free posture.
	CapabilityHash string
}

// RunClass routes the §9.2 hybrid regime: interactive Runs draw from the
// warm pool; batch/non-interactive Runs cold-start at target=0 (zero idle
// cost, and sidesteps reuse-contamination).
type RunClass string

const (
	// ClassInteractive draws from the warm pool (min-floored, max-capped
	// base-stock target).
	ClassInteractive RunClass = "interactive"

	// ClassBatch cold-starts: target 0 regardless of pressure.
	ClassBatch RunClass = "batch"
)

// v1 LOCKED replenish times (seconds) — measured p95 by claim-latency-bench
// (ITERS=10) on cluster `observable-agentsandbox` (Proxmox+CAPI, k8s
// v1.35.3, containerd 1.7.25, gVisor RuntimeClass gvisor->runsc verified
// sentry-real), per ISI-2292/ISI-2294 on 2026-08-12 (arch §21).
//
// gVisor BEATS runc (1.716 s vs 3.560 s) — no pool-size tax; warm-claim
// p50 0.110 s / p95 0.135 s clears NFR-PERF1 (S9) by ~15–37x.
//
// Kata is NOT measured (handler not installed on the bench cluster); the
// value is the spike's CONSERVATIVE PLACEHOLDER. Remeasure on a
// nested-virt cluster before locking any Kata default.
const (
	ReplenishGVisorSeconds = 1.716 // MEASURED (ISI-2294)
	ReplenishRuncSeconds   = 3.560 // MEASURED (ISI-2294) — trusted-dev only
	ReplenishKataSeconds   = 15.0  // PLACEHOLDER (spike §6) — remeasure
)

// DefaultReplenish returns the v1 replenish-time table keyed by
// RuntimeClass, mirroring the reference implementation's __main__ constants.
func DefaultReplenish() map[string]float64 {
	return map[string]float64{
		"gvisor": ReplenishGVisorSeconds,
		"runc":   ReplenishRuncSeconds,
		"kata":   ReplenishKataSeconds,
	}
}

// §9.2 default load regimes (peak claim rates, claims/second) for the v1
// curve: light 3/min, medium 12/min, heavy 30/min.
const (
	LambdaLight  = 0.05
	LambdaMedium = 0.20
	LambdaHeavy  = 0.50
)

// DefaultMinReady / DefaultMaxReady / DefaultServiceLevel are the §9.2 gVisor
// v1 defaults: interactive min=2, max=10, 95% warm-hit; batch target=0.
const (
	DefaultMinReady     = 2
	DefaultMaxReady     = 10
	DefaultServiceLevel = 0.95
)

// Policy is the sizing policy over per-key measured replenish times with a
// [min, max] idle-cost envelope and warm-hit service level. It is pure
// arithmetic over the live pressure signal — no cluster, no clock.
type Policy struct {
	// ReplenishSByKey maps each (RuntimeClass × image) key to ITS OWN
	// measured replenish time R (seconds). A key absent from this map fails
	// closed with an error — it is never silently sized from another
	// key's R.
	ReplenishSByKey map[PoolKey]float64

	// MinReady is the interactive floor (>= 1: an interactive pool always
	// keeps a warm floor — 0 Ready is cold).
	MinReady int

	// MaxReady is the idle-cost ceiling: no pressure spike can drive the
	// target past it.
	MaxReady int

	// ServiceLevel is the target P(pool_hit=warm) (0 < sl < 1).
	ServiceLevel float64
}

// NewDefaultPolicy returns the §9.2 v1 default policy (min=2, max=10, 95%)
// over the given per-key replenish table.
func NewDefaultPolicy(replenishSByKey map[PoolKey]float64) *Policy {
	return &Policy{
		ReplenishSByKey: replenishSByKey,
		MinReady:        DefaultMinReady,
		MaxReady:        DefaultMaxReady,
		ServiceLevel:    DefaultServiceLevel,
	}
}

// BaseStock returns the raw base-stock target for one key at the observed
// claim rate — RecommendBuffer(λ, R_key, sl), carrying its z·sqrt(λR)
// safety stock, NOT the bare cycle demand ceil(λR).
func (p *Policy) BaseStock(key PoolKey, observedLambda float64) (int, error) {
	r, ok := p.ReplenishSByKey[key]
	if !ok {
		return 0, fmt.Errorf("warmpool: no measured replenish time for pool key %+v (size every key by its own R — never a shared fallback)", key)
	}
	return RecommendBuffer(observedLambda, r, p.ServiceLevel)
}

// Target returns the policy target for a (key, live-pressure, run-class):
// batch → 0 (§9.2 hybrid); interactive → the live-λ base-stock clamped to
// [min, max] so idle cost is bounded at BOTH ends.
func (p *Policy) Target(key PoolKey, observedLambda float64, class RunClass) (int, error) {
	if class == ClassBatch {
		return 0, nil // zero idle cost — batch cold-starts (§9.2)
	}
	if class != ClassInteractive {
		return 0, fmt.Errorf("warmpool: unknown run class %q (want %q or %q)", class, ClassInteractive, ClassBatch)
	}
	base, err := p.BaseStock(key, observedLambda) // derived from the LIVE pressure signal
	if err != nil {
		return 0, err
	}
	return clamp(base, p.MinReady, p.MaxReady), nil
}

func clamp(v, lo, hi int) int {
	return int(math.Max(float64(lo), math.Min(float64(v), float64(hi))))
}

// DefaultStabilizationTicks is the default scale-down stabilization band
// (reconcile ticks a lower desired target must persist before the pool
// scales DOWN). Scale-up is always immediate.
const DefaultStabilizationTicks = 3

// Autoscaler drives the §9.2 min/target/max reconcile off the live
// `warmpool.claim.pressure` signal, with a scale-DOWN stabilization band so
// a transient dip does not thrash the pool (tearing down warm pods the
// pool immediately re-needs = wasted boots + reintroduced cold misses).
//
// The SandboxPool reconciler loop is leader-elected (arch §5.2 — one owner,
// no racing resizers); this type is that leader's per-key target state and
// is not safe for concurrent use.
type Autoscaler struct {
	policy *Policy

	// stabilizationTicks is how many consecutive ticks the desired target
	// must sit below current before scale-down commits.
	stabilizationTicks int

	// current is the effective target ready-count per key.
	current map[PoolKey]int

	// lowTicks counts consecutive ticks the desired target sat below
	// current (the stabilization band).
	lowTicks map[PoolKey]int
}

// NewAutoscaler returns an Autoscaler over policy with the given
// scale-down stabilization band (<= 0 falls back to the default).
func NewAutoscaler(policy *Policy, stabilizationTicks int) *Autoscaler {
	if stabilizationTicks <= 0 {
		stabilizationTicks = DefaultStabilizationTicks
	}
	return &Autoscaler{
		policy:             policy,
		stabilizationTicks: stabilizationTicks,
		current:            make(map[PoolKey]int),
		lowTicks:           make(map[PoolKey]int),
	}
}

// Reconcile folds one live pressure reading for a key into its effective
// target: scale UP immediately (never starve a burst), scale DOWN only
// after the desired target has sat below current for stabilizationTicks
// consecutive ticks. Returns the effective target the 3.4 mechanism
// replenishes toward.
func (a *Autoscaler) Reconcile(key PoolKey, observedLambda float64, class RunClass) (int, error) {
	desired, err := a.policy.Target(key, observedLambda, class)
	if err != nil {
		return 0, err
	}
	cur, ok := a.current[key]
	if !ok {
		cur = desired
	}
	switch {
	case desired > cur:
		a.lowTicks[key] = 0
		cur = desired // scale UP immediately
	case desired < cur:
		a.lowTicks[key]++
		if a.lowTicks[key] >= a.stabilizationTicks { // sustained low pressure only
			cur = desired
			a.lowTicks[key] = 0
		}
	default:
		a.lowTicks[key] = 0
	}
	a.current[key] = cur
	return cur, nil
}

// Current returns the effective target ready-count for a key (0 if the
// key has not been reconciled yet). It is what the 3.4 mechanism
// replenishes toward between ticks.
func (a *Autoscaler) Current(key PoolKey) int {
	return a.current[key]
}

// RecommendTable renders the runtime × load-regime recommended-buffer grid
// (the Go port of the reference table()): for each runtime and load name,
// the raw base-stock RecommendBuffer(λ, R_runtime, sl). Deterministic
// output: runtimes and load names are iterated in sorted order.
func RecommendTable(replenishByRuntime map[string]float64, lambdas map[string]float64, serviceLevel float64) (map[string]map[string]int, error) {
	rtNames := make([]string, 0, len(replenishByRuntime))
	for rt := range replenishByRuntime {
		rtNames = append(rtNames, rt)
	}
	sort.Strings(rtNames)
	loadNames := make([]string, 0, len(lambdas))
	for name := range lambdas {
		loadNames = append(loadNames, name)
	}
	sort.Strings(loadNames)

	out := make(map[string]map[string]int, len(rtNames))
	for _, rt := range rtNames {
		row := make(map[string]int, len(loadNames))
		for _, name := range loadNames {
			n, err := RecommendBuffer(lambdas[name], replenishByRuntime[rt], serviceLevel)
			if err != nil {
				return nil, err
			}
			row[name] = n
		}
		out[rt] = row
	}
	return out, nil
}
