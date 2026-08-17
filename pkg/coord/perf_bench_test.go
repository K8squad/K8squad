//go:build chaos

// Story 14.3 / ISI-2706 — the P1–P4 Go Benchmark* funcs the perf.yml lane runs
// head-vs-main to feed the RELATIVE regression gate (pkg/perfgate). They live
// under the `chaos` build tag so they reuse the real-Postgres fixtures
// (dsnOrFatal/openDB/resetProdSchema/seedProdItems, widened to testing.TB) and
// never compile into the default `go test` path — the same isolation the
// TestSpine chaos suite uses.
//
// Skip-with-reason discipline (AC7, Epic-14 convention): a leg whose source
// signal or harness has not landed SKIPS WITH A PRINTED REASON, never a silent
// drop. P1/P4 skip-with-reason when DATABASE_URL is unset (no Postgres locally);
// P3 skips unconditionally until the SSE progress bus (cmd/apiserver) lands.
//
// The benchmarks emit the SLI as a custom metric (p95 ms, or the isolation
// ratio) via b.ReportMetric so benchstat compares the RIGHT number head-vs-main,
// not just ns/op. Numeric thresholds stay spike-gated (ISI-2113); this lane ships
// the gate MECHANISM against defaults — the pass/fail teeth live in
// pkg/perfgate + perf-regression-gate-check.py (mirrored by TestPerfGate).
package coord_test

import (
	"context"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/K8squad/K8squad/pkg/coord"
	"github.com/K8squad/K8squad/pkg/events"
	"github.com/K8squad/K8squad/pkg/warmpool"
)

// perfDSNOrSkip returns the Postgres DSN, or SKIPS the benchmark with a printed
// reason when DATABASE_URL is unset. Unlike dsnOrFatal (which is a required-gate
// FATAL), a perf leg with no Postgres is an AC7 skip-with-reason, not a failure:
// perf.yml provides the service, a laptop run legitimately does not.
func perfDSNOrSkip(b *testing.B) string {
	b.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		b.Skip("SKIP-WITH-REASON: DATABASE_URL unset — P1/P4 need a live Postgres " +
			"(perf.yml provides the services.postgres DSN; a local run without it is skipped, not failed).")
	}
	return dsn
}

// p95ms returns the nearest-rank p95 of a duration sample, in milliseconds —
// the same percentile rule as perfgate.Pctl / the python anchor.
func p95ms(samples []time.Duration) float64 {
	if len(samples) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), samples...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	k := (95*len(s) + 99) / 100 // ceil(0.95*N), 1-indexed
	if k < 1 {
		k = 1
	}
	return float64(s[k-1].Microseconds()) / 1000.0
}

// ---------------------------------------------------------------------------
// P1 — claim latency (S9/NFR-PERF1). Times the production §6.2 SKIP LOCKED claim
// (ProdClaimer.ClaimNext) against the SHIPPED schema on real Postgres. The p95
// is the gated SLI (perfgate.P1Gate, relative vs main).
// ---------------------------------------------------------------------------
func BenchmarkP1ClaimLatency(b *testing.B) {
	dsn := perfDSNOrSkip(b)
	db := openDB(b, dsn)

	// Re-provision + seed enough claimable items for the whole run (one claim
	// pops one item; a claim past exhaustion returns ok=false and would skew the
	// sample). Reseed in a stop-timer block whenever the lane is drained.
	resetProdSchema(b, db)
	seedProdItems(b, db, b.N+8)

	pc, err := coord.NewProdClaimer(db, coord.DefaultProdConfig())
	if err != nil {
		b.Fatalf("NewProdClaimer: %v", err)
	}

	ctx := context.Background()
	samples := make([]time.Duration, 0, b.N)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		t0 := time.Now()
		_, _, ok, err := pc.ClaimNext(ctx, "perf-agent", prodRunUUID(i+1), "")
		d := time.Since(t0)
		if err != nil {
			b.Fatalf("ClaimNext[%d]: %v", i, err)
		}
		if !ok {
			// Lane drained (or fully contended in a parallel variant) — refill
			// without charging the timer, so every sample is a real claim.
			b.StopTimer()
			seedProdItems(b, db, 64)
			b.StartTimer()
			continue
		}
		samples = append(samples, d)
	}
	b.StopTimer()
	b.ReportMetric(p95ms(samples), "p95-ms")
}

// ---------------------------------------------------------------------------
// P2 — warm-pool ready-count (FR-C1). Pure compute (no Postgres): the base-stock
// / autoscale convergence the interactive-warm guarantee rides on. Always runs;
// there is no source signal to skip on. The perfgate P2 gate is on the observed
// cold-start rate, exercised by TestPerfGate; this benchmark protects the
// sizing-math hot path from silent slowdown.
// ---------------------------------------------------------------------------
func BenchmarkP2WarmPoolTarget(b *testing.B) {
	policy := warmpool.NewDefaultPolicy(map[warmpool.PoolKey]float64{
		{RuntimeClass: "gvisor", Image: "claude"}: warmpool.ReplenishGVisorSeconds,
		{RuntimeClass: "runc", Image: "opencode"}: warmpool.ReplenishRuncSeconds,
	})
	as := warmpool.NewAutoscaler(policy, 3)
	key := warmpool.PoolKey{RuntimeClass: "gvisor", Image: "claude"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// A varying observed arrival rate drives the base-stock + stabilization
		// path each iteration (mod keeps it deterministic, no RNG).
		lambda := 0.5 + float64(i%20)*0.25
		if _, err := as.Reconcile(key, lambda, warmpool.ClassInteractive); err != nil {
			b.Fatalf("Reconcile: %v", err)
		}
	}
}

