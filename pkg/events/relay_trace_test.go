package events

import (
	"context"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/K8squad/K8squad/pkg/telemetry"
)

// headerPub implements both events.Publisher and events.HeaderPublisher, so the
// relay takes the trace-carrying path and the test can inspect the headers the
// relay injected (ISI-4440).
type headerPub struct {
	mu        sync.Mutex
	delivered []string
	headers   []map[string]string // per-publish headers, in delivery order
}

func (p *headerPub) Publish(_ context.Context, subject string, _ []byte) error {
	return p.PublishMsg(context.Background(), subject, nil, nil)
}

func (p *headerPub) PublishMsg(_ context.Context, subject string, _ []byte, headers map[string]string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.delivered = append(p.delivered, subject)
	p.headers = append(p.headers, headers)
	return nil
}

func (p *headerPub) Close() error { return nil }

// withRecordingTracer installs an in-memory tracer + the W3C propagator for the
// duration of a test and returns the span recorder.
func withRecordingTracer(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})
	return sr
}

// TestRelay_ProducerSpanJoinsRunTrace_FromCarrier proves the core ISI-4440
// behavior: a row whose trace_carrier holds the run's traceparent produces a
// `nats publish` span on the SAME trace, and the injected message headers carry
// a traceparent whose trace_id matches — so the consumer continues the run
// trace.
func TestRelay_ProducerSpanJoinsRunTrace_FromCarrier(t *testing.T) {
	sr := withRecordingTracer(t)

	// A known run trace id, expressed as a W3C carrier (as capture would stamp it).
	const runTraceID = "0af7651916cd43dd8448eb211c80319c"
	carrier := map[string]string{
		"traceparent": "00-" + runTraceID + "-b7ad6b7169203331-01",
	}

	store := &fakeStore{}
	store.append(OutboxRow{
		Entity: "run", ProjectID: "p1", EventType: "started", RunID: "run-1",
		Payload: []byte("{}"), TraceCarrier: carrier,
	})
	pub := &headerPub{}

	r, err := NewRelay(RelayConfig{Store: store, Publisher: pub})
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if _, _, err := r.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// (1) exactly one producer span, on the run trace, with messaging semconv.
	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("want 1 producer span, got %d", len(spans))
	}
	sp := spans[0]
	if sp.SpanKind() != trace.SpanKindProducer {
		t.Errorf("span kind = %v, want Producer", sp.SpanKind())
	}
	if got := sp.SpanContext().TraceID().String(); got != runTraceID {
		t.Errorf("producer span trace_id = %s, want run trace %s", got, runTraceID)
	}
	if !hasAttr(sp, "messaging.system", "nats") || !hasAttr(sp, "messaging.operation", "publish") {
		t.Errorf("producer span missing messaging semconv attrs: %v", sp.Attributes())
	}

	// (2) the injected headers carry a traceparent on the SAME trace.
	pub.mu.Lock()
	defer pub.mu.Unlock()
	if len(pub.headers) != 1 {
		t.Fatalf("want headers for 1 publish, got %d", len(pub.headers))
	}
	tp := pub.headers[0]["traceparent"]
	if tp == "" {
		t.Fatalf("no traceparent injected into NATS headers")
	}
	// Re-extract and confirm the header trace id matches the run trace.
	hc := otel.GetTextMapPropagator().Extract(context.Background(),
		propagation.MapCarrier(pub.headers[0]))
	if got := trace.SpanContextFromContext(hc).TraceID().String(); got != runTraceID {
		t.Errorf("injected header trace_id = %s, want %s", got, runTraceID)
	}
}

