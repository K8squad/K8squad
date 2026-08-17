// Story 14.3 (ISI-2700 / ISI-2706) — L3 performance regression benchmark for
// the P2 warm-pool SLI: ready-count convergence.
//
// P2 measures the cost of folding a live pressure trace into the effective
// ready-count target via the §9.2 Autoscaler (scale-up-now / scale-down-after-
// stabilization). It is pure CPU — no Postgres, no cluster — so it carries no
// build tag and runs on any runner in the perf lane. The trace is a full
// burst→sustain→decay cycle so the bench exercises BOTH regimes (immediate
// scale-up AND the stabilization-gated scale-down), not just the cheap steady
// state.
//
// Interactive is the measured path (draws from the pool). A batch control run
// is included because §9.2 exempts batch from the pool (target 0 regardless of
// pressure) — a regression that accidentally sized batch pools would show up as
// the batch arm doing base-stock math it must not do.
//
// Relative-regression only (benchstat head vs cached `main`); no absolute
// nanosecond threshold lives here by design.

package warmpool_test

import (
	"testing"

	"github.com/K8squad/K8squad/pkg/warmpool"
)

// convergenceTrace is one burst→sustain→decay pressure cycle (claims/sec).
// Rising λ drives immediate scale-up; the long low tail exercises the
// stabilization-gated scale-down band. Kept deterministic so the bench is
// reproducible across runs (no RNG — benchstat needs a stable workload).
func convergenceTrace() []float64 {
	tr := make([]float64, 0, 64)
	for l := 0.0; l <= 8.0; l += 0.5 { // ramp up
		tr = append(tr, l)
	}
	for i := 0; i < 16; i++ { // sustain at peak
		tr = append(tr, 8.0)
	}
	for l := 8.0; l >= 0.0; l -= 0.5 { // decay down (stabilization band)
		tr = append(tr, l)
	}
	return tr
}

// BenchmarkP2WarmPoolConvergence times one full Autoscaler run over the
// burst→sustain→decay trace for an interactive pool — the P2 headline SLI.
func BenchmarkP2WarmPoolConvergence(b *testing.B) {
	key := warmpool.PoolKey{RuntimeClass: "gvisor", Image: "perf"}
	policy := warmpool.NewDefaultPolicy(map[warmpool.PoolKey]float64{key: warmpool.ReplenishGVisorSeconds})
	trace := convergenceTrace()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		as := warmpool.NewAutoscaler(policy, warmpool.DefaultStabilizationTicks)
		for _, lambda := range trace {
			if _, err := as.Reconcile(key, lambda, warmpool.ClassInteractive); err != nil {
				b.Fatalf("Reconcile(interactive, λ=%.1f): %v", lambda, err)
			}
		}
	}
}

// BenchmarkP2WarmPoolConvergenceBatch is the §9.2 control: batch cold-starts,
// so every reconcile must return target 0 and never touch base-stock math.
// A regression that started pooling batch would move this number.
func BenchmarkP2WarmPoolConvergenceBatch(b *testing.B) {
	key := warmpool.PoolKey{RuntimeClass: "gvisor", Image: "perf"}
	policy := warmpool.NewDefaultPolicy(map[warmpool.PoolKey]float64{key: warmpool.ReplenishGVisorSeconds})
	trace := convergenceTrace()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		as := warmpool.NewAutoscaler(policy, warmpool.DefaultStabilizationTicks)
		for _, lambda := range trace {
			got, err := as.Reconcile(key, lambda, warmpool.ClassBatch)
			if err != nil {
				b.Fatalf("Reconcile(batch, λ=%.1f): %v", lambda, err)
			}
			if got != 0 {
				b.Fatalf("batch reconcile returned target %d, want 0 (§9.2 batch cold-starts)", got)
			}
		}
	}
}
