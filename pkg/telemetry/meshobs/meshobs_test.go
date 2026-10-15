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

// The mesh-side conformance suite (AgentMesh P1.2, ISI-5823): it drives the
// REAL pkg/telemetry/meshobs.Observer — the emitter the pod-side gateway
// plugs in — through fully-populated lifecycle events and asserts the emitted
// mesh.* spans satisfy the registry contract, mirroring the discipline the
// toolusage conformance suite established for the run trace (ISI-4387):
//
//   - every Stable Required attribute the registry documents for mesh.route /
//     mesh.spawn is present on the emitted span;
//   - no ksquad.* attribute escapes the emitter that the registry does not
//     document (the undocumented-key regression guard);
//   - the emitted span names agree with the registry constants (no drift).
package meshobs_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/K8squad/K8squad/pkg/mesh"
	"github.com/K8squad/K8squad/pkg/telemetry/meshobs"
	ksqsemconv "github.com/K8squad/K8squad/pkg/telemetry/semconv"
)

// emitMeshLifecycle drives the observer through one fully-populated lifecycle
// pass (route idle, route overflow+recreate, route error) on a real tracer +
// meter with a SpanRecorder and a manual metric reader, and returns the ended
// spans by name.
func emitMeshLifecycle(t *testing.T) (map[string]sdktrace.ReadOnlySpan, *sdkmetric.ManualReader) {
	t.Helper()

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	reader := sdkmetric.NewManualReader()
	t.Cleanup(func() { _ = reader.Shutdown(context.Background()) })
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	obs := meshobs.New(tp.Tracer("mesh-conformance"), mp.Meter("mesh-conformance"),
		meshobs.Labels{Team: "team-acme", Role: "coder", Pod: "mesh-team-acme-coder"})

	ctx := context.Background()
	obs.ObserveSpawn(ctx, mesh.SpawnEvent{
		Team: "team-acme", Role: "coder", Pod: "mesh-team-acme-coder", EndpointID: "ep-1",
		Source: mesh.SpawnSourceCold, Duration: 120 * time.Millisecond,
	})
	obs.ObserveRoute(ctx, mesh.RouteEvent{
		Team: "team-acme", Role: "coder", Pod: "mesh-team-acme-coder", EndpointID: "ep-1",
		Selection: mesh.SelectionIdle, Duration: 5 * time.Millisecond,
	})
	obs.ObserveSpawn(ctx, mesh.SpawnEvent{
		Team: "team-acme", Role: "coder", Pod: "mesh-team-acme-coder", EndpointID: "ep-2",
		Source: mesh.SpawnSourceEnvelope, Duration: 420 * time.Millisecond,
	})
	obs.ObserveRoute(ctx, mesh.RouteEvent{
		Team: "team-acme", Role: "coder", Pod: "mesh-team-acme-coder", EndpointID: "ep-2",
		Selection: mesh.SelectionOverflow, Duration: 460 * time.Millisecond,
	})
	obs.ObserveRoute(ctx, mesh.RouteEvent{
		Team: "team-acme", Role: "coder", Pod: "mesh-team-acme-coder", EndpointID: "ep-3",
		Selection: mesh.SelectionOverflow, Duration: 4 * time.Second,
		Err: errors.New("recreate deadline"),
	})

	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range sr.Ended() {
		byName[s.Name()] = s
	}
	return byName, reader
}

func keysOf(s sdktrace.ReadOnlySpan) map[string]bool {
	m := map[string]bool{}
	for _, kv := range s.Attributes() {
		m[string(kv.Key)] = true
	}
	return m
}

// TestMeshConformance_StableAttributesPresent asserts every Stable Required
// attribute the mesh registry documents is emitted on its span.
func TestMeshConformance_StableAttributesPresent(t *testing.T) {
	spans, _ := emitMeshLifecycle(t)

	for _, sc := range ksqsemconv.SpanConventions {
		if !strings.HasPrefix(sc.Name, "mesh.") {
			continue // the run-trace spans belong to the toolusage suite
		}
		span, ok := spans[sc.Name]
		if !ok {
			// Planned spans (mesh.wake, S4) are legitimately absent — nothing
			// emits them yet. Stable spans must be there.
			if len(ksqsemconv.RequiredStable(sc.Name)) > 0 {
				t.Errorf("span %q was never emitted by the mesh observer", sc.Name)
			}
			continue
		}
		got := keysOf(span)
		for _, key := range ksqsemconv.RequiredStable(sc.Name) {
			if !got[key] {
				t.Errorf("span %q missing REQUIRED stable attribute %q (contract: registry.go)", sc.Name, key)
			}
		}
	}
}

