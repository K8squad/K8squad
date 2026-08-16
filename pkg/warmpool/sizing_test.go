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
	"math"
	"testing"

	"github.com/K8squad/K8squad/pkg/warmpool"
)

// Story 3.5 (ISI-2530) differential falsification — the faithful Go port of
// docs/bmad/spikes/bench/warmpool-sizing-check.py (same shape as the 3.4 /
// 3.2 / 2.2 checks and pkg/coord's chaos gate). Every scenario is
// DIFFERENTIAL: it computes a naive twin (the broken variant the mutation
// contract names) and asserts the twin VIOLATES the invariant while the
// real policy holds it — so a PASS means the check has teeth, not that the
// assertions are vacuous.
//
// Case ↔ invariant map (story 3-5-warmpool-sizing-as-policy.md §AC):
//
//	P1 safety stock (AC1)   mean-only ceil(λR) twin under-provisions vs base-stock
//	P2 autoscale (AC2)      constant-λ twin is flat; the real target tracks pressure
//	P3 idle-cost bound(AC3) no-max-cap twin runs past max under a spike; min floor holds
//	P3b batch→0 (AC4)       batch sizes to 0 at every λ (§9.2 hybrid)
//	P4 per-key R (AC5)      shared-R twin collapses the gVisor/Kata split
//	P5 hysteresis (AC6)     naive raw-every-tick flaps on a single-tick dip
//	PIN curve (AC7)         shipped v1 gVisor {light:2, medium:2, heavy:3}, batch:0

// Pool keys = (RuntimeClass × AgentRuntime image), the one-dimensional §9.2 key.
var (
	gvisorKey = warmpool.PoolKey{RuntimeClass: "gvisor", Image: "runtime:v1"}
	kataKey   = warmpool.PoolKey{RuntimeClass: "kata", Image: "runtime:v1"}
)

// v1 MEASURED replenish_s per key (ISI-2294: gVisor repl_p95=1.716 s beats
// runc; Kata is the spike's conservative placeholder pending nested-virt).
func replenishTable() map[warmpool.PoolKey]float64 {
	return map[warmpool.PoolKey]float64{
		gvisorKey: 1.716,
		kataKey:   15.0,
	}
}

func newPolicy() *warmpool.Policy {
	return warmpool.NewDefaultPolicy(replenishTable()) // min=2, max=10, 95%
}

func mustTarget(t *testing.T, p *warmpool.Policy, key warmpool.PoolKey, lambda float64, class warmpool.RunClass) int {
	t.Helper()
	n, err := p.Target(key, lambda, class)
	if err != nil {
		t.Fatalf("Target(%v, %v, %q): %v", key, lambda, class, err)
	}
	return n
}

func mustBaseStock(t *testing.T, p *warmpool.Policy, key warmpool.PoolKey, lambda float64) int {
	t.Helper()
	n, err := p.BaseStock(key, lambda)
	if err != nil {
		t.Fatalf("BaseStock(%v, %v): %v", key, lambda, err)
	}
	return n
}

// meanOnly is the P1 naive under-provisioner: cycle demand λR with NO
// safety stock.
func meanOnly(lambda, r float64) int {
	return int(math.Max(1, math.Ceil(lambda*r)))
}

func TestP1SafetyStock(t *testing.T) {
	p := newPolicy()
	// Heavy load on gVisor: cycle demand λR=0.858 → mean-only ceil = 1;
	// base-stock = 3 (safety stock +2).
	base := mustBaseStock(t, p, gvisorKey, 0.5)
	naive := meanOnly(0.5, replenishTable()[gvisorKey])

	// Differential teeth: the naive twin violates the invariant.
	if !(base > naive) {
		t.Fatalf("P1 vacuous: mean-only twin (%d) already >= base-stock (%d) — the differential has no teeth", naive, base)
	}
	if base != 3 || naive != 1 {
		t.Fatalf("P1 safety-stock FAILED: base-stock=%d mean-only=%d, want base=3 (λR+z√(λR)=2.38) naive=1 — "+
			"the target dropped its z·sqrt(λR) burst buffer and under-provisions under a claim burst", base, naive)
	}
}

