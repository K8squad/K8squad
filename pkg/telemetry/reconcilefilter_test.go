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
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// dropHarness builds a TracerProvider whose export pipeline is wrapped by
// dropNoopReconcile in front of an in-memory exporter — exactly the Setup
// wiring — so a test can assert which spans survive to export.
func dropHarness(t *testing.T) (*sdktrace.TracerProvider, *tracetest.InMemoryExporter) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(dropNoopReconcile{next: sdktrace.NewSimpleSpanProcessor(exp)}),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return tp, exp
}

// TestDropNoopReconcile is the ISI-5145 end-to-end proof: a run.reconcile span
// marked AttrReconcileNoop=true is dropped from export, while an unmarked
// run.reconcile span and any other span export unchanged.
func TestDropNoopReconcile(t *testing.T) {
	tp, exp := dropHarness(t)
	tr := tp.Tracer("test")
	ctx := context.Background()

	// A changeless poll tick: run.reconcile marked no-op → dropped.
	_, noop := tr.Start(ctx, reconcileSpanName)
	noop.SetAttributes(attribute.Bool(AttrReconcileNoop, true))
	noop.End()

	// A transition pass: run.reconcile with no mark → kept.
	_, transition := tr.Start(ctx, reconcileSpanName)
	transition.End()

	// A run.reconcile explicitly marked no-op=false → kept.
	_, mar3 := tr.Start(ctx, reconcileSpanName)
	mar3.SetAttributes(attribute.Bool(AttrReconcileNoop, false))
	mar3.End()

	// A differently-named span carrying the attribute → the filter only ever
	// drops run.reconcile, so this is kept.
	_, other := tr.Start(ctx, "contextasm.assemble")
	other.SetAttributes(attribute.Bool(AttrReconcileNoop, true))
	other.End()

	spans := exp.GetSpans()
	if len(spans) != 3 {
		t.Fatalf("want 3 exported spans (1 dropped), got %d: %v", len(spans), spanNames(spans))
	}
	for _, s := range spans {
		if s.Name == reconcileSpanName {
			if v, ok := attrBoolValue(s.Attributes, AttrReconcileNoop); ok && v {
				t.Errorf("a no-op run.reconcile span leaked to export")
			}
		}
	}
}

func spanNames(spans tracetest.SpanStubs) []string {
	out := make([]string, len(spans))
	for i, s := range spans {
		out[i] = s.Name
	}
	return out
}

func attrBoolValue(attrs []attribute.KeyValue, key string) (val, present bool) {
	for _, kv := range attrs {
		if string(kv.Key) == key {
			return kv.Value.AsBool(), true
		}
	}
	return false, false
}
