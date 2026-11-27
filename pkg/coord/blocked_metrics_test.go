// blocked_metrics_test.go — unit tests for the Story 13.3 blocked-by gauge
// (ISI-2916). Like sweeper_test.go these pin the SAMPLER's control flow with a
// fake blockedCounter and no Postgres; the SQL of CountBlocked itself is proven
// against a real Postgres in blocked_metrics_chaos_test.go.
package coord

import (
	"context"
	"errors"
	"testing"
)

// fakeBlocked is a scripted blockedCounter: it returns a fixed count (or error)
// and records how many times it was sampled.
type fakeBlocked struct {
	n     int64
	err   error
	calls int
}

func (f *fakeBlocked) CountBlocked(context.Context) (int64, error) {
	f.calls++
	return f.n, f.err
}

// recordingBlockedMetrics captures every SetBlocked value for assertion.
type recordingBlockedMetrics struct{ sets []int64 }

func (m *recordingBlockedMetrics) SetBlocked(n int64) { m.sets = append(m.sets, n) }
func (m *recordingBlockedMetrics) Signals() []string  { return []string{"coord_blocked_items"} }

// TestSweeperSamplesBlockedEveryCycle: once a sampler is wired, every cycle sets
// the gauge from the sampled count — including an EMPTY reclaim cycle (the blocked
// population is orthogonal to what was reclaimed).
func TestSweeperSamplesBlockedEveryCycle(t *testing.T) {
	src := &fakeReclaim{results: []reclaimResult{{batch: nil}}} // empty reclaim cycle
	sw, _, _ := newTestSweeper(src)
	blk := &fakeBlocked{n: 4}
	bm := &recordingBlockedMetrics{}
	sw.SetBlockedSampler(blk, bm)

	sw.sweepOnce(context.Background())

	if blk.calls != 1 {
		t.Fatalf("CountBlocked calls = %d, want 1 (sampled even on an empty reclaim cycle)", blk.calls)
	}
	if len(bm.sets) != 1 || bm.sets[0] != 4 {
		t.Fatalf("SetBlocked sets = %v, want [4]", bm.sets)
	}
}

// TestSweeperBlockedSamplerDefaultsOff: with no sampler wired (the default), a
// cycle never touches a blocked gauge — existing callers are unaffected.
func TestSweeperBlockedSamplerDefaultsOff(t *testing.T) {
	src := &fakeReclaim{results: []reclaimResult{{batch: []Reclaimed{{Item: 1, Fence: 1}}}}}
	sw, _, _ := newTestSweeper(src)
	// no SetBlockedSampler call
	sw.sweepOnce(context.Background()) // must not panic / nil-deref
}

// TestSweeperBlockedSampleErrorIsInert: a CountBlocked error routes to OnError and
// does NOT set the gauge — the reclaim cycle must still proceed.
func TestSweeperBlockedSampleErrorIsInert(t *testing.T) {
	src := &fakeReclaim{results: []reclaimResult{{batch: nil}}}
	sw, _, m := newTestSweeper(src)
	blk := &fakeBlocked{err: errors.New("db blip")}
	bm := &recordingBlockedMetrics{}
	sw.SetBlockedSampler(blk, bm)

	var gotErr error
	sw.OnError = func(err error) { gotErr = err }

	sw.sweepOnce(context.Background())

	if gotErr == nil {
		t.Fatal("CountBlocked error was not routed to OnError")
	}
	if len(bm.sets) != 0 {
		t.Fatalf("SetBlocked called on a count error: %v", bm.sets)
	}
	if m.cycles != 1 {
		t.Fatalf("reclaim cycle did not proceed after a blocked-sample error: cycles=%d", m.cycles)
	}
}

// TestBlockedMetricsSignals: both the Prometheus and nop emitters name exactly the
// one §13.3 signal (the completeness contract), and nop SetBlocked is inert.
func TestBlockedMetricsSignals(t *testing.T) {
	for _, bm := range []BlockedMetrics{PrometheusBlockedMetrics{}, nopBlockedMetrics{}} {
		sig := bm.Signals()
		if len(sig) != 1 || sig[0] != "coord_blocked_items" {
			t.Fatalf("Signals() = %v, want [coord_blocked_items]", sig)
		}
	}
	nopBlockedMetrics{}.SetBlocked(99) // inert, must not panic
}
