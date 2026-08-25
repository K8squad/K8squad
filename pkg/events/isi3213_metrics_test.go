package events

// isi3213_metrics_test.go — ISI-3213 (ratchet Go unit-test coverage 35→80).
//
// DB-free / NATS-free unit coverage for the §17.2 event-seam observability
// façade (metrics.go): the four bounded signals, the Prometheus emitter's
// gauge/counter writes, the no-op emitter's silence, and the C6 completeness
// contract that BOTH implementations name exactly the same four signals. These
// run in the ci.yml unit lane and lift the gated authored-coverage number; they
// touch neither Postgres nor JetStream.

import (
	"sort"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// the four §17.2 signals, canonical order-independent set.
var wantSignals = []string{"consumer_lag", "outbox_depth", "publish_failures", "unflushed_lag"}

func sortedSignals(m Metrics) []string {
	s := append([]string(nil), m.Signals()...)
	sort.Strings(s)
	return s
}

func TestNewPrometheusMetrics_NonNilAndIdempotent(t *testing.T) {
	// MustRegister must not panic across repeated construction (sync.Once guard).
	m1 := NewPrometheusMetrics()
	m2 := NewPrometheusMetrics()
	if m1 == nil || m2 == nil {
		t.Fatalf("NewPrometheusMetrics returned nil: m1=%v m2=%v", m1, m2)
	}
}

func TestPrometheusMetrics_SetDepth(t *testing.T) {
	NewPrometheusMetrics() // ensure instruments registered
	var m PrometheusMetrics
	m.SetDepth(7, 3)
	if got := testutil.ToFloat64(outboxDepth); got != 7 {
		t.Errorf("outbox_depth = %v, want 7", got)
	}
	if got := testutil.ToFloat64(outboxUnflushedLag); got != 3 {
		t.Errorf("unflushed_lag = %v, want 3", got)
	}
	// A gauge is a level, not a running sum — a second write replaces, never adds.
	m.SetDepth(2, 0)
	if got := testutil.ToFloat64(outboxDepth); got != 2 {
		t.Errorf("outbox_depth after re-set = %v, want 2", got)
	}
	if got := testutil.ToFloat64(outboxUnflushedLag); got != 0 {
		t.Errorf("unflushed_lag after re-set = %v, want 0", got)
	}
}

func TestPrometheusMetrics_SetConsumerLag(t *testing.T) {
	NewPrometheusMetrics()
	var m PrometheusMetrics
	m.SetConsumerLag(5)
	if got := testutil.ToFloat64(jetstreamConsumerLag); got != 5 {
		t.Errorf("consumer_lag = %v, want 5", got)
	}
	m.SetConsumerLag(0)
	if got := testutil.ToFloat64(jetstreamConsumerLag); got != 0 {
		t.Errorf("consumer_lag after re-set = %v, want 0", got)
	}
}

func TestPrometheusMetrics_IncPublishFailures(t *testing.T) {
	NewPrometheusMetrics()
	var m PrometheusMetrics
	before := testutil.ToFloat64(outboxPublishFailures)
	m.IncPublishFailures()
	m.IncPublishFailures()
	if got := testutil.ToFloat64(outboxPublishFailures); got != before+2 {
		t.Errorf("publish_failures = %v, want %v (a counter only climbs)", got, before+2)
	}
}

func TestPrometheusMetrics_Signals(t *testing.T) {
	if got := sortedSignals(PrometheusMetrics{}); !equalStrings(got, wantSignals) {
		t.Errorf("PrometheusMetrics.Signals() = %v, want %v", got, wantSignals)
	}
}

func TestNopMetrics_SilentButComplete(t *testing.T) {
	var n nopMetrics
	// The no-op emitter exists so the relay never nil-checks; its writes must be
	// harmless (no panic) regardless of value.
	n.SetDepth(999, -1)
	n.SetConsumerLag(123)
	n.IncPublishFailures()
	// Yet it still reports the full §17.2 signal set — the C6 contract is which
	// signals EXIST, not where they are scraped.
	if got := sortedSignals(n); !equalStrings(got, wantSignals) {
		t.Errorf("nopMetrics.Signals() = %v, want %v", got, wantSignals)
	}
}

// TestMetrics_C6Completeness asserts the completeness contract: every Metrics
// implementation names exactly the same four §17.2 signals. A drift here (an
// emitter that forgets or invents a signal) is the failure the C6 probe guards.
func TestMetrics_C6Completeness(t *testing.T) {
	impls := map[string]Metrics{
		"prometheus": PrometheusMetrics{},
		"nop":        nopMetrics{},
	}
	for name, m := range impls {
		if got := sortedSignals(m); !equalStrings(got, wantSignals) {
			t.Errorf("%s: Signals() = %v, want the four §17.2 signals %v", name, got, wantSignals)
		}
		if n := len(m.Signals()); n != 4 {
			t.Errorf("%s: emits %d signals, want exactly 4 (NFR-OBS3 bounded set)", name, n)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
