// decisionobs_test.go — ISI-5619 / ISI-5539 WS-G G-2. DB-free unit coverage of the
// decision_request lifecycle instrumentation helpers: the span events carry SHAPE
// only (bounded keys, no question/answer text — PII posture ADR-0026 §10), the
// funnel counter and resolution histogram record under bounded {mode,phase} /
// {mode,terminal_phase} labels, and an idempotent-key replay does NOT add a funnel
// row. The store-method wiring itself is covered by decision_integration_test.go
// against real Postgres; these prove the emission contract without a database.

package discussion

import (
	"context"
	"os"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// decisionObsReader is the process-wide metric reader the decisionInst() instruments
// bind to. It is installed in TestMain BEFORE any test touches the lazy sync.Once, so
// every test reads back from one live MeterProvider (a per-test provider would be torn
// down while the Once still pointed at it, and the counter would read empty).
var decisionObsReader *sdkmetric.ManualReader

func TestMain(m *testing.M) {
	decisionObsReader = sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(decisionObsReader)))
	os.Exit(m.Run())
}

// decisionObsAllowedEventKeys is the CLOSED set of attribute keys the lifecycle
// events may carry — all shape (mode, booleans, counts, bounded phase/cause) plus
// the span-event-only work_item.ref / team.id. A text attribute (the question,
// option labels, free text, reject reason) must never appear, so a new key outside
// this set fails the test.
var decisionObsAllowedEventKeys = map[string]bool{
	"ksquad.decision.mode":                true,
	"ksquad.decision.idempotent_hit":      true,
	"ksquad.decision.author_kind":         true,
	"ksquad.decision.free_text":           true,
	"ksquad.decision.selected_count":      true,
	"ksquad.decision.reject_reason_given": true,
	"ksquad.decision.phase":               true,
	"ksquad.decision.supersede_cause":     true,
	"ksquad.work_item.ref":                true,
	"ksquad.team.id":                      true,
}

// decisionObsHarness installs in-memory tracer + meter providers as the globals the
// helpers read (telemetry.Tracer/Meter resolve the global providers on every call),
// starts a span on the returned ctx so AddEvent lands somewhere recording, and hands
// back readers over the captured spans and metrics.
func decisionObsHarness(t *testing.T) (ctx context.Context, spans func() []tracetest.SpanStub, collect func() metricdata.ResourceMetrics) {
	t.Helper()

	spanExp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(spanExp)))
	prevTP := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)

	ctx, span := tp.Tracer("test").Start(context.Background(), "test.parent")
	t.Cleanup(func() {
		span.End()
		otel.SetTracerProvider(prevTP)
	})

	spans = func() []tracetest.SpanStub {
		span.End() // flush the parent so its events are exported
		return spanExp.GetSpans()
	}
	collect = func() metricdata.ResourceMetrics {
		var rm metricdata.ResourceMetrics
		if err := decisionObsReader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("metric collect: %v", err)
		}
		return rm
	}
	return ctx, spans, collect
}

// eventByName returns the first captured span event with the given name across all
// spans, or nil.
func eventByName(stubs []tracetest.SpanStub, name string) *sdktrace.Event {
	for i := range stubs {
		for j := range stubs[i].Events {
			if stubs[i].Events[j].Name == name {
				return &stubs[i].Events[j]
			}
		}
	}
	return nil
}

// attrMap flattens an event's attributes to a key→value(as string) map.
func attrMap(attrs []attribute.KeyValue) map[string]string {
	m := make(map[string]string, len(attrs))
	for _, a := range attrs {
		m[string(a.Key)] = a.Value.String()
	}
	return m
}

func TestDecisionObsEventsAreShapeOnly(t *testing.T) {
	ctx, spans, _ := decisionObsHarness(t)

	auth := AuthorContext{Principal: "run:abc", AgentID: strptr("agent-1")}
	recordDecisionCreated(ctx, DecisionModeChooseOne, false, auth, "wi-ref", "team-uid")
	emitDecisionAnswered(ctx, DecisionModeFreeForm, true, 2, "wi-ref")
	emitDecisionRejected(ctx, DecisionModeApprove, true, "wi-ref")
	emitDecisionExpired(ctx, DecisionPhaseSuperseded, "revision_moved", "wi-ref")

	stubs := spans()
	for _, name := range []string{
		"decision_request.created", "decision_request.answered",
		"decision_request.rejected", "decision_request.expired",
	} {
		ev := eventByName(stubs, name)
		if ev == nil {
			t.Fatalf("missing lifecycle event %q", name)
		}
		for _, a := range ev.Attributes {
			if !decisionObsAllowedEventKeys[string(a.Key)] {
				t.Errorf("event %q carries non-shape attribute key %q (PII posture)", name, a.Key)
			}
		}
	}

	// created event shape.
	created := attrMap(eventByName(stubs, "decision_request.created").Attributes)
	if created["ksquad.decision.mode"] != DecisionModeChooseOne {
		t.Errorf("created.mode = %q, want %q", created["ksquad.decision.mode"], DecisionModeChooseOne)
	}
	if created["ksquad.decision.author_kind"] != "agent" {
		t.Errorf("created.author_kind = %q, want agent", created["ksquad.decision.author_kind"])
	}
	if created["ksquad.decision.idempotent_hit"] != "false" {
		t.Errorf("created.idempotent_hit = %q, want false", created["ksquad.decision.idempotent_hit"])
	}

	// answered event shape — free_text bool + selected_count, never the text itself.
	answered := attrMap(eventByName(stubs, "decision_request.answered").Attributes)
	if answered["ksquad.decision.free_text"] != "true" {
		t.Errorf("answered.free_text = %q, want true", answered["ksquad.decision.free_text"])
	}
	if answered["ksquad.decision.selected_count"] != "2" {
		t.Errorf("answered.selected_count = %q, want 2", answered["ksquad.decision.selected_count"])
	}

	// expired event carries the bounded supersede cause.
	expired := attrMap(eventByName(stubs, "decision_request.expired").Attributes)
	if expired["ksquad.decision.supersede_cause"] != "revision_moved" {
		t.Errorf("expired.supersede_cause = %q, want revision_moved", expired["ksquad.decision.supersede_cause"])
	}
}

