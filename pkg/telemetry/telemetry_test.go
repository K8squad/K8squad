/*
Copyright 2026 The K8squad Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package telemetry

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// TestSetupEmitsSpanAndCorrelatedLog is the ISI-2915 end-to-end proof (AC1/AC2
// deps+provider, AC4 otelslog bridge, AC5 a trace is produced): Setup wires the
// real stdout pipelines; a span plus a log emitted under that span's context
// both land in the writer carrying the SAME trace id, i.e. logs are correlated
// to the trace.
func TestSetupEmitsSpanAndCorrelatedLog(t *testing.T) {
	buf := &bytes.Buffer{}
	logger, shutdown, err := Setup(context.Background(), Options{ServiceName: "test-svc", Writer: buf})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	ctx, span := Tracer().Start(context.Background(), "unit.span")
	traceID := span.SpanContext().TraceID().String()
	logger.InfoContext(ctx, "correlated-log-line")
	span.End()

	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "unit.span") {
		t.Errorf("span was not exported to stdout:\n%s", out)
	}
	if !strings.Contains(out, "correlated-log-line") {
		t.Errorf("log record was not exported to stdout:\n%s", out)
	}
	// The trace id appears once in the span export and once in the log export:
	// that shared id is the trace<->log correlation the otelslog bridge provides.
	if n := strings.Count(out, traceID); n < 2 {
		t.Errorf("trace id %q appears %d times, want >=2 (span + correlated log):\n%s", traceID, n, out)
	}
}

// TestSetupAllNilSignalsKeepsStdout proves the C-AC2 default is preserved: with
// every *SignalExport nil, Setup builds the stdout pipelines exactly as before
// ISI-3620 — a span emitted after Setup still lands in the configured Writer.
func TestSetupAllNilSignalsKeepsStdout(t *testing.T) {
	buf := &bytes.Buffer{}
	_, shutdown, err := Setup(context.Background(), Options{ServiceName: "test-svc", Writer: buf})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	_, span := Tracer().Start(context.Background(), "stdout.default.span")
	span.End()
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "stdout.default.span") {
		t.Errorf("all-nil signals should keep stdout default; span not in writer:\n%s", out)
	}
}

// TestSetupOTLPSignalDoesNotBlockOrError proves the opt-in OTLP path is
// offline-safe: a non-nil SignalExport pointed at an unreachable endpoint still
// returns nil error and a working shutdown, because the OTLP batchers connect
// lazily. No network is required for the test.
func TestSetupOTLPSignalDoesNotBlockOrError(t *testing.T) {
	buf := &bytes.Buffer{}
	unreachable := &SignalExport{Protocol: "grpc", Endpoint: "http://127.0.0.1:0"}
	_, shutdown, err := Setup(context.Background(), Options{
		ServiceName: "test-svc",
		Writer:      buf,
		Traces:      unreachable,
		Metrics:     unreachable,
		Logs:        unreachable,
	})
	if err != nil {
		t.Fatalf("Setup with unreachable OTLP endpoint returned error: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Shutdown may report a flush error against the dead endpoint; it must not
	// hang. We only require that it returns.
	_ = shutdown(ctx)
}

// TestSetupRecordsMetric is the ISI-3593 metric prerequisite proof: Setup wires
// a real stdout MeterProvider; a counter incremented through the process Meter()
// lands in the writer after shutdown flushes the pipeline, carrying its own name
// and the resource service.name — i.e. Meter() is now a usable, exporting meter,
// which every bootstrap-path metric (ksquad.contextasm.* / ksquad.taskio.*)
// depends on.
func TestSetupRecordsMetric(t *testing.T) {
	buf := &bytes.Buffer{}
	_, shutdown, err := Setup(context.Background(), Options{ServiceName: "test-svc", Writer: buf})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	counter, err := Meter().Int64Counter("ksquad.test.metric")
	if err != nil {
		t.Fatalf("Int64Counter: %v", err)
	}
	counter.Add(context.Background(), 7)

	// Shutdown flushes the PeriodicReader, emitting the collected metric to buf.
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "ksquad.test.metric") {
		t.Errorf("metric was not exported to stdout:\n%s", out)
	}
	if !strings.Contains(out, "test-svc") {
		t.Errorf("metric export missing resource service.name:\n%s", out)
	}
}

// TestSetupCarriesVCSRepositoryURLFull is the P3#8 / ISI-5016 proof: the
// telemetry resource must carry vcs.repository.url.full so every span's service
// maps to its repository deterministically (a backend must not guess the
// service↔repo mapping by name resemblance). It also verifies an explicit
// RepositoryURL override wins over the default.
func TestSetupCarriesVCSRepositoryURLFull(t *testing.T) {
	cases := []struct {
		name          string
		repositoryURL string
		wantURL       string
	}{
		{"defaults to K8squad repo", "", "https://github.com/K8squad/K8squad"},
		{"honors explicit override", "https://github.com/acme/widget", "https://github.com/acme/widget"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			_, shutdown, err := Setup(context.Background(), Options{
				ServiceName:   "test-svc",
				RepositoryURL: tc.repositoryURL,
				Writer:        buf,
			})
			if err != nil {
				t.Fatalf("Setup: %v", err)
			}
			_, span := Tracer().Start(context.Background(), "vcs.resource.span")
			span.End()
			if err := shutdown(context.Background()); err != nil {
				t.Fatalf("shutdown: %v", err)
			}

			out := buf.String()
			if !strings.Contains(out, "vcs.repository.url.full") {
				t.Errorf("span export missing resource vcs.repository.url.full:\n%s", out)
			}
			if !strings.Contains(out, tc.wantURL) {
				t.Errorf("span export missing vcs.repository.url.full value %q:\n%s", tc.wantURL, out)
			}
		})
	}
}

// TestMeterSafeBeforeSetup proves the no-op-before-Setup contract the accessor
// promises: recording through Meter() with no MeterProvider installed neither
// panics nor errors, so bootstrap-path code can build instruments at init time.
func TestMeterSafeBeforeSetup(t *testing.T) {
	restore := installNoopMeter(t)
	defer restore()

	counter, err := Meter().Int64Counter("ksquad.test.preinit")
	if err != nil {
		t.Fatalf("Int64Counter before Setup: %v", err)
	}
	// Must not panic.
	counter.Add(context.Background(), 1)
}

// installNoopMeter clears any globally-installed MeterProvider so Meter() falls
// back to the API's no-op, returning a restore func.
func installNoopMeter(t *testing.T) func() {
	t.Helper()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(noop.NewMeterProvider())
	return func() { otel.SetMeterProvider(prev) }
}

// TestSetupRegistersGlobalPropagator pins an invariant that is load-bearing but
// otherwise untested (ISI-4540 review): Setup must register a WORKING W3C
// propagator as the OTel GLOBAL. internal/apiserver wraps its router in
// otelhttp.NewHandler, and otelhttp snapshots otel.GetTextMapPropagator() at
// handler-construction time — so if this registration ever disappears or moves
// after handler construction, every inbound apiserver request silently stops
// joining its caller's trace (the same no-op-propagator failure this PR fixes,
// one layer out). Assert via behavior so the test is robust to the global's
// internal delegation wrapper.
func TestSetupRegistersGlobalPropagator(t *testing.T) {
	prevProp := otel.GetTextMapPropagator()
	// Start from a no-op global so a stale registration can't pass the test.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
	t.Cleanup(func() { otel.SetTextMapPropagator(prevProp) })

	_, shutdown, err := Setup(context.Background(), Options{ServiceName: "test-svc", Writer: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	const parentTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	carrier := map[string]string{"traceparent": "00-" + parentTrace + "-00f067aa0ba902b7-01"}
	sc := trace.SpanContextFromContext(
		otel.GetTextMapPropagator().Extract(context.Background(), propagation.MapCarrier(carrier)))
	if !sc.IsValid() || sc.TraceID().String() != parentTrace {
		t.Errorf("Setup did not register a working W3C propagator as the OTel global "+
			"(otelhttp snapshots it at handler construction); extracted valid=%v trace=%q",
			sc.IsValid(), sc.TraceID().String())
	}
}

// TestExtractJoinsW3CTrace proves AC3 inbound: Extract lifts a W3C traceparent
// out of a carrier so the next span joins the caller's distributed trace.
func TestExtractJoinsW3CTrace(t *testing.T) {
	restore := installRecorder(t)
	defer restore()

	const parentTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	carrier := map[string]string{"traceparent": "00-" + parentTrace + "-00f067aa0ba902b7-01"}

	ctx := Extract(context.Background(), carrier)
	_, span := Tracer().Start(ctx, "child")
	defer span.End()

	if got := span.SpanContext().TraceID().String(); got != parentTrace {
		t.Errorf("child trace id = %q, want joined %q", got, parentTrace)
	}
}

// TestExtractWorksBeforeSetup is the ISI-4540 regression proof. The sandbox
// entrypoints (`shim run` / `shim supervisor`) call Extract to continue the
// operator-injected run trace BEFORE Setup installs the global propagator. If
// Extract read otel.GetTextMapPropagator() it would get the SDK-default no-op at
// that point and silently drop the inbound traceparent, so every sandbox span
// rooted its own trace — Henrik's "single-span / disconnected" traces. This
// forces the global propagator to a no-op (the true pre-Setup state, which
// installRecorder does NOT reproduce because it sets a TraceContext global) and
// proves Extract still joins the caller's distributed trace.
func TestExtractWorksBeforeSetup(t *testing.T) {
	restore := installNoopPropagator(t)
	defer restore()

	const parentTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	const parentSpan = "00f067aa0ba902b7"
	carrier := map[string]string{"traceparent": "00-" + parentTrace + "-" + parentSpan + "-01"}

	ctx := Extract(context.Background(), carrier)

	// Assert on the extracted span context directly — this exercises Extract
	// itself rather than the SDK's parenting. Trace id AND parent span id must
	// survive: a propagator that recovered only the trace id would yield the flat,
	// broken-hierarchy shape that is easy to confuse with the bug being fixed. The
	// parent must be remote, and its sampled flag (from -01) must carry through,
	// otherwise the CaptureUnsampledRemoteParent net would needlessly rescue it.
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		t.Fatalf("Extract before Setup produced no valid span context (traceparent dropped)")
	}
	if got := sc.TraceID().String(); got != parentTrace {
		t.Errorf("trace id = %q, want joined %q (propagator-ordering regression)", got, parentTrace)
	}
	if got := sc.SpanID().String(); got != parentSpan {
		t.Errorf("parent span id = %q, want injected %q (trace joined but hierarchy broken)", got, parentSpan)
	}
	if !sc.IsRemote() {
		t.Errorf("extracted parent not marked remote")
	}
	if !sc.IsSampled() {
		t.Errorf("extracted parent lost its sampled flag (-01)")
	}

	// End-to-end: a child started from that ctx joins the caller's trace.
	_, span := Tracer().Start(ctx, "child")
	defer span.End()
	if got := span.SpanContext().TraceID().String(); got != parentTrace {
		t.Errorf("child trace id = %q, want joined %q "+
			"(sandbox spans would otherwise root their own trace)", got, parentTrace)
	}
}

// TestInjectWorksBeforeSetup is the outbound twin of TestExtractWorksBeforeSetup
// (ISI-4540 review). Inject was changed in the same way as Extract but had no
// guard: TestInjectWritesW3CTrace uses installRecorder, which sets a TraceContext
// global first and masks the ordering dependency. Reverting Inject to
// otel.GetTextMapPropagator() left the whole suite green. This forces a no-op
// global (the true pre-Setup state) and proves Inject still emits a traceparent.
func TestInjectWorksBeforeSetup(t *testing.T) {
	restore := installNoopPropagator(t)
	defer restore()

	ctx, span := Tracer().Start(context.Background(), "producer")
	defer span.End()
	traceID := span.SpanContext().TraceID().String()

	carrier := map[string]string{}
	Inject(ctx, carrier)

	if tp := carrier["traceparent"]; !strings.Contains(tp, traceID) {
		t.Errorf("Inject before Setup wrote %q, want a traceparent carrying trace id %q "+
			"(no-op global would have emitted nothing)", tp, traceID)
	}
}

// installNoopPropagator forces the OTel global text-map propagator to a genuine
// no-op (a composite with no delegates — exactly what the global is before Setup
// runs) and installs a recording tracer, returning a restore func. It reproduces
// the sandbox-entrypoint state that Extract/Inject must work in (ISI-4540);
// installRecorder does NOT, because it sets a TraceContext global that masks the
// ordering bug. Mirrors installNoopMeter.
func installNoopPropagator(t *testing.T) func() {
	t.Helper()
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(tracetest.NewInMemoryExporter()))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
	return func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	}
}

// TestInjectWritesW3CTrace proves AC3 outbound: Inject serializes the current
// span context into a carrier as a W3C traceparent for a downstream process.
func TestInjectWritesW3CTrace(t *testing.T) {
	restore := installRecorder(t)
	defer restore()

	ctx, span := Tracer().Start(context.Background(), "producer")
	defer span.End()
	traceID := span.SpanContext().TraceID().String()

	carrier := map[string]string{}
	Inject(ctx, carrier)

	tp := carrier["traceparent"]
	if tp == "" {
		t.Fatalf("Inject wrote no traceparent: %v", carrier)
	}
	if !strings.Contains(tp, traceID) {
		t.Errorf("traceparent %q does not carry trace id %q", tp, traceID)
	}
}

// TestCaptureUnsampledRemoteParent is the ISI-4413 head-sampling proof: a
// sandbox continues the operator-injected credential traceparent, and when that
// remote parent carries sampled=0 the SDK-default ParentBased(AlwaysSample)
// sampler head-drops the WHOLE run subtree (run.start/llm.call/run.end never
// reach the collector). Options.CaptureUnsampledRemoteParent must flip ONLY
// that case to sample, while still honoring a sampled remote parent, and still
// rooting a fresh trace when there is no parent.
func TestCaptureUnsampledRemoteParent(t *testing.T) {
	traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")

	remoteParent := func(sampled bool) context.Context {
		flags := trace.TraceFlags(0)
		if sampled {
			flags = flags.WithSampled(true)
		}
		sc := trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    traceID,
			SpanID:     spanID,
			TraceFlags: flags,
			Remote:     true,
		})
		return trace.ContextWithRemoteSpanContext(context.Background(), sc)
	}

	cases := []struct {
		name        string
		capture     bool
		parentCtx   context.Context
		wantSampled bool
	}{
		{"default drops unsampled remote parent", false, remoteParent(false), false},
		{"capture keeps unsampled remote parent", true, remoteParent(false), true},
		{"capture still roots a fresh trace", true, context.Background(), true},
		{"capture honors a sampled remote parent", true, remoteParent(true), true},
		{"default honors a sampled remote parent", false, remoteParent(true), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			_, shutdown, err := Setup(context.Background(), Options{
				ServiceName:                  "sandbox-svc",
				Writer:                       buf,
				CaptureUnsampledRemoteParent: tc.capture,
			})
			if err != nil {
				t.Fatalf("Setup: %v", err)
			}
			defer func() { _ = shutdown(context.Background()) }()

			_, span := Tracer().Start(tc.parentCtx, "run.start")
			got := span.SpanContext().IsSampled()
			span.End()
			if got != tc.wantSampled {
				t.Errorf("IsSampled() = %v, want %v", got, tc.wantSampled)
			}
		})
	}
}

// TestCaptureUnsampledRemoteParentComposesWithCRSampler is the ISI-4540 review
// fix. CaptureUnsampledRemoteParent must COMPOSE with a CR-declared sampler, not
// be suppressed by it. Before the fix, any non-nil Traces.Sampler took over and
// its ParentBased(root) defaulted remoteParentNotSampled to NeverSample — so the
// ISI-4413 safety net silently switched off for exactly the deployments that
// configure sampling (even "always_on" dropped an unsampled remote parent). With
// each CR sampler set alongside capture: an unsampled remote parent (the injected
// run traceparent) is ALWAYS recorded, while a fresh ROOT still honors the CR
// decision.
func TestCaptureUnsampledRemoteParentComposesWithCRSampler(t *testing.T) {
	traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	unsampledRemote := func() context.Context {
		sc := trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: traceID, SpanID: spanID, TraceFlags: 0, Remote: true,
		})
		return trace.ContextWithRemoteSpanContext(context.Background(), sc)
	}

	cases := []struct {
		name         string
		sampler      *SamplerSpec
		wantRootKept bool
	}{
		{"always_on", &SamplerSpec{Type: "always_on"}, true},
		{"always_off", &SamplerSpec{Type: "always_off"}, false},
		{"probabilistic ratio 0", &SamplerSpec{Type: "probabilistic", Ratio: 0}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			_, shutdown, err := Setup(context.Background(), Options{
				ServiceName: "sandbox-svc",
				Writer:      buf,
				// Endpoint connects lazily (see TestSetupOTLPSignalDoesNotBlockOrError),
				// so no network is required; we only need Sampler to be honored.
				Traces:                       &SignalExport{Protocol: "grpc", Endpoint: "http://127.0.0.1:0", Sampler: tc.sampler},
				CaptureUnsampledRemoteParent: true,
			})
			if err != nil {
				t.Fatalf("Setup: %v", err)
			}
			// Bound shutdown: the endpoint is dead, so flush would otherwise block
			// on the exporter's retry window (cf. TestSetupOTLPSignalDoesNotBlockOrError).
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_ = shutdown(ctx)
			}()

			// The run's injected parent is unsampled — it MUST still be captured
			// regardless of the CR sampler (the whole point of the safety net).
			_, runSpan := Tracer().Start(unsampledRemote(), "run.start")
			if !runSpan.SpanContext().IsSampled() {
				t.Errorf("unsampled remote parent dropped under CR sampler %q — safety net switched off", tc.name)
			}
			runSpan.End()

			// A fresh ROOT (no parent) must still honor the CR sampler's decision.
			_, rootSpan := Tracer().Start(context.Background(), "root")
			if got := rootSpan.SpanContext().IsSampled(); got != tc.wantRootKept {
				t.Errorf("fresh root IsSampled() = %v, want %v (CR sampler %q must still govern roots)",
					got, tc.wantRootKept, tc.name)
			}
			rootSpan.End()
		})
	}
}

// installRecorder points the global tracer at a sampling in-memory provider and
// a W3C propagator, returning a restore func.
func installRecorder(t *testing.T) func() {
	t.Helper()
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(tracetest.NewInMemoryExporter()))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	}
}