// TestRelay_ProducerSpanJoinsRunTrace_FromPayloadTraceID proves the fallback:
// a row with NO trace_carrier but a trace_id in the payload (a WS-D lifecycle
// row) still produces a span on that trace.
func TestRelay_ProducerSpanJoinsRunTrace_FromPayloadTraceID(t *testing.T) {
	sr := withRecordingTracer(t)
	const runTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"

	store := &fakeStore{}
	store.append(OutboxRow{
		Entity: "run", ProjectID: "p1", EventType: "ended", RunID: "run-9",
		Payload: []byte(`{"event":"run.ended","trace_id":"` + runTraceID + `"}`),
	})
	pub := &headerPub{}

	r, _ := NewRelay(RelayConfig{Store: store, Publisher: pub})
	if _, _, err := r.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	if got := spans[0].SpanContext().TraceID().String(); got != runTraceID {
		t.Errorf("producer span trace_id = %s, want %s (payload fallback)", got, runTraceID)
	}
}

// TestRelay_NoTrace_RootsFreshTrace confirms a row with neither carrier nor
// payload trace_id still publishes (untraced parent) — the relay never fails or
// drops an event for lack of trace context.
func TestRelay_NoTrace_RootsFreshTrace(t *testing.T) {
	withRecordingTracer(t)
	store := &fakeStore{}
	store.append(OutboxRow{Entity: "scm", ProjectID: "p1", EventType: "sync", Payload: []byte("{}")})
	pub := &headerPub{}

	r, _ := NewRelay(RelayConfig{Store: store, Publisher: pub})
	published, failed, err := r.Flush(context.Background())
	if err != nil || published != 1 || failed != 0 {
		t.Fatalf("Flush = (%d,%d,%v), want (1,0,nil)", published, failed, err)
	}
}

// TestRelayHeaderExtractsToConsumerSpan proves the consumer half of the
// ISI-5010 hop: the W3C headers the relay injects into the NATS message
// extract — exactly as subscriber.go Consume does on delivery — into a context
// whose consumer span NESTS under the relay's producer span: same trace_id,
// parent span id = the publish span's. That is the parent/child join across
// the NATS hop.
func TestRelayHeaderExtractsToConsumerSpan(t *testing.T) {
	sr := withRecordingTracer(t)

	const runTraceID = "0af7651916cd43dd8448eb211c80319c"
	store := &fakeStore{}
	store.append(OutboxRow{
		Entity: "run", ProjectID: "p1", EventType: "started", RunID: "run-1",
		Payload: []byte("{}"),
		TraceCarrier: map[string]string{
			"traceparent": "00-" + runTraceID + "-b7ad6b7169203331-01",
		},
	})
	pub := &headerPub{}

	r, err := NewRelay(RelayConfig{Store: store, Publisher: pub})
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if _, _, err := r.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("want 1 producer span, got %d", len(spans))
	}
	prod := spans[0]

	// The consumer side, as subscriber.go runs it: extract the delivered
	// headers, start the receive span under the restored parent.
	pub.mu.Lock()
	hdrs := pub.headers[0]
	pub.mu.Unlock()
	if len(hdrs) == 0 {
		t.Fatal("relay injected no headers on publish")
	}
	cctx := telemetry.Extract(context.Background(), hdrs)
	if !trace.SpanContextFromContext(cctx).IsValid() {
		t.Fatal("injected headers do not carry a valid traceparent")
	}
	_, cspan := telemetry.Tracer().Start(cctx, "nats receive ksquad.>",
		trace.WithSpanKind(trace.SpanKindConsumer))
	cspan.End()

	spans = sr.Ended()
	if len(spans) != 2 {
		t.Fatalf("want producer+consumer spans, got %d", len(spans))
	}
	cons := spans[1]
	if got := cons.SpanContext().TraceID().String(); got != runTraceID {
		t.Errorf("consumer span trace_id = %s, want run trace %s", got, runTraceID)
	}
	if got, want := cons.Parent().SpanID(), prod.SpanContext().SpanID(); got != want {
		t.Errorf("consumer span parent = %s, want the producer span %s (true child, not just same trace)",
			got, want)
	}
	if cons.SpanKind() != trace.SpanKindConsumer {
		t.Errorf("consumer span kind = %v, want Consumer", cons.SpanKind())
	}
}

func hasAttr(sp sdktrace.ReadOnlySpan, key, val string) bool {
	for _, kv := range sp.Attributes() {
		if string(kv.Key) == key && kv.Value.AsString() == val {
			return true
		}
	}
	return false
}
