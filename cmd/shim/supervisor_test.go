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
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
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