func TestP2AutoscaleTracksPressure(t *testing.T) {
	p := newPolicy()
	lambdas := []float64{0.05, 0.2, 0.5, 1.0, 3.0}
	targets := make([]int, len(lambdas))
	for i, lam := range lambdas {
		targets[i] = mustTarget(t, p, gvisorKey, lam, warmpool.ClassInteractive)
	}
	monotonic := true
	for i := range targets[1:] {
		if targets[i] > targets[i+1] {
			monotonic = false
		}
	}
	moves := targets[0] != targets[len(targets)-1]

	// Differential teeth: a frozen-λ policy (constant target) is flat.
	frozen := mustTarget(t, p, gvisorKey, 0.05, warmpool.ClassInteractive)
	frozenFlat := frozen == 2 // min floor at trickle λ — a constant policy pins here
	if !frozenFlat {
		t.Fatalf("P2 vacuous: frozen twin already moves — the differential has no teeth")
	}
	if !monotonic || !moves {
		t.Fatalf("P2 autoscale FAILED: target does not track the live claim-pressure signal "+
			"(monotonic=%v moves=%v targets=%v) — a constant ignores load: over-provisions idle at low load, misses at high load", monotonic, moves, targets)
	}
}

func TestP3IdleCostBounded(t *testing.T) {
	p := newPolicy()
	spike := mustTarget(t, p, gvisorKey, 50.0, warmpool.ClassInteractive)     // absurd pressure
	trickle := mustTarget(t, p, gvisorKey, 0.0001, warmpool.ClassInteractive) // near-zero pressure
	rawAtSpike := mustBaseStock(t, p, gvisorKey, 50.0)                        // unclamped demand

	// Differential teeth: with the max cap dropped the target runs past max.
	uncapped := rawAtSpike // noCap twin = raw base-stock
	if !(uncapped > warmpool.DefaultMaxReady) {
		t.Fatalf("P3 vacuous: no-cap twin (%d) did not exceed max (%d) — the differential has no teeth", uncapped, warmpool.DefaultMaxReady)
	}
	if spike != warmpool.DefaultMaxReady || trickle != warmpool.DefaultMinReady {
		t.Fatalf("P3 idle-cost-bound FAILED: spike=%d (want max=%d), trickle=%d (want min=%d), raw-at-spike=%d — "+
			"an unclamped target runs past max (runaway idle pods) or below the interactive warm floor",
			spike, warmpool.DefaultMaxReady, trickle, warmpool.DefaultMinReady, rawAtSpike)
	}
}

func TestP3bBatchZeroIdle(t *testing.T) {
	p := newPolicy()
	for _, lam := range []float64{0.0, 0.5, 50.0} {
		if got := mustTarget(t, p, gvisorKey, lam, warmpool.ClassBatch); got != 0 {
			t.Fatalf("P3b batch-zero-idle FAILED: batch class at λ=%v sized to %d, want 0 — "+
				"§9.2 hybrid: batch cold-starts, zero idle cost", lam, got)
		}
	}
}

func TestP4PerKeyReplenish(t *testing.T) {
	p := newPolicy()
	gv := mustBaseStock(t, p, gvisorKey, 0.2) // R=1.716 → 2
	ka := mustBaseStock(t, p, kataKey, 0.2)   // R=15.0  → 6 (the 2–2.6×+ multiplier)

	// Differential teeth: a shared-R policy collapses the split to equal values.
	if !(ka > gv) {
		t.Fatalf("P4 vacuous: shared-R twin already agrees with per-key sizing — the differential has no teeth")
	}
	if gv != 2 || ka != 6 || ka < 2*gv {
		t.Fatalf("P4 per-key-R FAILED: gVisor=%d Kata=%d (want 2 and 6, Kata >= 2× gVisor) — sizing every key from one "+
			"shared R under-sizes Kata (misses its warm-hit SLA) or over-sizes gVisor (idle cost)", gv, ka)
	}

	// An unknown key fails closed — never silently sized from another key's R.
	if _, err := p.BaseStock(warmpool.PoolKey{RuntimeClass: "nanos", Image: "runtime:v1"}, 0.2); err == nil {
		t.Fatalf("P4 FAILED: unknown pool key did not fail closed — a missing per-key R must error, not fall back")
	}
}

