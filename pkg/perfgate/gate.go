// Package perfgate is the Go landing of the Story 14.3 (ISI-2700) L3 performance
// regression GATE — the RELATIVE, not-brittle-absolute comparator the perf.yml
// lane runs against the four headline SLIs (P1 claim latency, P2 warm-pool
// ready-count, P3 SSE throughput, P4 outbox delivery lag).
//
// It is a faithful 1:1 translation of the language-neutral falsification anchor
// docs/bmad/spikes/bench/perf-regression-gate-check.py (green; mutations M1–M4
// each RED), exactly as TestSpine mirrors chaos-harness.py (ISI-2347). The Go
// verdict table in gate_test.go asserts the SAME GREEN/RED verdict per scenario
// the python pins, so the two cannot drift.
//
// The thing under test is NOT a latency number — final numeric curves are
// spike-gated (ISI-2113). The thing under test is the GATE LOGIC: given
// (baseline-on-main, candidate-on-branch) measured on the SAME runner in the
// SAME job, does the comparator fail the build on a real regression and stay
// green on hardware noise? The tolerances below are the only thing the gate
// hard-codes; everything else is measured head-vs-main in the same job.
package perfgate

import (
	"errors"
	"fmt"
	"sort"
)

// Relative-gate knobs. These are TOLERANCES on the ratio-to-main, NOT absolute
// SLO numbers (those land after ISI-2113). Kept byte-for-byte equal to the
// python anchor's P1_TOL / P2_COLDRATE_FLOOR / P3_LAT_TOL / P4_ISO_FACTOR.
const (
	// P1Tol — claim-latency p95 may drift +20% vs main before RED.
	P1Tol = 0.20
	// P2ColdRateFloor — interactive cold-start rate must stay ~0 (allow 2% jitter).
	P2ColdRateFloor = 0.02
	// P3LatTol — SSE emit→deliver p95 may drift +20% vs main before RED.
	P3LatTol = 0.20
	// P4IsoFactor — write p95 under a sick plugin must stay within 1.5x of the
	// healthy write p95 (§17.4 isolation).
	P4IsoFactor = 1.50
)

// ErrMissingBaseline is the build-FAILURE signal: a perf run with no pinned main
// baseline must fail the build, never silent-green (the M2 tooth / F2-trap
// analogue of 14.2). Callers treat it distinctly from a passed/failed gate.
var ErrMissingBaseline = errors.New("perfgate: missing baseline")

// Pctl is the nearest-rank percentile (q in 0..100). Deterministic; no
// interpolation, no floats-in-the-rank — the same rule as the python pctl so
// the two comparators agree bit-for-bit on the same sample vector.
func Pctl(xs []float64, q int) (float64, error) {
	if len(xs) == 0 {
		return 0, errors.New("perfgate: percentile of empty sample")
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	// nearest-rank: ceil(q/100 * N), 1-indexed.
	k := (q*len(s) + 99) / 100 // ceil division
	if k < 1 {
		k = 1
	}
	return s[k-1], nil
}

// RelRegression is the signed fractional change vs main. >0 == slower/worse.
func RelRegression(mainV, branchV float64) float64 {
	return (branchV - mainV) / mainV
}

// RequireBaseline is the M2 tooth: a baseline-less run must FAIL THE BUILD. It
// returns ErrMissingBaseline (wrapped with the SLI name) so the driver aborts
// rather than passing silently.
func RequireBaseline(baseline []float64, name string) error {
	if len(baseline) == 0 {
		return fmt.Errorf("%w: %s", ErrMissingBaseline, name)
	}
	return nil
}

// P1Gate — claim latency (S9/NFR-PERF1). p95 drift vs main beyond P1Tol is RED;
// an improvement never gates. A missing baseline is a build FAILURE
// (ErrMissingBaseline), not a passed==false gate.
func P1Gate(mainSamples, branchSamples []float64) (passed bool, detail string, err error) {
	if err := RequireBaseline(mainSamples, "P1 claim-latency baseline"); err != nil {
		return false, "", err
	}
	mp95, err := Pctl(mainSamples, 95)
	if err != nil {
		return false, "", err
	}
	bp95, err := Pctl(branchSamples, 95)
	if err != nil {
		return false, "", err
	}
	reg := RelRegression(mp95, bp95)
	return reg <= P1Tol, fmt.Sprintf("p95 drift %+.1f%% (tol +%.0f%%)", reg*100, P1Tol*100), nil
}

// P2Gate — warm-pool ready-count (FR-C1). Batch Runs are ALLOWED to cold-start
// (expected, §5 P2 row); only the interactive tier must draw warm.
func P2Gate(interactiveColdRate float64, tier string) (passed bool, detail string) {
	if tier == "batch" {
		return true, "batch tier exempt (cold-start expected)"
	}
	passed = interactiveColdRate <= P2ColdRateFloor
	return passed, fmt.Sprintf("interactive cold-start rate %.1f%% (floor %.0f%%)",
		interactiveColdRate*100, P2ColdRateFloor*100)
}

// P3Gate — SSE throughput (NFR-USE). emit→deliver p95 drift beyond P3LatTol is
// RED; ANY dropped event at target concurrency is RED (zero-tolerance, never a
// relative budget — the M3 tooth). Missing baseline is a build FAILURE.
func P3Gate(mainSamples, branchSamples []float64, droppedEvents int) (passed bool, detail string, err error) {
	if err := RequireBaseline(mainSamples, "P3 SSE-latency baseline"); err != nil {
		return false, "", err
	}
	if droppedEvents != 0 {
		return false, fmt.Sprintf("%d dropped event(s) at target concurrency (zero-tolerance)", droppedEvents), nil
	}
	mp95, err := Pctl(mainSamples, 95)
	if err != nil {
		return false, "", err
	}
	bp95, err := Pctl(branchSamples, 95)
	if err != nil {
		return false, "", err
	}
	reg := RelRegression(mp95, bp95)
	return reg <= P3LatTol, fmt.Sprintf("emit→deliver p95 drift %+.1f%%, 0 dropped", reg*100), nil
}

// P4Gate — outbox delivery lag (§17.4). The invariant is ISOLATION, proved
// WITHIN the candidate: a slow/failing plugin must not push backpressure onto
// the write path. Compared branch-internal, not vs main. Gating on this RATIO
// (not absolute outbox depth) is the M4 tooth.
func P4Gate(writeP95Healthy, writeP95Sick float64) (passed bool, detail string) {
	ratio := writeP95Sick / writeP95Healthy
	return ratio <= P4IsoFactor, fmt.Sprintf("write p95 sick/healthy = %.2fx (max %.2fx)", ratio, P4IsoFactor)
}
