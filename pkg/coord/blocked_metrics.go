// blocked_metrics.go — the Story 13.3 blocked-by gauge (metric set §13,
// ISI-2916). `blocked` is an orthogonal CONDITION (work_item.blocked_reason),
// never a lane (§8.6, 0001_coord_schema.sql) — so unlike every other lifecycle
// signal it has NO phase-transition edge to count (see phasespan.go). The correct
// shape is therefore a periodically-sampled GAUGE of the current blocked
// population, refreshed each sweeper cycle the way events.SetDepth refreshes
// ksquad_outbox_unflushed_lag from a `published_at IS NULL` scan.
//
// The gauge is BOUNDED to a SINGLE series — no per-item / per-project / per-reason
// label — so it obeys the same NFR-OBS3 cardinality firewall as the other coord
// metrics (sweeper_metrics.go, phasespan.go): the authoritative per-item record is
// the work_item row (queryable by blocked_reason), never a metric label.
//
//	ksquad_coord_blocked_items  gauge  work items with blocked_reason IS NOT NULL
package coord

import (
	"context"
	"fmt"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	blockedMetricsRegisterOnce sync.Once

	coordBlockedItems = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "ksquad_coord_blocked_items",
		Help: "Work items currently in the blocked condition (blocked_reason IS NOT NULL) — Story 13.3 blocked-by gauge. Bounded single series (NFR-OBS3); the per-item record lives on the work_item row, never a metric label.",
	})
)

// BlockedMetrics is the blocked-by gauge façade — the events.Metrics /
// SweeperMetrics discipline: a thin, swappable interface so unit tests assert on
// emissions with a fake and the signal is named in exactly one place.
type BlockedMetrics interface {
	// SetBlocked sets the gauge to the current count of blocked work items. Called
	// once per sweeper cycle with a freshly-sampled population.
	SetBlocked(n int64)
	// Signals returns the names of every signal this emitter emits.
	Signals() []string
}

// PrometheusBlockedMetrics implements BlockedMetrics on the gauge above.
type PrometheusBlockedMetrics struct{}

// NewPrometheusBlockedMetrics registers the blocked-by gauge on the default
// registry (idempotent across calls) and returns the emitter.
func NewPrometheusBlockedMetrics() *PrometheusBlockedMetrics {
	blockedMetricsRegisterOnce.Do(func() {
		prometheus.MustRegister(coordBlockedItems)
	})
	return &PrometheusBlockedMetrics{}
}

func (PrometheusBlockedMetrics) SetBlocked(n int64) { coordBlockedItems.Set(float64(n)) }
func (PrometheusBlockedMetrics) Signals() []string  { return []string{"coord_blocked_items"} }

// nopBlockedMetrics is the default when no blocked sampler is wired. It still
// reports the full signal set (the contract is which signals EXIST) — the
// nopSweeperMetrics discipline.
type nopBlockedMetrics struct{}

func (nopBlockedMetrics) SetBlocked(int64)  {}
func (nopBlockedMetrics) Signals() []string { return []string{"coord_blocked_items"} }

var _ BlockedMetrics = PrometheusBlockedMetrics{}
var _ BlockedMetrics = nopBlockedMetrics{}

// blockedCounter is the read side the blocked-by gauge samples each cycle — just
// the one count the sampler needs. SweepStore satisfies it (CountBlocked below),
// so the sweeper can sample the same bound work_item table it already reclaims
// against, with no new DB handle or config.
type blockedCounter interface {
	CountBlocked(ctx context.Context) (int64, error)
}

// CountBlocked returns the number of work items currently in the blocked
// condition (blocked_reason IS NOT NULL) in the bound work_item table — the read
// half of the Story 13.3 gauge. It is a single bounded COUNT against the same
// table ReclaimExpired scans; the sampler sets the gauge from it each cycle.
func (s *SweepStore) CountBlocked(ctx context.Context) (int64, error) {
	q := fmt.Sprintf(`SELECT count(*) FROM %s WHERE blocked_reason IS NOT NULL`, s.cfg.WorkItem)
	var n int64
	if err := s.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, fmt.Errorf("coord.CountBlocked: %w", err)
	}
	return n, nil
}

var _ blockedCounter = (*SweepStore)(nil)
