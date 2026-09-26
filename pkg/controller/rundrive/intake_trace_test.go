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

// intake_trace_test.go — ISI-5010: the operator-side halves of the trace join.
// The sweep stamps the mint pass's W3C context onto the Run as raw annotation
// headers; the driver (otel_test.go TestReconcileSpanJoinsInboundTrace, already
// committed) Extracts those annotations before starting run.reconcile. These
// tests prove the mint half and the two halves joined end-to-end: one run, one
// trace, run.reconcile nested under the sweep that minted it.
package rundrive

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/types"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/reconcile"
	"github.com/K8squad/K8squad/pkg/telemetry"
)

// findSpan returns the first recorded span with the given name.
func findSpan(t *testing.T, spans []tracetest.SpanStub, name string) tracetest.SpanStub {
	t.Helper()
	for _, sp := range spans {
		if sp.Name == name {
			return sp
		}
	}
	t.Fatalf("no span named %q recorded (have %d spans)", name, len(spans))
	return tracetest.SpanStub{}
}

// traceTestAttrBool reads a bool span attribute; ""-valued when absent.
func traceTestAttrBool(attrs []attribute.KeyValue, key string) (bool, bool) {
	for _, kv := range attrs {
		if string(kv.Key) == key {
			return kv.Value.AsBool(), true
		}
	}
	return false, false
}

// TestIntakeSweepStampsTraceparentOnRun proves the mint half of ISI-5010: a
// sweep running under an active span stamps the Run's annotations with a raw
// W3C traceparent on that same trace — the exact carrier the driver Extracts —
// and the sweep itself is traced as a child of the inbound span.
func TestIntakeSweepStampsTraceparentOnRun(t *testing.T) {
	exp := installTestTracer(t)

	const (
		itemID  = "11111111-1111-1111-1111-111111111111"
		teamUID = "22222222-2222-2222-2222-222222222222"
	)

	objs := squadGraph(teamUID, "squad-alpha", "alpha", "coder", "proj")
	src := &fakeIntakeSource{
		items: []IntakeItem{{ID: itemID, TeamID: teamUID, ProjectID: "proj"}},
	}
	in, cl, _ := newIntake(t, src, objs...)

	// The board-side producer: one live span the minted Run should join.
	ctx, parent := otel.Tracer("test").Start(context.Background(), "board.dispatch")
	in.sweep(ctx)
	parent.End()

	var runs api.RunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs.Items) != 1 {
		t.Fatalf("expected 1 minted run, got %d", len(runs.Items))
	}
	run := runs.Items[0]

	// (1) The annotation is a raw W3C traceparent — not JSON, not nested —
	// because the annotations map is the propagation carrier the driver
	// feeds straight to telemetry.Extract.
	tp := run.Annotations["traceparent"]
	if tp == "" {
		t.Fatal("minted Run carries no traceparent annotation")
	}
	if want := "00-"; len(tp) < len(want) || tp[:len(want)] != want {
		t.Errorf("traceparent annotation %q is not a raw W3C value", tp)
	}

	// (2) Driver-side contract: extracting the annotations yields the
	// producer's trace, so run.reconcile joins it instead of rooting a new one.
	sc := trace.SpanContextFromContext(telemetry.Extract(context.Background(), run.Annotations))
	if !sc.IsValid() {
		t.Fatalf("traceparent annotation %q does not extract to a valid span context", tp)
	}
	if got, want := sc.TraceID().String(), parent.SpanContext().TraceID().String(); got != want {
		t.Errorf("annotation trace id = %s, want the producer's %s", got, want)
	}

	// (3) The mint pass itself was traced under the producer.
	sweepSpan := findSpan(t, exp.GetSpans(), "intake.sweep")
	if got, want := sweepSpan.Parent.SpanID(), parent.SpanContext().SpanID(); got != want {
		t.Errorf("intake.sweep parent = %s, want the producer's %s", got, want)
	}
}

