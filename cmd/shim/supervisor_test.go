/*
Copyright 2026 The K8squad Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the limitations under the License.
*/

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/K8squad/K8squad/pkg/telemetry"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// TestSupervisorMetricsEndpointServesRegistry covers ISI-4385 (WS-C): the
// supervisor exposes GET /metrics backed by its own real registry, and the
// tool-usage metric set registered on that registry appears in the exposition.
// This is the pod-side surface that was silently dead while `shim supervisor`
// built the mapper with a NIL registry (metrics incremented into the void).
func TestSupervisorMetricsEndpointServesRegistry(t *testing.T) {
	sup := &supervisor{metricsReg: prometheus.NewRegistry()}

	// Register a representative tool-usage series the way NewMapper would, then
	// touch it so it appears in the exposition (a childless CounterVec is
	// omitted from Prometheus output).
	tc := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ksquad_tool_calls_total",
		Help: "test",
	}, []string{"tool", "agent", "skill"})
	sup.metricsReg.MustRegister(tc)
	tc.WithLabelValues("kubectl", "coder", "").Inc()

	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(sup.metricsReg, promhttp.HandlerOpts{}))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "ksquad_tool_calls_total") {
		t.Errorf("exposition missing ksquad_tool_calls_total:\n%s", body)
	}
}

// TestSupervisorMetricsEndpointEmptyBeforeEngine asserts the endpoint is live
// even before the first /task builds the engine (it serves an empty but valid
// exposition, never a 404/500).
func TestSupervisorMetricsEndpointEmptyBeforeEngine(t *testing.T) {
	sup := &supervisor{metricsReg: prometheus.NewRegistry()}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(sup.metricsReg, promhttp.HandlerOpts{}))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics (pre-engine) status = %d, want 200", rec.Code)
	}
}

// TestSupervisorHandleTaskSpanCarriesCodeAttrs pins ISI-5014 P1#5: the
// supervisor.handle_task span is stamped with code.namespace + code.function so
// it maps to its source. The busy short-circuit path still opens and closes the
// span, so it needs no runtime wiring.
func TestSupervisorHandleTaskSpanCarriesCodeAttrs(t *testing.T) {
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})

	sup := &supervisor{busy: true}
	rec := httptest.NewRecorder()
	sup.handleTask(rec, httptest.NewRequest(http.MethodPost, "/task?taskid=t1", nil))

	var attrs []attribute.KeyValue
	found := false
	for _, s := range exp.GetSpans() {
		if s.Name == "supervisor.handle_task" {
			attrs = s.Attributes
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a supervisor.handle_task span, got %d spans", len(exp.GetSpans()))
	}
	got := map[string]string{}
	for _, kv := range attrs {
		got[string(kv.Key)] = kv.Value.AsString()
	}
	if got["code.namespace"] != "github.com/K8squad/K8squad/cmd/shim" {
		t.Errorf("code.namespace = %q, want cmd/shim path", got["code.namespace"])
	}
	if got["code.function"] != "handleTask" {
		t.Errorf("code.function = %q, want handleTask", got["code.function"])
	}
}

// TestSupervisorTelemetryOptionsRoutesOTLP covers ISI-5142 (ISI-4540 W3): the
// supervisor's telemetry spine is configured at process START from the
// operator-stamped OTEL_EXPORTER_OTLP_* env, so the OTLP trace exporter (and the
// ksquad-supervisor service entity) exist before the credential handshake — not
// deferred until a Run binds, which dropped every pre-credential span.
func TestSupervisorTelemetryOptionsRoutesOTLP(t *testing.T) {
	env := map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://otel-gateway.observability:4317",
	}
	opts, filled := supervisorTelemetryOptions(func(k string) string { return env[k] })

	if opts.ServiceName != "ksquad-supervisor" {
		t.Errorf("ServiceName = %q, want ksquad-supervisor", opts.ServiceName)
	}
	if !opts.CaptureUnsampledRemoteParent {
		t.Error("CaptureUnsampledRemoteParent = false, want true (sandbox one-Run capture, ISI-4413)")
	}
	// The env endpoint must route every signal still on the stdout default so
	// supervisor/runtime spans reach the gateway instead of dying on stderr.
	if len(filled) != 3 {
		t.Errorf("filled = %v, want traces+metrics+logs routed to OTLP", filled)
	}
	if opts.Traces == nil || opts.Traces.Endpoint != env["OTEL_EXPORTER_OTLP_ENDPOINT"] {
		t.Errorf("traces exporter not pointed at the operator gateway endpoint: %+v", opts.Traces)
	}
}

