// phasespan.go — per-phase timing observability for the lifecycle board
// (ISI-4490 / E7, NFR-5). Every state_transition on a work item emits:
//
//   - a trace span `coord.phase_transition` carrying the high-cardinality
//     JOIN KEY ksquad.work_item.ref plus phase.from/phase.to/phase.initiator
//     and the "time in phase" duration. The span is the authoritative
//     per-transition record, queryable by work_item.ref in the trace backend
//     (Dynatrace), consistent with the run-trace work (ISI-4238);
//   - a BOUNDED metric pair — a transitions counter keyed on the (from,to,
//     initiator) graph edge and a "time in phase" histogram keyed on the
//     phase being left. Neither metric carries work_item.ref: per the
//     NFR-OBS3 cardinality firewall (see sweeper_metrics.go) the per-item
//     record lives on the span / audit_log row, NEVER on a metric label.
//
// The emitter is deliberately standalone and best-effort: it is called AFTER
// the transition's own transaction commits, so a telemetry failure can never
// roll back or fail a board move. humanstate.go wires it for human/agent lane
// moves today; the E5 coordinator drive loop (ISI-4489) calls the same
// EmitPhaseTransition once it advances tickets across phases, so both the
// human and the coordinator halves of the lifecycle are instrumented through
// one path.
package coord

import (
	"context"
	"sync"
	"time"

	"github.com/K8squad/K8squad/pkg/telemetry"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Phase-transition attribute keys (arch §5.2). ksquad.work_item.ref is the
// canonical JOIN KEY across telemetry (per memory: the ref, not the issue id);
// it matches the toolusage span convention (ksquad.work_item.ref).
const (
	attrPhaseWorkItemRef = "ksquad.work_item.ref"
	attrPhaseFrom        = "ksquad.phase.from"
	attrPhaseTo          = "ksquad.phase.to"
	attrPhaseInitiator   = "ksquad.phase.initiator"
	attrPhaseDurationMS  = "ksquad.duration.ms"

	// phaseSpanName is the transition marker span; short-lived by design —
	// the "time in phase" it reports is the attribute/metric, not the span's
	// own wall-clock (which is ~0).
	phaseSpanName = "coord.phase_transition"
)

var (
	phaseMetricsRegisterOnce sync.Once

	coordPhaseTransitionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ksquad_coord_phase_transitions_total",
		Help: "Work-item phase transitions, by graph edge and initiator (ISI-4490 / NFR-5).",
	}, []string{"from", "to", "initiator"})

	// Buckets span seconds→days: a phase can be left in seconds (a fast
	// coordinator advance) or sit for days (a human working lane). Powers-of
	// -ish coverage from 1s to ~1 week.
	coordPhaseDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "ksquad_coord_phase_duration_seconds",
		Help:    "Time a work item spent in a phase before leaving it, by phase and initiator (ISI-4490 / NFR-5).",
		Buckets: []float64{1, 10, 60, 300, 1800, 3600, 6 * 3600, 24 * 3600, 3 * 24 * 3600, 7 * 24 * 3600},
	}, []string{"phase", "initiator"})
)

// PhaseTransitionEvent is one advance of a work item across the lifecycle
// board — the input to the observability emission. It is a pure value; the
// caller has already committed the transition (and its audit row) before
// building it.
type PhaseTransitionEvent struct {
	// WorkItemRef is the coordination-side work-item id — the JOIN KEY
	// stamped on the span. Required; an empty ref skips emission.
	WorkItemRef string
	// From is the phase/lane the item is leaving (the phase whose "time in
	// phase" this event reports). To is the phase/lane it now sits in.
	From, To string
	// Initiator attributes the move: "human", "agent", or "coordinator"
	// (mirrors the state_transition audit_log initiator).
	Initiator string
	// TimeInPhase is how long the item sat in From before this transition,
	// derived from audit-log timestamps. A negative or zero value is emitted
	// as 0 (a clock skew or a same-instant move is not "negative time").
	TimeInPhase time.Duration
}

// PhaseMetrics is the phase-timing metric façade — the CoordMetrics /
// SweeperMetrics discipline so tests assert emissions with a fake and the
// signal set is named in exactly one place. Labels are BOUNDED (phase/edge/
// initiator enums), never per-item.
type PhaseMetrics interface {
	// IncPhaseTransition counts one transition on the (from,to,initiator) edge.
	IncPhaseTransition(from, to, initiator string)
	// ObservePhaseDuration records seconds spent in `phase` before leaving it.
	ObservePhaseDuration(phase, initiator string, seconds float64)
	// Signals returns the names of every signal this emitter emits.
	Signals() []string
}