// TestIntakeSweepWithoutTracerStampsNoTraceparent pins the degraded path:
// with no SDK installed (otel's default no-op provider — exactly the
// pre-telemetry production shape) the sweep's span carries an invalid span
// context, Inject writes nothing, and the Run is minted with NO traceparent
// annotation — behavior unchanged, and the driver still roots its own trace.
func TestIntakeSweepWithoutTracerStampsNoTraceparent(t *testing.T) {
	const (
		itemID  = "11111111-1111-1111-1111-111111111111"
		teamUID = "22222222-2222-2222-2222-222222222222"
	)

	objs := squadGraph(teamUID, "squad-alpha", "alpha", "coder", "proj")
	src := &fakeIntakeSource{
		items: []IntakeItem{{ID: itemID, TeamID: teamUID, ProjectID: "proj"}},
	}
	in, cl, _ := newIntake(t, src, objs...)

	// Deliberately NO installTestTracer here: the global provider stays
	// otel's no-op default.
	in.sweep(context.Background())

	var runs api.RunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs.Items) != 1 {
		t.Fatalf("expected 1 minted run, got %d", len(runs.Items))
	}
	run := runs.Items[0]
	// Fan-out mint (no requested agent) carries the agent name segment.
	if want := "intake-" + itemID + "-coder"; run.Name != want {
		t.Errorf("minted run name = %q, want %q (no mint regression)", run.Name, want)
	}
	if tp, ok := run.Annotations["traceparent"]; ok {
		t.Errorf("sweep without a recording tracer must not stamp a traceparent, got %q", tp)
	}
}

// TestMintedRunReconcileJoinsMintTrace is the ISI-5010 acceptance criterion
// end-to-end inside the operator: intake mints the Run under a traced sweep,
// then the driver reconciles that same Run object. Both spans land on ONE
// trace — run.reconcile nests under intake.sweep — and run.reconcile carries
// request.is_root_span=true as the operator flow anchor.
func TestMintedRunReconcileJoinsMintTrace(t *testing.T) {
	exp := installTestTracer(t)

	const (
		itemID  = "11111111-1111-1111-1111-111111111111"
		teamUID = "22222222-2222-2222-2222-222222222222"
	)

	objs := squadGraph(teamUID, "squad-alpha", "alpha", "coder", "proj")
	src := &fakeIntakeSource{
		items: []IntakeItem{{ID: itemID, TeamID: teamUID, ProjectID: "proj"}},
	}
	in, cl, _ := newIntake(t, src, objs...)

	// Mint: the sweep roots the run's trace (no inbound span on ctx).
	in.sweep(context.Background())

	var runs api.RunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs.Items) != 1 {
		t.Fatalf("expected 1 minted run, got %d", len(runs.Items))
	}
	minted := runs.Items[0]

	// Drive the minted Run with the standard driver harness.
	claims := &fakeClaims{found: true, state: ClaimState{Step: reconcile.StepPending, Fence: 1}}
	store := &fakeMachineStore{step: reconcile.StepPending, fence: 1, advanceOK: true}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: &fakeMachineEffects{}})
	if _, err := runOnce(t, d, types.NamespacedName{Namespace: minted.Namespace, Name: minted.Name}); err != nil {
		t.Fatalf("drive minted run: %v", err)
	}

	spans := exp.GetSpans()
	sweepSpan := findSpan(t, spans, "intake.sweep")
	reconSpan := findSpan(t, spans, "run.reconcile")

	// One connected trace: mint → reconcile sharing one trace_id, with
	// run.reconcile a true child of the sweep span.
	if got, want := reconSpan.SpanContext.TraceID().String(),
		sweepSpan.SpanContext.TraceID().String(); got != want {
		t.Errorf("run.reconcile trace id = %s, want the mint trace %s", got, want)
	}
	if got, want := reconSpan.Parent.SpanID(), sweepSpan.SpanContext.SpanID(); got != want {
		t.Errorf("run.reconcile parent = %s, want the intake.sweep span %s", got, want)
	}

	// The operator flow anchor (ISI-5010): the reconcile root is marked.
	if v, ok := traceTestAttrBool(reconSpan.Attributes, "request.is_root_span"); !ok || !v {
		t.Errorf("run.reconcile request.is_root_span = (%v, %v), want (true, true)",
			v, ok)
	}
}
