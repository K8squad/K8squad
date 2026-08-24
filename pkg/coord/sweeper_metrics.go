// sweeper_metrics.go — the reclaim-sweeper's Prometheus instruments (Story 2.4 /
// ISI-3104). Kept in a dedicated file alongside sweeper.go (the token_metrics.go
// convention) rather than folded into metrics.go, so the whole crash-safe reclaim
// sweeper — loop, store, and its metrics — is one self-contained unit.
//
// The three instruments are BOUNDED — global counters and a histogram, NO
// per-item/per-project labels — so the sweeper obeys the same NFR-OBS3 cardinality
// firewall as the coord metrics in metrics.go (the authoritative per-reclaim record
// is the claim/audit_log row, never a metric):
//
//	ksquad_coord_sweep_cycles_total     counter    reclaim scan cycles executed
//	ksquad_coord_sweep_reclaims_total   counter    expired claims reclaimed by the sweeper
//	ksquad_coord_sweep_duration_seconds histogram  per-cycle scan+reclaim latency
//
// sweep_reclaims_total is sweeper-ATTRIBUTED — distinct from a general reclaim
// counter that would also count the opportunistic acquire-path reclaim — so an SRE
// can see how much recovery the periodic scan is doing on its own.
package coord

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	sweeperMetricsRegisterOnce sync.Once

	coordSweepCyclesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "ksquad_coord_sweep_cycles_total",
		Help: "Reclaim sweep cycles executed (story 2.4 crash-safe reclaim sweeper).",
	})

	coordSweepReclaimsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "ksquad_coord_sweep_reclaims_total",
		Help: "Expired claims reclaimed by the background sweeper (story 2.4).",
	})

	coordSweepDurationSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "ksquad_coord_sweep_duration_seconds",
		Help:    "Per-cycle reclaim scan+reclaim latency (story 2.4).",
		Buckets: prometheus.DefBuckets,
	})
)

// SweeperMetrics is the reclaim-sweeper metrics façade — a thin, swappable
// interface (the CoordMetrics discipline) so unit tests assert on emissions with a
// fake and the signal set is named in exactly one place.
type SweeperMetrics interface {
	// IncSweepCycle increments the sweep-cycle counter — one per scan, whether or
	// not it reclaimed anything.
	IncSweepCycle()
	// AddSweepReclaims adds n to the total items the sweeper reclaimed. Called only
	// with n > 0.
	AddSweepReclaims(n int)
	// ObserveSweepDuration records one cycle's scan+reclaim latency in seconds.
	ObserveSweepDuration(seconds float64)
	// Signals returns the names of every signal this emitter emits.
	Signals() []string
}

// PrometheusSweeperMetrics implements SweeperMetrics on the instruments above.
type PrometheusSweeperMetrics struct{}

// NewPrometheusSweeperMetrics registers the sweeper instruments on the default
// registry (idempotent across calls) and returns the emitter.
func NewPrometheusSweeperMetrics() *PrometheusSweeperMetrics {
	sweeperMetricsRegisterOnce.Do(func() {
		prometheus.MustRegister(
			coordSweepCyclesTotal,
			coordSweepReclaimsTotal,
			coordSweepDurationSeconds,
		)
	})
	return &PrometheusSweeperMetrics{}
}

func (PrometheusSweeperMetrics) IncSweepCycle()                 { coordSweepCyclesTotal.Inc() }
func (PrometheusSweeperMetrics) AddSweepReclaims(n int)         { coordSweepReclaimsTotal.Add(float64(n)) }
func (PrometheusSweeperMetrics) ObserveSweepDuration(s float64) { coordSweepDurationSeconds.Observe(s) }
func (PrometheusSweeperMetrics) Signals() []string {
	return []string{
		"coord_sweep_cycles_total",
		"coord_sweep_reclaims_total",
		"coord_sweep_duration_seconds",
	}
}

// nopSweeperMetrics is the default when no sweeper metrics are wired. It still
// reports the full signal set (the contract is which signals EXIST) — the
// nopCoordMetrics discipline.
type nopSweeperMetrics struct{}

func (nopSweeperMetrics) IncSweepCycle()               {}
func (nopSweeperMetrics) AddSweepReclaims(int)         {}
func (nopSweeperMetrics) ObserveSweepDuration(float64) {}
func (nopSweeperMetrics) Signals() []string {
	return []string{
		"coord_sweep_cycles_total",
		"coord_sweep_reclaims_total",
		"coord_sweep_duration_seconds",
	}
}

var _ SweeperMetrics = PrometheusSweeperMetrics{}
var _ SweeperMetrics = nopSweeperMetrics{}