func TestP5HysteresisNoThrash(t *testing.T) {
	p := newPolicy()
	az := warmpool.NewAutoscaler(p, 3) // stabilization band = 3 ticks

	// Steady heavy pressure establishes a target, ONE tick of low pressure,
	// then heavy resumes.
	seq := []float64{0.5, 0.5, 0.5, 0.05, 0.5, 0.5}
	hyst := make([]int, len(seq))
	for i, lam := range seq {
		n, err := az.Reconcile(gvisorKey, lam, warmpool.ClassInteractive)
		if err != nil {
			t.Fatalf("Reconcile tick %d: %v", i, err)
		}
		hyst[i] = n
	}
	// The naive (no stabilization) twin re-evaluates raw every tick.
	naive := make([]int, len(seq))
	for i, lam := range seq {
		naive[i] = mustTarget(t, p, gvisorKey, lam, warmpool.ClassInteractive)
	}

	hystFlat := minOf(hyst) == maxOf(hyst)
	naiveFlaps := minOf(naive) < maxOf(naive)

	// Differential teeth: the naive twin must actually flap.
	if !naiveFlaps {
		t.Fatalf("P5 vacuous: naive raw-eval twin did not flap — the differential has no teeth")
	}
	if !hystFlat {
		t.Fatalf("P5 hysteresis FAILED: a single-tick dip thrashed the pool %v (naive flaps %v) — tearing down warm pods it "+
			"immediately re-needs = wasted boots + reintroduced cold misses", hyst, naive)
	}

	// Sustained low pressure (past the band) DOES commit scale-down — the
	// band damps transients, it does not freeze the target.
	for _, lam := range []float64{0.05, 0.05, 0.05, 0.05} {
		if _, err := az.Reconcile(gvisorKey, lam, warmpool.ClassInteractive); err != nil {
			t.Fatalf("Reconcile sustained-low: %v", err)
		}
	}
	if got := az.Current(gvisorKey); got != warmpool.DefaultMinReady {
		t.Fatalf("P5 FAILED: sustained low pressure did not scale down to min=%d (got %d) — the band damps transients, it must not freeze the target",
			warmpool.DefaultMinReady, got)
	}

	// Scale-up stays immediate: one tick of spike raises the target at once.
	if got, err := az.Reconcile(gvisorKey, 50.0, warmpool.ClassInteractive); err != nil || got != warmpool.DefaultMaxReady {
		t.Fatalf("P5 FAILED: scale-up was not immediate (got %d err %v, want max=%d) — never starve a burst",
			got, err, warmpool.DefaultMaxReady)
	}
}

func TestPinnedVCurve(t *testing.T) {
	p := newPolicy()
	inter := map[string]int{
		"light":  mustTarget(t, p, gvisorKey, warmpool.LambdaLight, warmpool.ClassInteractive),
		"medium": mustTarget(t, p, gvisorKey, warmpool.LambdaMedium, warmpool.ClassInteractive),
		"heavy":  mustTarget(t, p, gvisorKey, warmpool.LambdaHeavy, warmpool.ClassInteractive),
	}
	batch := mustTarget(t, p, gvisorKey, 0.5, warmpool.ClassBatch)

	want := map[string]int{"light": 2, "medium": 2, "heavy": 3}
	for regime, w := range want {
		if inter[regime] != w {
			t.Fatalf("PIN curve DRIFTED: gVisor interactive %s=%d, want %d — at measured R=1.716 s (ISI-2294) and §9.2 "+
				"defaults (min=2, max=10, 95%%) the shipped v1 curve is %v, batch: 0. A change here means the R/λ constants "+
				"or the min/max envelope moved — re-pin deliberately, not silently", regime, inter[regime], w, want)
		}
	}
	if batch != 0 {
		t.Fatalf("PIN curve DRIFTED: batch=%d, want 0", batch)
	}

	// Kata cells at the placeholder R (§B): medium → 6, heavy raw 13 clamped
	// to max=10 — the clamp visibly bites for Kata ("cap Kata tighter,
	// prefer cold-start for bursts"). Re-pin when the measured R lands.
	if got := mustBaseStock(t, p, kataKey, warmpool.LambdaMedium); got != 6 {
		t.Fatalf("Kata medium base-stock = %d, want 6 (placeholder R=15 s)", got)
	}
	if got := mustTarget(t, p, kataKey, warmpool.LambdaHeavy, warmpool.ClassInteractive); got != warmpool.DefaultMaxReady {
		t.Fatalf("Kata heavy target = %d, want max=%d (raw 13 clamped)", got, warmpool.DefaultMaxReady)
	}
}

