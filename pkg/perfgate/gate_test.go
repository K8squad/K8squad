package perfgate

import (
	"errors"
	"testing"
)

// TestPerfGate is the Go mirror of perf-regression-gate-check.py's run_scenarios()
// — one subtest per scenario, each pinning the SAME expected GREEN/RED verdict the
// python anchor pins (AC8: the Go perf lane is a faithful 1:1 translation of the
// anchor's verdict table, as TestSpine is of chaos-harness.py). No wall-clock, no
// RNG: samples are fixed vectors; a "slow runner" is a deterministic scalar
// multiply. This runs in EVERY CI lane (no build tag, no Postgres) — it is the
// gate's teeth, provable now against defaults.
//
// If any tooth is filed off in gate.go (widen P1Tol → M1; drop RequireBaseline →
// M2; tolerate dropped>0 in P3Gate → M3; gate P4 on absolute depth → M4) the
// matching subtest below goes RED, exactly as the python mutations re-RED the
// anchor.

// MAIN_P1: a representative claim-latency sample vector (ms) measured on `main`,
// p95 ~ 42. Identical to the python fixture.
var mainP1 = []float64{12, 14, 15, 16, 18, 19, 20, 22, 24, 26, 28, 31, 34, 38, 42}

func scale(xs []float64, k float64) []float64 {
	out := make([]float64, len(xs))
	for i, x := range xs {
		out[i] = x * k
	}
	return out
}

func addAll(xs []float64, d float64) []float64 {
	out := make([]float64, len(xs))
	for i, x := range xs {
		out[i] = x + d
	}
	return out
}

// green asserts the gate PASSED (merge allowed); red asserts it FAILED the build.
func green(t *testing.T, name string, passed bool, err error, detail string) {
	t.Helper()
	if err != nil {
		t.Fatalf("[%s] expected GREEN, gate errored: %v", name, err)
	}
	if !passed {
		t.Fatalf("[%s] expected GREEN, gate said RED: %s", name, detail)
	}
}

func red(t *testing.T, name string, passed bool, err error, detail string) {
	t.Helper()
	if err != nil {
		t.Fatalf("[%s] expected RED (gate false), gate errored: %v", name, err)
	}
	if passed {
		t.Fatalf("[%s] expected RED, gate said GREEN: %s", name, detail)
	}
}

func TestPerfGate(t *testing.T) {
	// -- P1: no change (noise within tol) -> GREEN (positive control) --
	t.Run("p1_no_change", func(t *testing.T) {
		ok, d, err := P1Gate(mainP1, addAll(mainP1, 1))
		green(t, "p1_no_change", ok, err, d)
	})

	// -- P1: a genuine +50% p95 regression -> RED (the teeth; M1 kills this) --
	t.Run("p1_real_regression", func(t *testing.T) {
		ok, d, err := P1Gate(mainP1, scale(mainP1, 1.5))
		red(t, "p1_real_regression", ok, err, d)
	})

	// -- P1: whole runner 1.6x slower, main re-measured same job -> ratio ~1 -> GREEN.
	//        A brittle ABSOLUTE gate would false-RED here; the relative gate rides through. --
	t.Run("p1_uniform_hw_slowdown", func(t *testing.T) {
		slowMain := scale(mainP1, 1.6)
		slowBranch := scale(mainP1, 1.6)
		ok, d, err := P1Gate(slowMain, slowBranch)
		green(t, "p1_uniform_hw_slowdown", ok, err, d)
	})

	// -- P1: a real regression LAYERED ON the slow runner is still caught -> RED --
	t.Run("p1_regression_on_slow_hw", func(t *testing.T) {
		slowMain := scale(mainP1, 1.6)
		ok, d, err := P1Gate(slowMain, scale(scale(mainP1, 1.6), 1.5))
		red(t, "p1_regression_on_slow_hw", ok, err, d)
	})

	// -- P1: an improvement (faster) is never gated -> GREEN --
	t.Run("p1_improvement", func(t *testing.T) {
		ok, d, err := P1Gate(mainP1, scale(mainP1, 0.7))
		green(t, "p1_improvement", ok, err, d)
	})

	// -- P1: missing baseline -> build FAILURE (M2 kills this) --
	t.Run("p1_missing_baseline", func(t *testing.T) {
		_, _, err := P1Gate(nil, mainP1)
		if !errors.Is(err, ErrMissingBaseline) {
			t.Fatalf("[p1_missing_baseline] expected fail-fast ErrMissingBaseline, got err=%v", err)
		}
	})

	// -- P2: interactive draws warm -> GREEN --
	t.Run("p2_interactive_warm", func(t *testing.T) {
		ok, d := P2Gate(0.0, "interactive")
		green(t, "p2_interactive_warm", ok, nil, d)
	})

	// -- P2: a regression makes interactive Runs cold-start (18%) -> RED --
	t.Run("p2_interactive_coldstart_regression", func(t *testing.T) {
		ok, d := P2Gate(0.18, "interactive")
		red(t, "p2_interactive_coldstart_regression", ok, nil, d)
	})

	// -- P2: batch tier cold-starts -> GREEN (expected, exempt) --
	t.Run("p2_batch_coldstart_exempt", func(t *testing.T) {
		ok, d := P2Gate(0.40, "batch")
		green(t, "p2_batch_coldstart_exempt", ok, nil, d)
	})

	// -- P3: bounded latency, zero drops -> GREEN --
	t.Run("p3_clean", func(t *testing.T) {
		ok, d, err := P3Gate(mainP1, addAll(mainP1, 2), 0)
		green(t, "p3_clean", ok, err, d)
	})

	// -- P3: one dropped event at target concurrency -> RED (zero-tolerance; M3 kills this) --
	t.Run("p3_dropped_events", func(t *testing.T) {
		ok, d, err := P3Gate(mainP1, mainP1, 1)
		red(t, "p3_dropped_events", ok, err, d)
	})

	// -- P3: latency p95 blows out +60% even with zero drops -> RED --
	t.Run("p3_latency_regression", func(t *testing.T) {
		ok, d, err := P3Gate(mainP1, scale(mainP1, 1.6), 0)
		red(t, "p3_latency_regression", ok, err, d)
	})

	// -- P4: sick plugin, write path unaffected (1.1x) -> GREEN (isolation holds) --
	t.Run("p4_isolation_holds", func(t *testing.T) {
		ok, d := P4Gate(20.0, 22.0)
		green(t, "p4_isolation_holds", ok, nil, d)
	})

	// -- P4: sick plugin leaks backpressure, write p95 3x -> RED (isolation broken; M4 kills this) --
	t.Run("p4_isolation_break", func(t *testing.T) {
		ok, d := P4Gate(20.0, 60.0)
		red(t, "p4_isolation_break", ok, nil, d)
	})
}

// TestPctlNearestRank pins the percentile rule against the python's nearest-rank
// so the two comparators cannot silently diverge on the same vector.
func TestPctlNearestRank(t *testing.T) {
	p95, err := Pctl(mainP1, 95)
	if err != nil {
		t.Fatal(err)
	}
	if p95 != 42 {
		t.Fatalf("nearest-rank p95 of mainP1 = %v, want 42 (ceil(0.95*15)=15th element)", p95)
	}
	if _, err := Pctl(nil, 95); err == nil {
		t.Fatal("percentile of empty sample must error")
	}
}
