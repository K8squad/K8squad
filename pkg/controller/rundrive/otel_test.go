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
	"errors"
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
	run := newTestRun(uid, "10000000-0000-0000-0000-000000000001")
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
	if got := attrString(span.Attributes, "ksquad.run.work_item_ref"); got != "10000000-0000-0000-0000-000000000001" {
		t.Errorf("ksquad.run.work_item_ref = %q, want 10000000-0000-0000-0000-000000000001", got)
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
	run := newTestRun("22222222-2222-2222-2222-222222222222", "20000000-0000-0000-0000-000000000002")
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

// TestReconcileSpanStatusOnSuccess proves ISI-5011 P0#2 (success half): a clean
// drive that reaches a terminal success step marks the run.reconcile span
// status Ok — never leaving a success span's status unset.
func TestReconcileSpanStatusOnSuccess(t *testing.T) {
	exp := installTestTracer(t)

	const uid = "33333333-3333-3333-3333-333333333333"
	run := newTestRun(uid, "30000000-0000-0000-0000-000000000003")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).Build()
	claims := &fakeClaims{found: true, state: ClaimState{Step: reconcile.StepPending, Fence: 1, ItemState: "todo"},
		acquireOK: true, acquireFence: 1}
	store := &fakeMachineStore{step: reconcile.StepPending, fence: 1, advanceOK: true}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: &fakeMachineEffects{}})

	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err != nil {
		t.Fatalf("drive: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("want exactly one span, got %d", len(spans))
	}
	if spans[0].Status.Code != codes.Ok {
		t.Errorf("success span status = %v, want Ok (description %q)",
			spans[0].Status.Code, spans[0].Status.Description)
	}
}

// TestReconcileSpanStatusOnDriveError proves ISI-5011 P0#2 (error half): a
// drive that returns an infra error marks the span Error and records an
// exception event, so span.status_code and the exception signal (which Bluebox
// derives request.is_failed from) stay coupled to the same outcome.
func TestReconcileSpanStatusOnDriveError(t *testing.T) {
	exp := installTestTracer(t)

	const uid = "44444444-4444-4444-4444-444444444444"
	run := newTestRun(uid, "40000000-0000-0000-0000-000000000004")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).Build()
	claims := &fakeClaims{found: true, stateErr: errors.New("conn refused")}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{
		store: &fakeMachineStore{step: reconcile.StepPending, fence: 1},
	})

	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err == nil {
		t.Fatal("want a drive error, got nil")
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("want exactly one span, got %d", len(spans))
	}
	span := spans[0]
	if span.Status.Code != codes.Error {
		t.Errorf("error span status = %v, want Error", span.Status.Code)
	}
	if !hasExceptionEvent(span.Events) {
		t.Errorf("error span should record an exception event, got %d events", len(span.Events))
	}
}

// TestReconcileSpanStatusOnFailedStep proves the decoupling fix itself: a Run
// whose durable step is already the terminal StepFailed absorbs with NO Go
// error, yet its span must still read Error (previously left unset because the
// span status only tracked the returned error, not the durable step).
func TestReconcileSpanStatusOnFailedStep(t *testing.T) {
	exp := installTestTracer(t)

	const uid = "55555555-5555-5555-5555-555555555555"
	run := newTestRun(uid, "50000000-0000-0000-0000-000000000005")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).Build()
	claims := &fakeClaims{found: true, state: ClaimState{Step: reconcile.StepFailed, Fence: 1}}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{
		store: &fakeMachineStore{step: reconcile.StepFailed, fence: 1},
	})

	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err != nil {
		t.Fatalf("absorbing a terminal failed step should not error: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("want exactly one span, got %d", len(spans))
	}
	if spans[0].Status.Code != codes.Error {
		t.Errorf("failed-step span status = %v, want Error (description %q)",
			spans[0].Status.Code, spans[0].Status.Description)
	}
}

// hasExceptionEvent reports whether the recorded span events contain the OTel
// exception event the SDK emits for span.RecordError.
func hasExceptionEvent(events []sdktrace.Event) bool {
	for _, ev := range events {
		if ev.Name == "exception" {
			return true
		}
	}
	return false
}