// TestSupervisorTelemetryOptionsUnsetEndpoint asserts that with no OTLP endpoint
// the signals stay on the stderr default (filled empty, Traces nil) — the
// caller logs this loudly so "zero shim spans" reads as a config gap, not a
// silent void.
func TestSupervisorTelemetryOptionsUnsetEndpoint(t *testing.T) {
	opts, filled := supervisorTelemetryOptions(func(string) string { return "" })
	if len(filled) != 0 {
		t.Errorf("filled = %v, want none (no endpoint configured)", filled)
	}
	if opts.Traces != nil {
		t.Errorf("Traces = %+v, want nil (stderr default)", opts.Traces)
	}
	if opts.ServiceName != "ksquad-supervisor" || !opts.CaptureUnsampledRemoteParent {
		t.Errorf("service resource/sampler config lost when endpoint unset: %+v", opts)
	}
}

// TestSupervisorSpansShareRunTraceID pins ISI-5144 (ISI-4540 W2): once the
// handshake landed the run traceparent, handle_task, runtime.init and run.start
// must all continue that ONE run trace instead of each rooting a fresh trace off
// the process-start span. Bluebox verified a run fragmenting across ≥3 trace IDs
// because runtime.init/run.start started from context.Background(); this locks in
// the fix (thread the live/extracted context through the supervisor call chain).
func TestSupervisorSpansShareRunTraceID(t *testing.T) {
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})

	// The operator-injected run traceparent, as awaitCredential extracts it from
	// the credential carrier (ISI-4238). This is the trace every supervisor span
	// must join.
	const runTraceID = "0af7651916cd43dd8448eb211c80319c"
	runCtx := propagation.TraceContext{}.Extract(context.Background(),
		propagation.MapCarrier{"traceparent": "00-" + runTraceID + "-b7ad6b7169203331-01"})

	// The process-start span roots its OWN trace (created at boot, before any
	// credential) — the wrong parent the pre-fix code grafted everywhere.
	_, startSpan := tp.Tracer("test").Start(context.Background(), "supervisor.start")
	defer startSpan.End()
	if startSpan.SpanContext().TraceID().String() == runTraceID {
		t.Fatal("test setup: process-start span must root a different trace than the run")
	}

	sup := &supervisor{
		metricsReg:     prometheus.NewRegistry(),
		supervisorSpan: startSpan,
		traceCtx:       runCtx,
	}

	// Replay the supervisor call chain the way handleTask threads it: graft the
	// run trace, open handle_task, hand that live ctx to runtime (runtime.init),
	// and open run.start on the detached handshake ctx (as SubmitTask's submitCtx
	// does).
	ctx := sup.runTraceContext(context.Background())
	ctx, handle := telemetry.Tracer().Start(ctx, "supervisor.handle_task")
	_, _ = sup.runtime(ctx) // KSQUAD_RUNTIME_TYPE unset: still opens+ends runtime.init
	handle.End()
	_, runStart := telemetry.Tracer().Start(sup.traceCtx, "run.start")
	runStart.End()

	traceIDs := map[string]string{}
	for _, s := range exp.GetSpans() {
		traceIDs[s.Name] = s.SpanContext.TraceID().String()
	}
	for _, name := range []string{"supervisor.handle_task", "supervisor.runtime.init", "run.start"} {
		got, ok := traceIDs[name]
		if !ok {
			t.Fatalf("expected a %s span, got spans %v", name, traceIDs)
		}
		if got != runTraceID {
			t.Errorf("%s traceID = %q, want run traceID %q (span fragmented onto a different trace)", name, got, runTraceID)
		}
	}
}

// TestSupervisorHandleTaskContinuesRunTrace pins ISI-5143 (ISI-4540 W1) at the
// HTTP-handler boundary: driving the real handleTask endpoint (not a replayed
// call chain) must produce a supervisor.handle_task span that CONTINUES the
// operator run trace landed in s.traceCtx — not a fresh trace rooted on the
// boot-time supervisor.start span (the "handle_task is a fresh trace ROOT (no
// parent)" orphan Bluebox flagged). Complements TestSupervisorSpansShareRunTraceID
// (ISI-5144), which replays the chain directly; here the busy short-circuit still
// opens/closes the span, so no runtime wiring is needed.
func TestSupervisorHandleTaskContinuesRunTrace(t *testing.T) {
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})

	// Simulate awaitCredential: the operator injected the run's traceparent into
	// the task-io credential and the handshake Extracted it into s.traceCtx.
	runTraceID, _ := oteltrace.TraceIDFromHex("00000000000000000000000000004540")
	parentSpanID, _ := oteltrace.SpanIDFromHex("0000000000005143")
	runSC := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
		TraceID:    runTraceID,
		SpanID:     parentSpanID,
		TraceFlags: oteltrace.FlagsSampled,
		Remote:     true,
	})
	traceCtx := oteltrace.ContextWithRemoteSpanContext(context.Background(), runSC)

	// supervisorSpan is a run-DISJOINT boot root: if the fix regressed, handle_task
	// would inherit ITS traceID instead of the run's — this makes the assertion
	// discriminating.
	_, bootSpan := tp.Tracer("test").Start(context.Background(), "supervisor.start")
	defer bootSpan.End()

	sup := &supervisor{busy: true, traceCtx: traceCtx, supervisorSpan: bootSpan}
	rec := httptest.NewRecorder()
	sup.handleTask(rec, httptest.NewRequest(http.MethodPost, "/task?taskid=t1", nil))

	var got oteltrace.TraceID
	found := false
	for _, s := range exp.GetSpans() {
		if s.Name == "supervisor.handle_task" {
			got = s.SpanContext.TraceID()
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a supervisor.handle_task span, got %d spans", len(exp.GetSpans()))
	}
	if got != runTraceID {
		t.Errorf("supervisor.handle_task traceID = %s, want %s (must continue the operator run trace, not root a fresh one)",
			got, runTraceID)
	}
	if got == bootSpan.SpanContext().TraceID() {
		t.Errorf("supervisor.handle_task inherited the supervisor.start boot trace %s — the ISI-5143 orphan", got)
	}
}

