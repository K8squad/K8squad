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
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/K8squad/K8squad/pkg/reconcile"
	"github.com/K8squad/K8squad/pkg/telemetry"
)

// ISI-5145: a Run in flight is re-driven every ~2s requeue, and every pass that
// advances NO durable step is poll-loop noise. The drive loop marks such a pass
// with telemetry.AttrReconcileNoop so the export pipeline drops its span; a pass
// that advances the machine (a real state transition) is left unmarked and
// exports. These tests assert the MARK — the in-memory tracer keeps every span,
// so the split is asserted on the attribute, not on the drop (which
// telemetry.TestDropNoopReconcile covers end to end).

// attrBool returns the bool value of key and whether it was present.
func attrBool(attrs []attribute.KeyValue, key string) (val, present bool) {
	for _, kv := range attrs {
		if string(kv.Key) == key {
			return kv.Value.AsBool(), true
		}
	}
	return false, false
}

// A pass that advances the machine to a terminal step is a real transition: its
// span must NOT carry the no-op mark, so it exports.
func TestReconcileTransitionSpanIsNotMarkedNoop(t *testing.T) {
	exp := installTestTracer(t)

	run := newTestRun("11111111-1111-1111-1111-111111111111", "10000000-0000-0000-0000-000000000001")
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
	if _, present := attrBool(spans[0].Attributes, telemetry.AttrReconcileNoop); present {
		t.Errorf("a state-transition pass must not be marked %s", telemetry.AttrReconcileNoop)
	}
}

// A contended pass (live foreign lease) requeues without driving — nothing
// changed — so its span carries the no-op mark and gets dropped downstream.
func TestReconcileContentionSpanIsMarkedNoop(t *testing.T) {
	exp := installTestTracer(t)

	lease := time.Now().Add(time.Minute)
	claims := &fakeClaims{found: true, state: ClaimState{
		Step: reconcile.StepClaimingSandbox, Fence: 4,
		Holder: OperatorPrincipal, RunID: "99999999-9999-9999-9999-999999999999",
		LeaseExpiresAt: &lease, ItemState: "in_progress"}, acquireOK: false}
	store := &fakeMachineStore{step: reconcile.StepClaimingSandbox, fence: 4, advanceOK: true}
	d, key := gateHarness(t, claims, store)

	rq, err := runOnce(t, d, key)
	if err != nil {
		t.Fatalf("contention must not error: %v", err)
	}
	if rq == 0 {
		t.Fatal("contention must requeue a bounded step")
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("want exactly one span, got %d", len(spans))
	}
	noop, present := attrBool(spans[0].Attributes, telemetry.AttrReconcileNoop)
	if !present || !noop {
		t.Errorf("a contended, changeless pass must be marked %s=true (present=%v value=%v)",
			telemetry.AttrReconcileNoop, present, noop)
	}
}

// A held-and-renewed pass whose drive advances no step (spin guard / the
// steady running-wait poll) is changeless: its span is marked no-op.
func TestReconcileNonAdvancingDriveSpanIsMarkedNoop(t *testing.T) {
	exp := installTestTracer(t)

	lease := time.Now().Add(time.Minute)
	claims := &fakeClaims{found: true, renewOK: true, state: ClaimState{
		Step: reconcile.StepClaimingSandbox, Fence: 4,
		Holder: OperatorPrincipal, RunID: gateRunUID, LeaseExpiresAt: &lease,
		ItemState: "in_progress"}}
	// advanceOK:false — the machine cannot commit a step this pass, so the
	// drive resolves to the SAME step it entered on (the poll shape).
	store := &fakeMachineStore{step: reconcile.StepClaimingSandbox, fence: 4, advanceOK: false}
	d, key := gateHarness(t, claims, store)

	if _, err := runOnce(t, d, key); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if store.step != reconcile.StepClaimingSandbox {
		t.Fatalf("precondition: drive must not advance, got step %q", store.step)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("want exactly one span, got %d", len(spans))
	}
	noop, present := attrBool(spans[0].Attributes, telemetry.AttrReconcileNoop)
	if !present || !noop {
		t.Errorf("a non-advancing drive pass must be marked %s=true (present=%v value=%v)",
			telemetry.AttrReconcileNoop, present, noop)
	}
}