// ---------------------------------------------------------------------------
// P3 — SSE throughput (NFR-USE). The emit→deliver progress bus lives behind the
// apiserver (cmd/apiserver), which has not landed in code yet (only planned in
// Dockerfile.apiserver / README). SKIP-WITH-REASON until it does — never a
// silent green. The perfgate P3 gate (latency drift + zero-tolerance dropped
// events) is already proven by TestPerfGate against fixed vectors.
// ---------------------------------------------------------------------------
func BenchmarkP3SSEThroughput(b *testing.B) {
	b.Skip("SKIP-WITH-REASON: SSE progress bus (cmd/apiserver emit→deliver) not " +
		"implemented yet — no publish/subscribe surface to measure. Gate logic is " +
		"held by perfgate.P3Gate/TestPerfGate; wire this leg when Epic 8.2 apiserver lands.")
}

// ---------------------------------------------------------------------------
// P4 — outbox delivery lag (§17.4). ISOLATION, proved branch-internal: the write
// path (a claim co-committing one coord.outbox row via WithOutboxCapture) must
// be INDEPENDENT of delivery health. Two arms drive the same write path while a
// background relay drains the outbox with a HEALTHY publisher vs a SICK (slow +
// erroring) one; perfgate.P4Gate asserts sick/healthy write p95 ≤ 1.5x.
// ---------------------------------------------------------------------------
func BenchmarkP4OutboxDeliveryLag(b *testing.B) {
	dsn := perfDSNOrSkip(b)
	db := openDB(b, dsn)

	// Provision 0001 spine + 0003 outbox (the §17.4 seam substrate).
	mustExec(b, db, `DROP SCHEMA IF EXISTS coord CASCADE`)
	mustExec(b, db, coordMigrationSQL(b))
	mustExec(b, db, outboxMigrationSQL(b))

	// Split b.N across the two arms so each measures the SAME write path under a
	// different delivery health. The write path is ClaimNext+WithOutboxCapture:
	// one txn that INSERTs the outbox row and never waits on the relay.
	half := b.N / 2
	if half < 1 {
		half = 1
	}
	seedProdItems(b, db, 2*half+16)

	claimer, err := coord.NewProdClaimer(db, coord.DefaultProdConfig(), coord.WithOutboxCapture())
	if err != nil {
		b.Fatalf("NewProdClaimer(WithOutboxCapture): %v", err)
	}
	ctx := context.Background()

	measureArm := func(pub events.Publisher, n, seedFrom int) float64 {
		relay, err := events.NewRelay(events.RelayConfig{
			Store:        events.NewSQLStore(db),
			Publisher:    pub,
			PollInterval: 5 * time.Millisecond, // drain aggressively so the sick arm is genuinely under delivery pressure
			BatchLimit:   32,
		})
		if err != nil {
			b.Fatalf("NewRelay: %v", err)
		}
		rctx, cancel := context.WithCancel(ctx)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); _ = relay.Run(rctx) }()

		samples := make([]time.Duration, 0, n)
		for i := 0; i < n; i++ {
			t0 := time.Now()
			_, _, ok, err := claimer.ClaimNext(ctx, "perf-agent", prodRunUUID(seedFrom+i+1), "")
			d := time.Since(t0)
			if err != nil {
				cancel()
				wg.Wait()
				b.Fatalf("ClaimNext[%d]: %v", i, err)
			}
			if ok {
				samples = append(samples, d)
			}
		}
		cancel()
		wg.Wait()
		return p95ms(samples)
	}

	b.ResetTimer()
	healthy := measureArm(healthyPublisher{}, half, 0)
	sick := measureArm(&sickPublisher{delay: 25 * time.Millisecond}, half, half)
	b.StopTimer()

	if healthy <= 0 {
		b.Skip("SKIP-WITH-REASON: healthy-arm produced no committed-claim samples (lane drained) — cannot form the §17.4 isolation ratio this run.")
	}
	ratio := sick / healthy
	b.ReportMetric(healthy, "healthy-p95-ms")
	b.ReportMetric(sick, "sick-p95-ms")
	b.ReportMetric(ratio, "iso-ratio")
}

// healthyPublisher acks instantly: delivery keeps pace, outbox depth stays low.
type healthyPublisher struct{}

func (healthyPublisher) Publish(ctx context.Context, subject string, data []byte) error { return nil }
func (healthyPublisher) Close() error                                                   { return nil }

// sickPublisher models a slow/failing plugin: every Publish sleeps then errors,
// so rows are never flushed and the backlog grows. §17.4 requires this to leave
// the WRITE path (ClaimNext) unaffected — the relay owns no write-txn state.
type sickPublisher struct{ delay time.Duration }

func (s *sickPublisher) Publish(ctx context.Context, subject string, data []byte) error {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
	}
	return context.DeadlineExceeded // never acks → rows stay unflushed, backlog climbs
}
func (s *sickPublisher) Close() error { return nil }
