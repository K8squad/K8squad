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

package rundrive

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/K8squad/K8squad/pkg/reconcile"
)

// installTestTracer swaps the global TracerProvider for a synchronous in-memory
// recorder and a W3C propagator, so a Reconcile pass's span can be asserted
// without a running collector. It returns the exporter and restores the globals
// on cleanup.
func installTestTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
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
	return exp
}

// TestReconcileProducesOneRunSpan proves ISI-2915 AC3/AC5: a single drivable Run
// pass emits exactly one "run.reconcile" span carrying the Run's identity, so
// every Run is one node in a distributed trace.
func TestReconcileProducesOneRunSpan(t *testing.T) {
	exp := installTestTracer(t)

	const uid = "11111111-1111-1111-1111-111111111111"
	run := newTestRun(uid, "wi-1")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).Build()
	claims := &fakeClaims{found: true, state: ClaimState{Step: reconcile.StepPending, Fence: 1}}
	store := &fakeMachineStore{step: reconcile.StepPending, fence: 1, advanceOK: true}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: &fakeMachineEffects{}})

	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err != nil {
		t.Fatalf("drive: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("want exactly one span, got %d", len(spans))
	}
	span := spans[0]
	if span.Name != "run.reconcile" {
		t.Errorf("span name = %q, want run.reconcile", span.Name)
	}
	if got := attrString(span.Attributes, "ksquad.run.id"); got != uid {
		t.Errorf("ksquad.run.id = %q, want %q", got, uid)
	}
	if got := attrString(span.Attributes, "ksquad.run.work_item_ref"); got != "wi-1" {
		t.Errorf("ksquad.run.work_item_ref = %q, want wi-1", got)
	}
	if span.Status.Code == codes.Error {
		t.Errorf("healthy drive should not mark the span as error: %q", span.Status.Description)
	}
}

// TestReconcileSpanJoinsInboundTrace proves W3C trace-context propagation
// (AC3): a traceparent stamped on the Run's annotations makes the Run span a
// child of that inbound distributed trace rather than a fresh root.
func TestReconcileSpanJoinsInboundTrace(t *testing.T) {
	exp := installTestTracer(t)

	const parentTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	run := newTestRun("22222222-2222-2222-2222-222222222222", "wi-2")
	run.Annotations = map[string]string{
		"traceparent": "00-" + parentTrace + "-00f067aa0ba902b7-01",
	}
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).Build()
	claims := &fakeClaims{found: true, state: ClaimState{Step: reconcile.StepPending, Fence: 1}}
	store := &fakeMachineStore{step: reconcile.StepPending, fence: 1, advanceOK: true}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: &fakeMachineEffects{}})

	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err != nil {
		t.Fatalf("drive: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("want one span, got %d", len(spans))
	}
	if got := spans[0].SpanContext.TraceID().String(); got != parentTrace {
		t.Errorf("run span trace id = %q, want it to join inbound %q", got, parentTrace)
	}
	if !spans[0].Parent.IsValid() {
		t.Error("run span should have a valid remote parent from the inbound traceparent")
	}
}

func attrString(attrs []attribute.KeyValue, key string) string {
	for _, kv := range attrs {
		if string(kv.Key) == key {
			return kv.Value.AsString()
		}
	}
	return ""
}
