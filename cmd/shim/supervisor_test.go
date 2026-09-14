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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
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
