package events

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// The four §17.2 event-seam signals (AC-c / C6). All are bounded — global
// gauges + one global counter, NO per-event / per-project / per-subject label —
// so the seam's observability obeys the NFR-OBS3 cardinality firewall (the
// authoritative per-event record is the outbox row itself, never a metric):
//
//	ksquad_outbox_depth                    gauge    total outbox rows
//	ksquad_outbox_unflushed_lag            gauge    rows with published_at IS NULL (backlog)
//	ksquad_outbox_publish_failures_total   counter  NATS publish failures
//	ksquad_jetstream_consumer_lag          gauge    messages pending across JS consumers
var (
	metricsRegisterOnce sync.Once

	outboxDepth = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "ksquad_outbox_depth",
		Help: "Total rows in coord.outbox (event-seam observability, §17.2).",
	})
	outboxUnflushedLag = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "ksquad_outbox_unflushed_lag",
		Help: "coord.outbox rows not yet published to NATS (published_at IS NULL) — the relay backlog (§17.2).",
	})
	outboxPublishFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "ksquad_outbox_publish_failures_total",
		Help: "NATS publish attempts the relay failed (row left unflushed for retry — at-least-once, §17.2).",
	})
	jetstreamConsumerLag = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "ksquad_jetstream_consumer_lag",
		Help: "Messages pending across the event stream's JetStream consumers (downstream fan-out lag, §17.2).",
	})
)

// Metrics is the relay's handle to the four §17.2 instruments. It is a thin,
// swappable façade so unit tests can assert on emissions with a fake and so the
// signal set is named in exactly one place (the C6 completeness contract).
type Metrics interface {
	SetDepth(total, unflushed int64)
	IncPublishFailures()
	SetConsumerLag(pending int64)
	// Signals returns the names of every signal this Metrics emits — the C6
	// completeness probe asserts all four §17.2 signals are present.
	Signals() []string
}

// PrometheusMetrics implements Metrics on the four instruments above.
type PrometheusMetrics struct{}

// NewPrometheusMetrics registers the four §17.2 instruments on the default
// registry (idempotent across calls) and returns the emitter.
func NewPrometheusMetrics() *PrometheusMetrics {
	metricsRegisterOnce.Do(func() {
		prometheus.MustRegister(outboxDepth, outboxUnflushedLag, outboxPublishFailures, jetstreamConsumerLag)
	})
	return &PrometheusMetrics{}
}

func (PrometheusMetrics) SetDepth(total, unflushed int64) {
	outboxDepth.Set(float64(total))
	outboxUnflushedLag.Set(float64(unflushed))
}
func (PrometheusMetrics) IncPublishFailures()          { outboxPublishFailures.Inc() }
func (PrometheusMetrics) SetConsumerLag(pending int64) { jetstreamConsumerLag.Set(float64(pending)) }
func (PrometheusMetrics) Signals() []string {
	return []string{"outbox_depth", "unflushed_lag", "publish_failures", "consumer_lag"}
}

// nopMetrics is the default when a relay is constructed without an emitter, so
// the relay logic never nil-checks. It still reports the full signal set for the
// C6 probe (the contract is which signals EXIST, not where they're scraped).
type nopMetrics struct{}

func (nopMetrics) SetDepth(int64, int64) {}
func (nopMetrics) IncPublishFailures()   {}
func (nopMetrics) SetConsumerLag(int64)  {}
func (nopMetrics) Signals() []string {
	return []string{"outbox_depth", "unflushed_lag", "publish_failures", "consumer_lag"}
}

var _ Metrics = PrometheusMetrics{}
var _ Metrics = nopMetrics{}
