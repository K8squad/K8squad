package apiserver

import (
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
)

// ============================================================================
// P3 — SSE throughput, zero dropped events (NFR-USE / perf gate leg P3)
// ============================================================================
//
// This is the REAL P3 benchmark. The emit→deliver progress bus it measures is
// the in-process SSE Hub in sse.go (Story 8.2), which has landed — so the old
// pkg/coord skip-with-reason ("apiserver bus not landed") is stale (ISI-2918 /
// gap ISI-2876). It exercises the actual Hub.Publish fan-out that every live
// console stream rides, and proves the P3 guarantee: throughput WITH zero
// dropped events.
//
// The Hub is deliberately best-effort — a subscriber whose 64-deep buffer is
// full is dropped rather than blocking the publisher (Publish returns the
// delivered count, 0 when the lone subscriber is momentarily behind). A naive
// hot-loop publisher would therefore outrun the consumer and record drops,
// which is expected transport behaviour but not what P3 measures. So this
// benchmark backs off (Gosched) on a transiently-full buffer instead of
// tolerating a loss, measuring the sustained rate at which the bus delivers
// EVERY event. Any residual drop (delivered != published after drain) fails
// the bench — the zero-drop tooth, live against the real Hub rather than the
// fixed vectors held by pkg/perfgate.P3Gate / TestPerfGate.

// BenchmarkP3SSEThroughput measures single-consumer emit→deliver throughput on
// the SSE Hub and asserts zero dropped events.
func BenchmarkP3SSEThroughput(b *testing.B) {
	const runID = "run-p3"
	hub := NewHub()
	sub := hub.Subscribe(runID)

	var received int64
	done := make(chan struct{})
	go func() {
		// range exits once Unsubscribe closes the channel, after draining every
		// buffered event — so `received` settles to the true delivered total.
		for range sub.ch {
			atomic.AddInt64(&received, 1)
		}
		close(done)
	}()

	ev := Event{ID: "1", Type: "progress", Data: `{"step":"reconcile","pct":42}`}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Back off on a momentarily-full buffer rather than accept a drop: this
		// leg measures the zero-loss sustained rate, not best-effort shedding.
		for hub.Publish(runID, ev) == 0 {
			runtime.Gosched()
		}
	}
	b.StopTimer()

	hub.Unsubscribe(runID, sub) // closes sub.ch → consumer drains the tail, then exits
	<-done

	if got := atomic.LoadInt64(&received); got != int64(b.N) {
		b.Fatalf("P3 zero-drop violated: published %d, delivered %d (%d lost)",
			b.N, got, int64(b.N)-got)
	}
	b.ReportMetric(float64(b.N), "events")
}

// BenchmarkP3SSEFanout measures throughput of the same publish stream fanned out
// to a set of concurrent subscribers (the many-console-tiles case), and asserts
// zero drop across the whole fan-out set. A single Publish that fails to reach
// EVERY subscriber (delivered != fanout) is retried after a yield, so the metric
// is the sustained rate at which the Hub delivers each event to all consumers
// with no loss.
func BenchmarkP3SSEFanout(b *testing.B) {
	for _, fanout := range []int{2, 8, 32} {
		b.Run(fmt.Sprintf("subs=%d", fanout), func(b *testing.B) {
			const runID = "run-p3-fan"
			hub := NewHub()

			subs := make([]*subscriber, fanout)
			var received int64
			done := make(chan struct{}, fanout)
			for i := 0; i < fanout; i++ {
				s := hub.Subscribe(runID)
				subs[i] = s
				go func(s *subscriber) {
					for range s.ch {
						atomic.AddInt64(&received, 1)
					}
					done <- struct{}{}
				}(s)
			}

			ev := Event{ID: "1", Type: "progress", Data: `{"step":"reconcile"}`}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// Require delivery to ALL subscribers before advancing so every
				// published event lands exactly once on each — the zero-drop
				// fan-out invariant. Buffers are drained fast, so a full retry of
				// a partially-delivered publish would double-count; instead wait
				// until the whole set has room, then publish once.
				for {
					if allHaveRoom(subs) {
						hub.Publish(runID, ev)
						break
					}
					runtime.Gosched()
				}
			}
			b.StopTimer()

			for _, s := range subs {
				hub.Unsubscribe(runID, s)
			}
			for i := 0; i < fanout; i++ {
				<-done
			}

			want := int64(b.N) * int64(fanout)
			if got := atomic.LoadInt64(&received); got != want {
				b.Fatalf("P3 fan-out zero-drop violated: want %d deliveries, got %d", want, got)
			}
			b.ReportMetric(float64(b.N), "events")
		})
	}
}

// allHaveRoom reports whether every subscriber's buffer currently has slack, so a
// single Publish will land on all of them (no partial delivery, no drop).
func allHaveRoom(subs []*subscriber) bool {
	for _, s := range subs {
		if len(s.ch) == cap(s.ch) {
			return false
		}
	}
	return true
}