func TestDecisionObsFunnelAndLatencyMetrics(t *testing.T) {
	ctx, _, collect := decisionObsHarness(t)

	// ksquad.decision_requests / .resolution are process-wide CUMULATIVE instruments:
	// decisionInst()'s sync.Once binds them to the single TestMain MeterProvider shared
	// by every test in this package — including the real-DB decision_integration_test.go,
	// which also emits approve-mode creates and sorts before this file, so it runs first
	// and primes the counter. Assert DELTAS around this test's own calls, never absolute
	// totals, or the suite's run order contaminates the reading.
	createdLbl := map[string]string{"mode": DecisionModeApprove, "phase": "created"}
	answeredLbl := map[string]string{"mode": DecisionModeApprove, "phase": DecisionPhaseAnswered}
	histLbl := map[string]string{"mode": DecisionModeApprove, "terminal_phase": DecisionPhaseAnswered}

	base := collect()
	baseCreated := funnelSum(base, createdLbl)
	baseAnswered := funnelSum(base, answeredLbl)
	baseHistCount, baseHistSum := resolutionDP(base, histLbl)

	start := time.Now().Add(-5 * time.Second)
	// One real create, one idempotent replay (must NOT add a created funnel row).
	auth := AuthorContext{Principal: "user:alice"}
	recordDecisionCreated(ctx, DecisionModeApprove, false, auth, "", "team")
	recordDecisionCreated(ctx, DecisionModeApprove, true, auth, "", "team")
	recordDecisionResolved(ctx, DecisionModeApprove, DecisionPhaseAnswered, start)

	rm := collect()
	// Real create adds exactly one created funnel row; the idempotent replay adds none.
	if got := funnelSum(rm, createdLbl) - baseCreated; got != 1 {
		t.Errorf("Δdecision_requests{created} = %d, want 1 (idempotent replay must not count)", got)
	}
	if got := funnelSum(rm, answeredLbl) - baseAnswered; got != 1 {
		t.Errorf("Δdecision_requests{answered} = %d, want 1", got)
	}

	gotCount, gotSum := resolutionDP(rm, histLbl)
	if gotCount == 0 {
		t.Fatalf("resolution histogram missing {mode=approve,terminal_phase=answered} point")
	}
	if d := gotCount - baseHistCount; d != 1 {
		t.Errorf("Δresolution histogram count = %d, want 1", d)
	}
	if d := gotSum - baseHistSum; d < 4 { // ~5s elapsed; guard against a bogus near-zero latency
		t.Errorf("Δresolution histogram sum = %fs, want ≳5s (created→answered span)", d)
	}
}

func TestDecisionAuthorKindAndModeFromPayload(t *testing.T) {
	if got := decisionAuthorKind(AuthorContext{AgentID: strptr("a")}); got != "agent" {
		t.Errorf("author_kind(agent) = %q, want agent", got)
	}
	if got := decisionAuthorKind(AuthorContext{}); got != "human" {
		t.Errorf("author_kind(human) = %q, want human", got)
	}
	if got := decisionModeFromPayload([]byte(`{"mode":"choose_many","title":"secret title"}`)); got != DecisionModeChooseMany {
		t.Errorf("mode = %q, want choose_many", got)
	}
	if got := decisionModeFromPayload(nil); got != "" {
		t.Errorf("mode(nil) = %q, want empty", got)
	}
}

// ---- metric read helpers ----

// funnelSum returns the summed value of the ksquad.decision_requests counter for the
// given label set, or 0 if the counter is not registered yet (tolerant baseline read —
// the instrument is lazily created on first emission, and the counter is cumulative and
// process-wide, so callers compare deltas rather than absolute totals).
func funnelSum(rm metricdata.ResourceMetrics, want map[string]string) int64 {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "ksquad.decision_requests" {
				continue
			}
			if s, ok := m.Data.(metricdata.Sum[int64]); ok {
				return sumFor(s, want)
			}
		}
	}
	return 0
}

// resolutionDP returns the (count, sum) of the ksquad.decision_request.resolution
// histogram data point for the given label set, or (0, 0) if absent (tolerant read).
func resolutionDP(rm metricdata.ResourceMetrics, want map[string]string) (uint64, float64) {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "ksquad.decision_request.resolution" {
				continue
			}
			if h, ok := m.Data.(metricdata.Histogram[float64]); ok {
				if dp := histDPFor(h, want); dp != nil {
					return dp.Count, dp.Sum
				}
			}
		}
	}
	return 0, 0
}

func attrsMatch(set attribute.Set, want map[string]string) bool {
	for k, v := range want {
		val, ok := set.Value(attribute.Key(k))
		if !ok || val.String() != v {
			return false
		}
	}
	return true
}

func sumFor(s metricdata.Sum[int64], want map[string]string) int64 {
	var total int64
	for _, dp := range s.DataPoints {
		if attrsMatch(dp.Attributes, want) {
			total += dp.Value
		}
	}
	return total
}

func histDPFor(h metricdata.Histogram[float64], want map[string]string) *metricdata.HistogramDataPoint[float64] {
	for i := range h.DataPoints {
		if attrsMatch(h.DataPoints[i].Attributes, want) {
			return &h.DataPoints[i]
		}
	}
	return nil
}