// TestMeshConformance_NoUndocumentedKeys is the mesh-side regression guard:
// any ksquad.* attribute the observer produces MUST be documented.
func TestMeshConformance_NoUndocumentedKeys(t *testing.T) {
	spans, _ := emitMeshLifecycle(t)
	documented := ksqsemconv.DocumentedSpanKeys()

	for name, span := range spans {
		for _, kv := range span.Attributes() {
			key := string(kv.Key)
			if !strings.HasPrefix(key, "ksquad.") {
				continue
			}
			if !documented[key] {
				t.Errorf("span %q emits UNDOCUMENTED attribute %q — add it to pkg/telemetry/semconv/registry.go", name, key)
			}
		}
	}
}

// TestMeshConformance_SpanNamesAgree pins the observer's span vocabulary to
// the registry constants.
func TestMeshConformance_SpanNamesAgree(t *testing.T) {
	spans, _ := emitMeshLifecycle(t)
	for _, want := range []string{ksqsemconv.SpanMeshRoute, ksqsemconv.SpanMeshSpawn} {
		if _, ok := spans[want]; !ok {
			t.Errorf("mesh span %q not emitted", want)
		}
	}
}

// TestMeshConformance_MetricsRecorded asserts the SLO instruments record on
// the meter: a2a.ready_latency + spawn.latency points, and the
// had-to-recreate counter increments exactly once per envelope spawn.
func TestMeshConformance_MetricsRecorded(t *testing.T) {
	_, reader := emitMeshLifecycle(t)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	points := map[string]int{}
	values := map[string]float64{}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			switch d := md.Data.(type) {
			case metricdata.Histogram[float64]:
				// Histogram points aggregate by attribute set — the count of
				// recordings rides dp.Count, not the slice length.
				for _, dp := range d.DataPoints {
					points[md.Name] += int(dp.Count)
				}
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					points[md.Name]++
					values[md.Name] = float64(dp.Value)
				}
			}
		}
	}
	// Three routes recorded ready-latency points; two spawns recorded
	// latency points; the had-to-recreate counter is exactly 1 (one
	// envelope recreate, one cold boot that does not count).
	if points["ksquad.mesh.a2a.ready_latency"] != 3 {
		t.Errorf("ready_latency points = %d, want 3", points["ksquad.mesh.a2a.ready_latency"])
	}
	if points["ksquad.mesh.spawn.latency"] != 2 {
		t.Errorf("spawn.latency points = %d, want 2", points["ksquad.mesh.spawn.latency"])
	}
	if values["ksquad.mesh.hibernate.misses_total"] != 1 {
		t.Errorf("hibernate.misses_total = %v, want 1", values["ksquad.mesh.hibernate.misses_total"])
	}
}

// TestMeshConformance_ProcessesGauge asserts the state gauge buckets the
// pool snapshot by bounded {team, role, state} labels.
func TestMeshConformance_ProcessesGauge(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	t.Cleanup(func() { _ = reader.Shutdown(context.Background()) })
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	if _, err := meshobs.RegisterProcessesGauge(mp.Meter("gauge-test"), "team-acme", "coder", func() map[mesh.ProcState]int64 {
		return map[mesh.ProcState]int64{mesh.StateIdle: 2, mesh.StateStopped: 1}
	}); err != nil {
		t.Fatal(err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	seen := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != "ksquad.mesh.processes.active" {
				continue
			}
			g, ok := md.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("processes.active is %T, want Gauge[int64]", md.Data)
			}
			for _, dp := range g.DataPoints {
				for _, kv := range dp.Attributes.ToSlice() {
					if string(kv.Key) == "state" {
						seen[kv.Value.AsString()] = dp.Value
					}
				}
			}
		}
	}
	if len(seen) == 0 {
		t.Fatal("ksquad.mesh.processes.active never collected")
	}
	if seen["idle"] != 2 || seen["stopped"] != 1 {
		t.Fatalf("gauge buckets = %v, want idle=2 stopped=1", seen)
	}
}
