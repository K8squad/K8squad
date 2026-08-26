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

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
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
