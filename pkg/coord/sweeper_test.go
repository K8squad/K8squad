// sweeper_test.go — unit tests for the reclaim sweeper LOOP (Story 2.4 /
// ISI-3104), driven by a mock clock and a fake reclaim source so they run with no
// Postgres in the normal `go test` lane. The DURABILITY of the reclaim statement
// itself (fence bump, release, done-guard, exactly-once under concurrency) is
// proven separately against a real Postgres in sweeper_chaos_test.go; here we pin
// the loop's control flow: metrics emission, OnReclaim/OnError dispatch, empty-
// batch handling, and clean context cancellation.
package coord

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeReclaim is a scripted reclaimSource: each call pops the next result from
// results (a batch or an error), recording how many times it was invoked.
type fakeReclaim struct {
	results []reclaimResult
	calls   int
}

type reclaimResult struct {
	batch []Reclaimed
	err   error
}

func (f *fakeReclaim) ReclaimExpired(ctx context.Context) ([]Reclaimed, error) {
	i := f.calls
	f.calls++
	if i >= len(f.results) {
		return nil, nil // nothing left to reclaim: idle cycles
	}
	r := f.results[i]
	return r.batch, r.err
}

// countingMetrics records the sweeper metric calls for assertion.
type countingMetrics struct {
	cycles    int
	reclaims  int
	durations []float64
}

func (m *countingMetrics) IncSweepCycle()                 { m.cycles++ }
func (m *countingMetrics) AddSweepReclaims(n int)         { m.reclaims += n }
func (m *countingMetrics) ObserveSweepDuration(s float64) { m.durations = append(m.durations, s) }
func (m *countingMetrics) Signals() []string              { return nil }

// newTestSweeper builds a Sweeper over a fake source with an INJECTED tick channel
// the test drives by hand, so cycles fire deterministically with no wall-clock
// sleep. Returns the sweeper, the tick channel, and the metrics sink.
func newTestSweeper(src reclaimSource) (*Sweeper, chan time.Time, *countingMetrics) {
	tick := make(chan time.Time)
	m := &countingMetrics{}
	sw := &Sweeper{
		store:    src,
		interval: time.Second, // ignored — the injected ticker governs
		metrics:  m,
		ticker:   func(time.Duration) (<-chan time.Time, func()) { return tick, func() {} },
	}
	return sw, tick, m
}

// TestSweeperCycleReclaimsAndNotifies: a cycle that reclaims a batch increments the
// cycle + reclaim counters, records a duration, and calls OnReclaim with the batch.
func TestSweeperCycleReclaimsAndNotifies(t *testing.T) {
	batch := []Reclaimed{{Item: 3, Fence: 7}, {Item: 5, Fence: 2}}
	src := &fakeReclaim{results: []reclaimResult{{batch: batch}}}
	sw, _, m := newTestSweeper(src)

	var gotBatch []Reclaimed
	sw.OnReclaim = func(_ context.Context, r []Reclaimed) { gotBatch = r }

	sw.sweepOnce(context.Background())

	if m.cycles != 1 {
		t.Fatalf("cycles = %d, want 1", m.cycles)
	}
	if m.reclaims != 2 {
		t.Fatalf("reclaims = %d, want 2", m.reclaims)
	}
	if len(m.durations) != 1 {
		t.Fatalf("durations recorded = %d, want 1", len(m.durations))
	}
	if len(gotBatch) != 2 || gotBatch[0].Item != 3 || gotBatch[1].Fence != 2 {
		t.Fatalf("OnReclaim got %+v, want the reclaimed batch", gotBatch)
	}
}

// TestSweeperEmptyCycleIsQuiet: a cycle that reclaims nothing still counts as a
// cycle (and records a duration) but does NOT touch the reclaim counter or fire
// OnReclaim — an idle sweeper must not spam a resource-layer fencer with empty
// batches.
func TestSweeperEmptyCycleIsQuiet(t *testing.T) {
	src := &fakeReclaim{results: []reclaimResult{{batch: nil}}}
	sw, _, m := newTestSweeper(src)

	fired := false
	sw.OnReclaim = func(context.Context, []Reclaimed) { fired = true }

	sw.sweepOnce(context.Background())

	if m.cycles != 1 {
		t.Fatalf("cycles = %d, want 1", m.cycles)
	}
	if m.reclaims != 0 {
		t.Fatalf("reclaims = %d, want 0 on an empty cycle", m.reclaims)
	}
	if len(m.durations) != 1 {
		t.Fatalf("durations recorded = %d, want 1 (an empty cycle is still timed)", len(m.durations))
	}
	if fired {
		t.Fatal("OnReclaim fired on an empty batch — must be skipped")
	}
}

// TestSweeperErrorCycleIsInert: a cycle whose scan errors routes the error to
// OnError and does NOT fire OnReclaim or bump the reclaim counter. Driven
// synchronously (no loop goroutine) so the assertions are race-free.
func TestSweeperErrorCycleIsInert(t *testing.T) {
	boom := errors.New("db down")
	src := &fakeReclaim{results: []reclaimResult{{err: boom}}}
	sw, _, m := newTestSweeper(src)

	var gotErr error
	sw.OnError = func(err error) { gotErr = err }
	fired := false
	sw.OnReclaim = func(context.Context, []Reclaimed) { fired = true }

	sw.sweepOnce(context.Background())

	if !errors.Is(gotErr, boom) {
		t.Fatalf("OnError got %v, want %v", gotErr, boom)
	}
	if fired {
		t.Fatal("OnReclaim fired on an errored cycle — must be skipped")
	}
	if m.cycles != 1 {
		t.Fatalf("cycles = %d, want 1 (an errored cycle still counts as a cycle)", m.cycles)
	}
	if m.reclaims != 0 {
		t.Fatalf("reclaims = %d, want 0 (the errored cycle adds nothing)", m.reclaims)
	}
	if len(m.durations) != 1 {
		t.Fatalf("durations = %d, want 1 (an errored cycle is still timed)", len(m.durations))
	}
}