// PrometheusPhaseMetrics implements PhaseMetrics on the instruments above.
type PrometheusPhaseMetrics struct{}

// NewPrometheusPhaseMetrics registers the phase instruments on the default
// registry (idempotent across calls) and returns the emitter.
func NewPrometheusPhaseMetrics() *PrometheusPhaseMetrics {
	phaseMetricsRegisterOnce.Do(func() {
		prometheus.MustRegister(coordPhaseTransitionsTotal, coordPhaseDurationSeconds)
	})
	return &PrometheusPhaseMetrics{}
}

func (PrometheusPhaseMetrics) IncPhaseTransition(from, to, initiator string) {
	coordPhaseTransitionsTotal.WithLabelValues(from, to, initiator).Inc()
}
func (PrometheusPhaseMetrics) ObservePhaseDuration(phase, initiator string, seconds float64) {
	coordPhaseDurationSeconds.WithLabelValues(phase, initiator).Observe(seconds)
}
func (PrometheusPhaseMetrics) Signals() []string {
	return []string{
		"coord_phase_transitions_total",
		"coord_phase_duration_seconds",
	}
}

// nopPhaseMetrics is the default when no phase metrics are wired. It still
// reports the full signal set (the contract is which signals EXIST).
type nopPhaseMetrics struct{}

func (nopPhaseMetrics) IncPhaseTransition(string, string, string)    {}
func (nopPhaseMetrics) ObservePhaseDuration(string, string, float64) {}
func (nopPhaseMetrics) Signals() []string {
	return []string{
		"coord_phase_transitions_total",
		"coord_phase_duration_seconds",
	}
}

var _ PhaseMetrics = PrometheusPhaseMetrics{}
var _ PhaseMetrics = nopPhaseMetrics{}

// defaultPhaseMetrics is the process-wide emitter. It is nop until a wiring
// site (the apiserver/coord bootstrap) installs the Prometheus one, mirroring
// the Tracer()/Meter() "safe before Setup" discipline so call sites never
// need a nil-check.
var defaultPhaseMetrics PhaseMetrics = nopPhaseMetrics{}

// SetPhaseMetrics installs the process phase-metrics emitter. Called once at
// bootstrap; not safe for concurrent use with EmitPhaseTransition (bootstrap
// happens before serving).
func SetPhaseMetrics(m PhaseMetrics) {
	if m != nil {
		defaultPhaseMetrics = m
	}
}

// EmitPhaseTransition records one phase transition as a span + bounded
// metrics. It is best-effort and never returns an error: the caller has
// already committed the transition, and observability must not be able to
// fail a board move. A blank WorkItemRef, From, or To is treated as
// not-a-phase-transition and skipped (the counter/histogram would carry an
// empty label). Safe to call before telemetry.Setup (spans dropped, metrics
// nop).
func EmitPhaseTransition(ctx context.Context, ev PhaseTransitionEvent) {
	if ev.WorkItemRef == "" || ev.From == "" || ev.To == "" {
		return
	}
	initiator := ev.Initiator
	if initiator == "" {
		initiator = "unknown"
	}
	secs := ev.TimeInPhase.Seconds()
	if secs < 0 {
		secs = 0
	}

	// Span: the per-item authoritative record, carrying the high-cardinality
	// join key. Short-lived marker; duration is the attribute, not the span.
	_, span := telemetry.Tracer().Start(ctx, phaseSpanName, trace.WithAttributes(
		attribute.String(attrPhaseWorkItemRef, ev.WorkItemRef),
		attribute.String(attrPhaseFrom, ev.From),
		attribute.String(attrPhaseTo, ev.To),
		attribute.String(attrPhaseInitiator, initiator),
		attribute.Int64(attrPhaseDurationMS, ev.TimeInPhase.Milliseconds()),
	))
	span.End()

	// Metrics: BOUNDED aggregate — never work_item.ref (cardinality firewall).
	defaultPhaseMetrics.IncPhaseTransition(ev.From, ev.To, initiator)
	defaultPhaseMetrics.ObservePhaseDuration(ev.From, initiator, secs)
}