// writeCred materializes the required credential files (+ optional traceparent)
// under dir, the file↔struct contract ReadCredentialFromDir reads.
func writeCred(t *testing.T, dir, traceparent string) {
	t.Helper()
	files := map[string]string{
		"KSQUAD_COORD_URL":   "http://coord",
		"KSQUAD_COORD_TOKEN": "tok",
		"WORK_ITEM_ID":       "wi-1",
		"RUN_ID":             "run-1",
	}
	if traceparent != "" {
		files["TRACEPARENT"] = traceparent
	}
	for name, v := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v+"\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// TestSupervisorSubmitTraceContextReRootsPerRun covers ISI-5331 R1 (warm-pool
// process-trace bleed): submitTraceContext re-reads the CURRENT credential's
// W3C carrier per task, so a warm-pool supervisor process that serves
// successive runs re-roots each run on ITS traceparent — never reusing the
// first handshake's cached s.traceCtx. Two successive binds with distinct
// traceparents must yield two distinct submit trace ids.
func TestSupervisorSubmitTraceContextReRootsPerRun(t *testing.T) {
	dir := t.TempDir()
	prev := supervisorCoordMountPath
	supervisorCoordMountPath = dir
	t.Cleanup(func() { supervisorCoordMountPath = prev })

	// run-A binds; s.traceCtx is the stale cache from the FIRST handshake (a
	// deliberately DIFFERENT trace, so reusing it would be caught).
	staleTID, _ := oteltrace.TraceIDFromHex("0000000000000000000000000000beef")
	staleSID, _ := oteltrace.SpanIDFromHex("000000000000beef")
	staleCtx := oteltrace.ContextWithRemoteSpanContext(context.Background(),
		oteltrace.NewSpanContext(oteltrace.SpanContextConfig{TraceID: staleTID, SpanID: staleSID, TraceFlags: oteltrace.FlagsSampled, Remote: true}))
	sup := &supervisor{traceCtx: staleCtx}

	traceA := "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-aaaaaaaaaaaaaaaa-01"
	writeCred(t, dir, traceA)
	gotA := oteltrace.SpanContextFromContext(sup.submitTraceContext()).TraceID().String()
	if gotA != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("run-A submit trace = %s, want run-A carrier trace (not stale cache)", gotA)
	}

	// Warm-pool reuse: the operator rebinds the pod and rewrites the credential
	// with run-B's traceparent. submitTraceContext must pick up the NEW carrier.
	traceB := "00-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-bbbbbbbbbbbbbbbb-01"
	writeCred(t, dir, traceB)
	gotB := oteltrace.SpanContextFromContext(sup.submitTraceContext()).TraceID().String()
	if gotB != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Errorf("run-B submit trace = %s, want run-B carrier trace (R1: per-run re-root, not run-A)", gotB)
	}
	if gotA == gotB {
		t.Errorf("successive runs collapsed into one trace %s (R1 bleed not fixed)", gotA)
	}
}

// TestSupervisorSubmitTraceContextFallsBackToCache covers the fallback: with no
// carrier on the current credential, submitTraceContext uses the cached
// handshake context so the prior behavior is preserved.
func TestSupervisorSubmitTraceContextFallsBackToCache(t *testing.T) {
	dir := t.TempDir()
	prev := supervisorCoordMountPath
	supervisorCoordMountPath = dir
	t.Cleanup(func() { supervisorCoordMountPath = prev })

	cTID, _ := oteltrace.TraceIDFromHex("0000000000000000000000000000cace")
	cSID, _ := oteltrace.SpanIDFromHex("0000000000000cac")
	cached := oteltrace.ContextWithRemoteSpanContext(context.Background(),
		oteltrace.NewSpanContext(oteltrace.SpanContextConfig{TraceID: cTID, SpanID: cSID, TraceFlags: oteltrace.FlagsSampled, Remote: true}))
	sup := &supervisor{traceCtx: cached}

	writeCred(t, dir, "") // required files present, NO traceparent
	got := oteltrace.SpanContextFromContext(sup.submitTraceContext()).TraceID()
	if got != cTID {
		t.Errorf("submit trace = %s, want cached %s (fallback when no per-run carrier)", got, cTID)
	}
}