// TestSweeperRunSurvivesError: the loop does NOT terminate when a cycle errors —
// a later tick still reclaims. Callbacks hand results back over channels so the
// assertions never touch state the Run goroutine writes (race-free).
func TestSweeperRunSurvivesError(t *testing.T) {
	boom := errors.New("db down")
	src := &fakeReclaim{results: []reclaimResult{
		{err: boom},
		{batch: []Reclaimed{{Item: 1, Fence: 1}}},
	}}
	// nop metrics: the Run goroutine writes them, so we assert only via channels.
	tick := make(chan time.Time)
	sw := &Sweeper{
		store:    src,
		interval: time.Second,
		metrics:  nopSweeperMetrics{},
		ticker:   func(time.Duration) (<-chan time.Time, func()) { return tick, func() {} },
	}
	errCh := make(chan error, 1)
	reclaimCh := make(chan int, 1)
	sw.OnError = func(err error) { errCh <- err }
	sw.OnReclaim = func(_ context.Context, r []Reclaimed) { reclaimCh <- len(r) }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sw.Run(ctx) }()

	tick <- time.Now() // cycle 1: error
	if err := <-errCh; !errors.Is(err, boom) {
		t.Fatalf("cycle 1 OnError got %v, want %v", err, boom)
	}
	tick <- time.Now() // cycle 2: the loop survived and reclaims
	if n := <-reclaimCh; n != 1 {
		t.Fatalf("cycle 2 reclaimed %d, want 1", n)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
}

// TestSweeperRunStopsOnContextCancel: Run returns ctx.Err() promptly on
// cancellation, with no tick delivered.
func TestSweeperRunStopsOnContextCancel(t *testing.T) {
	src := &fakeReclaim{}
	sw, _, _ := newTestSweeper(src)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sw.Run(ctx) }()

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
	if src.calls != 0 {
		t.Fatalf("ReclaimExpired called %d times, want 0 (cancelled before any tick)", src.calls)
	}
}

// TestSweeperTickerStoppedOnExit: the injected ticker's stop func is called when
// Run returns, so a real time.Ticker would not leak.
func TestSweeperTickerStoppedOnExit(t *testing.T) {
	stopped := false
	tick := make(chan time.Time)
	sw := &Sweeper{
		store:    &fakeReclaim{},
		interval: time.Second,
		metrics:  nopSweeperMetrics{},
		ticker:   func(time.Duration) (<-chan time.Time, func()) { return tick, func() { stopped = true } },
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sw.Run(ctx) }()
	cancel()
	<-done

	if !stopped {
		t.Fatal("ticker stop func was not called on Run exit — a real ticker would leak")
	}
}

// TestSweepConfigValidate pins the fail-closed binding checks.
func TestSweepConfigValidate(t *testing.T) {
	base := SweepConfig{WorkItem: "wi", Claim: "c", OpenState: "open", DoneState: "done", Interval: time.Second, Batch: 10}
	if err := base.validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	cases := map[string]func(SweepConfig) SweepConfig{
		"no work_item":  func(c SweepConfig) SweepConfig { c.WorkItem = ""; return c },
		"no claim":      func(c SweepConfig) SweepConfig { c.Claim = ""; return c },
		"no open":       func(c SweepConfig) SweepConfig { c.OpenState = ""; return c },
		"no done":       func(c SweepConfig) SweepConfig { c.DoneState = ""; return c },
		"zero interval": func(c SweepConfig) SweepConfig { c.Interval = 0; return c },
		"neg interval":  func(c SweepConfig) SweepConfig { c.Interval = -time.Second; return c },
	}
	for name, mut := range cases {
		if err := mut(base).validate(); err == nil {
			t.Errorf("%s: validate returned nil, want error", name)
		}
	}
}

// TestSweepConfigBatchParam: 0/negative Batch means unlimited (a large LIMIT), a
// positive Batch passes through.
func TestSweepConfigBatchParam(t *testing.T) {
	if got := (SweepConfig{Batch: 0}).batchParam(); got <= 1<<20 {
		t.Fatalf("batchParam(0) = %d, want a large 'unlimited' sentinel", got)
	}
	if got := (SweepConfig{Batch: -5}).batchParam(); got <= 1<<20 {
		t.Fatalf("batchParam(-5) = %d, want a large 'unlimited' sentinel", got)
	}
	if got := (SweepConfig{Batch: 64}).batchParam(); got != 64 {
		t.Fatalf("batchParam(64) = %d, want 64", got)
	}
}

// TestNopSweeperMetricsSignals: the no-op emitter still names the full signal set.
func TestNopSweeperMetricsSignals(t *testing.T) {
	got := nopSweeperMetrics{}.Signals()
	want := []string{"coord_sweep_cycles_total", "coord_sweep_reclaims_total", "coord_sweep_duration_seconds"}
	if len(got) != len(want) {
		t.Fatalf("Signals() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Signals()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