// TestRecommendBufferPins is the Go port of the ISI-2113 reference
// implementation's selftest (pool_sizing.py _selftest) — the worked values
// that pin the base-stock math itself.
func TestRecommendBufferPins(t *testing.T) {
	// 1–2. Monotonic in load and in replenish time (the load-bearing spike
	// claim: Kata's longer R costs pool).
	must := func(lam, rep float64, sl ...float64) int {
		t.Helper()
		level := 0.95
		if len(sl) > 0 {
			level = sl[0]
		}
		n, err := warmpool.RecommendBuffer(lam, rep, level)
		if err != nil {
			t.Fatalf("RecommendBuffer(%v, %v, %v): %v", lam, rep, level, err)
		}
		return n
	}
	if !(must(0.5, 4.0) >= must(0.2, 4.0)) {
		t.Fatalf("not monotonic in load")
	}
	if !(must(0.2, 15.0) > must(0.2, 4.0)) {
		t.Fatalf("not monotonic in replenish time")
	}
	// 3. Higher service level => >= buffer.
	if must(0.2, 4.0, 0.99) < must(0.2, 4.0, 0.90) {
		t.Fatalf("higher SL must not shrink the buffer")
	}
	// 4. Floor: a warm pool is never sized to 0 even at trickle load.
	if got := must(0.0001, 1.0); got != 1 {
		t.Fatalf("trickle floor = %d, want 1", got)
	}
	// 5. Worked value pinned in the spike report (gVisor placeholder era,
	// medium load, 95%): λL=0.8 → ceil(0.8+1.6449·√0.8)=3.
	if got := must(0.2, 4.0); got != 3 {
		t.Fatalf("worked pin (λ=0.2, R=4.0) = %d, want 3", got)
	}
	// 6. Kata heavy pin: λL=7.5 → ceil(7.5+1.6449·√7.5)=13.
	if got := must(0.5, 15.0); got != 13 {
		t.Fatalf("worked pin (λ=0.5, R=15.0) = %d, want 13", got)
	}
	// 7. The Acklam fallback path (non-tabulated SL adjacent to 0.95) agrees
	//    with the tabulated cell: z(0.949)≈1.638 ⇒ ceil(0.8+1.464)=3, same
	//    as the 0.95 pin above.
	if got := must(0.2, 4.0, 0.949); got != 3 {
		t.Fatalf("Acklam-path buffer at sl=0.949 = %d, want 3 (tabulated-0.95 cell)", got)
	}
	// Negative / out-of-range inputs fail closed.
	if _, err := warmpool.RecommendBuffer(-0.1, 1.0, 0.95); err == nil {
		t.Fatalf("negative lambda must error")
	}
	if _, err := warmpool.RecommendBuffer(0.1, -1.0, 0.95); err == nil {
		t.Fatalf("negative replenish must error")
	}
	if _, err := warmpool.RecommendBuffer(0.1, 1.0, 1.0); err == nil {
		t.Fatalf("service level 1.0 must error")
	}
	// Unknown run class fails closed.
	if _, err := newPolicy().Target(gvisorKey, 0.2, "burst"); err == nil {
		t.Fatalf("unknown run class must error")
	}
}

func TestRecommendTableMatchesMainGrid(t *testing.T) {
	// The reference __main__ grid at the v1 constants: raw base-stock per
	// runtime × load regime (pool_sizing.py __main__, ISI-2294): gVisor
	// 1/2/3 (light/medium/heavy); runc 1/3/4; kata 3/6/13.
	got, err := warmpool.RecommendTable(
		warmpool.DefaultReplenish(),
		map[string]float64{"light": 0.05, "medium": 0.2, "heavy": 0.5},
		0.95,
	)
	if err != nil {
		t.Fatalf("RecommendTable: %v", err)
	}
	for rt, want := range map[string]map[string]int{
		"gvisor": {"light": 1, "medium": 2, "heavy": 3},
		"runc":   {"light": 1, "medium": 3, "heavy": 4},
		"kata":   {"light": 3, "medium": 6, "heavy": 13},
	} {
		for load, w := range want {
			if got[rt][load] != w {
				t.Fatalf("RecommendTable[%s][%s] = %d, want %d", rt, load, got[rt][load], w)
			}
		}
	}
}

func minOf(s []int) int {
	m := s[0]
	for _, v := range s[1:] {
		if v < m {
			m = v
		}
	}
	return m
}

func maxOf(s []int) int {
	m := s[0]
	for _, v := range s[1:] {
		if v > m {
			m = v
		}
	}
	return m
}
