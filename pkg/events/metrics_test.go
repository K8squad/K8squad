package events

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// DB-free unit tests for the §17.2 event-seam metrics façade (ISI-3213). These
// exercise the Prometheus and no-op emitters and the C6 signal-completeness
// contract — no Postgres, runs in the ci.yml unit lane.

func TestPrometheusMetricsSetDepth(t *testing.T) {
	m := NewPrometheusMetrics()
	m.SetDepth(42, 7)
	if got := testutil.ToFloat64(outboxDepth); got != 42 {
		t.Errorf("outbox_depth = %v, want 42", got)
	}
	if got := testutil.ToFloat64(outboxUnflushedLag); got != 7 {
		t.Errorf("unflushed_lag = %v, want 7", got)
	}
	// Overwrite semantics (gauge Set, not Add).
	m.SetDepth(10, 0)
	if got := testutil.ToFloat64(outboxDepth); got != 10 {
		t.Errorf("outbox_depth after reset = %v, want 10", got)
	}
}

func TestPrometheusMetricsSetConsumerLag(t *testing.T) {
	m := NewPrometheusMetrics()
	m.SetConsumerLag(123)
	if got := testutil.ToFloat64(jetstreamConsumerLag); got != 123 {
		t.Errorf("consumer_lag = %v, want 123", got)
	}
}

func TestPrometheusMetricsIncPublishFailures(t *testing.T) {
	m := NewPrometheusMetrics()
	before := testutil.ToFloat64(outboxPublishFailures)
	m.IncPublishFailures()
	m.IncPublishFailures()
	if got := testutil.ToFloat64(outboxPublishFailures); got != before+2 {
		t.Errorf("publish_failures delta = %v, want 2", got-before)
	}
}

func TestNewPrometheusMetricsIdempotent(t *testing.T) {
	// The sync.Once registration must not panic on repeated construction.
	_ = NewPrometheusMetrics()
	_ = NewPrometheusMetrics()
}

func TestSignalCompletenessC6(t *testing.T) {
	want := []string{"outbox_depth", "unflushed_lag", "publish_failures", "consumer_lag"}
	for _, m := range []Metrics{PrometheusMetrics{}, nopMetrics{}} {
		got := m.Signals()
		if len(got) != len(want) {
			t.Fatalf("%T: Signals() = %v, want %v", m, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%T: Signals()[%d] = %q, want %q", m, i, got[i], want[i])
			}
		}
	}
}

func TestNopMetricsAreNoOps(t *testing.T) {
	var m Metrics = nopMetrics{}
	// Must be callable without panicking or touching any registry.
	m.SetDepth(1, 1)
	m.SetConsumerLag(1)
	m.IncPublishFailures()
}
