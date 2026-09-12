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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	wire "github.com/K8squad/K8squad/pkg/a2a"
)

// llmWriterScheme is the minimal scheme the fake client needs (Run CRD only
// for these tests — no Role/Agent fan-in like the credential-writer suite).
func llmWriterScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := api.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func llmTestRun(ns, uid string) *api.Run {
	return &api.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-" + uid[:4], Namespace: ns, UID: types.UID(uid)},
		Spec:       api.RunSpec{WorkItemRef: "10000000-0000-0000-0000-000000000001"},
	}
}

// usageEvent builds one EventUsage with the stdio-transport payload shape
// (a generic map — the writer must normalize it, not assume the typed form).
func usageEvent(taskID string, seq uint64, model string, in, out int) wire.Event {
	return wire.Event{
		Seq:       seq,
		A2ATaskID: taskID,
		TS:        time.Now().UTC(),
		Type:      wire.EventUsage,
		Payload: map[string]any{
			"model":  model,
			"input":  in,
			"output": out,
		},
	}
}

const (
	llmTaskID = "11111111-2222-3333-4444-555555555555"
	llmRunUID = "11111111-2222-3333-4444-555555555555"
)

// The core ISI-4238 projection: one EventUsage → one appended
// LLMInteraction + the running TotalTokenUsage, and a traced EventStatus →
// Run.Status.TraceID.
func TestRunLLMStatusWriter_ProjectsUsageAndTrace(t *testing.T) {
	const ns = "bmad-squad"
	run := llmTestRun(ns, llmRunUID)
	c := fake.NewClientBuilder().WithScheme(llmWriterScheme(t)).WithObjects(run).WithStatusSubresource(run).Build()
	w := NewRunLLMStatusWriter(c, nil)
	ctx := context.Background()

	if err := w.Event(ctx, wire.Event{
		Seq: 1, A2ATaskID: llmTaskID, TS: time.Now().UTC(),
		Type:    wire.EventStatus,
		Payload: wire.StatusPayload{State: wire.TaskWorking, TraceID: "4bf92f3577b34da6a3ce929d0e0e4736"},
	}); err != nil {
		t.Fatalf("status event: %v", err)
	}
	if err := w.Event(ctx, usageEvent(llmTaskID, 2, "zai/glm-5", 100, 40)); err != nil {
		t.Fatalf("usage event: %v", err)
	}
	if err := w.Event(ctx, usageEvent(llmTaskID, 3, "zai/glm-5", 10, 5)); err != nil {
		t.Fatalf("usage event 2: %v", err)
	}

	var got api.Run
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: run.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("traceID = %q, want the status-event trace id", got.Status.TraceID)
	}
	if len(got.Status.LLMInteractions) != 2 {
		t.Fatalf("llmInteractions = %d, want 2", len(got.Status.LLMInteractions))
	}
	second := got.Status.LLMInteractions[1]
	if second.ID != llmTaskID+":3" || second.Model != "zai/glm-5" || second.Type != api.InteractionResponse {
		t.Fatalf("interaction 2 = %+v", second)
	}
	if second.TokenUsage == nil || second.TokenUsage.TotalTokens != 15 {
		t.Fatalf("interaction 2 tokens = %+v, want total 15", second.TokenUsage)
	}
	tot := got.Status.TotalTokenUsage
	if tot == nil || tot.InputTokens != 110 || tot.OutputTokens != 45 || tot.TotalTokens != 155 {
		t.Fatalf("totalTokenUsage = %+v, want in=110 out=45 total=155", tot)
	}
}

// The EventSink delivery contract is at-least-once: a replayed usage event
// (same a2a_task_id + seq) must not double-append or double-count.
func TestRunLLMStatusWriter_ReplayedUsageIsIdempotent(t *testing.T) {
	const ns = "bmad-squad"
	run := llmTestRun(ns, llmRunUID)
	c := fake.NewClientBuilder().WithScheme(llmWriterScheme(t)).WithObjects(run).WithStatusSubresource(run).Build()
	w := NewRunLLMStatusWriter(c, nil)
	ctx := context.Background()

	ev := usageEvent(llmTaskID, 7, "zai/glm-5", 500, 250)
	for i := 0; i < 3; i++ { // original + two replays
		if err := w.Event(ctx, ev); err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}
	var got api.Run
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: run.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.LLMInteractions) != 1 {
		t.Fatalf("llmInteractions = %d, want exactly 1 after replays", len(got.Status.LLMInteractions))
	}
	if got.Status.TotalTokenUsage == nil || got.Status.TotalTokenUsage.TotalTokens != 750 {
		t.Fatalf("totalTokenUsage = %+v, want 750 (counted once)", got.Status.TotalTokenUsage)
	}
}

// The CR carries a bounded recent window (schema MaxItems=128) while the
// token TOTAL keeps accumulating over every event — the head drop loses no
// accounting, only the per-call summary (full fidelity rides the event
// stream).
func TestRunLLMStatusWriter_BoundedWindowRunningTotal(t *testing.T) {
	const ns = "bmad-squad"
	run := llmTestRun(ns, llmRunUID)
	c := fake.NewClientBuilder().WithScheme(llmWriterScheme(t)).WithObjects(run).WithStatusSubresource(run).Build()
	w := NewRunLLMStatusWriter(c, nil)
	ctx := context.Background()

	const total = llmInteractionsCap + 10
	for i := 1; i <= total; i++ {
		if err := w.Event(ctx, usageEvent(llmTaskID, uint64(i), "m", 1, 1)); err != nil {
			t.Fatalf("usage %d: %v", i, err)
		}
	}
	var got api.Run
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: run.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.LLMInteractions) != llmInteractionsCap {
		t.Fatalf("llmInteractions = %d, want capped at %d", len(got.Status.LLMInteractions), llmInteractionsCap)
	}
	// The window keeps the most recent entries: the FIRST retained must be
	// the (total-cap+1)-th event, the LAST the final one.
	first, last := got.Status.LLMInteractions[0], got.Status.LLMInteractions[llmInteractionsCap-1]
	if first.ID != llmTaskID+":11" || last.ID != llmTaskID+":138" {
		t.Fatalf("window = [%s .. %s], want [%s:11 .. %s:138]", first.ID, last.ID, llmTaskID, llmTaskID)
	}
	if got.Status.TotalTokenUsage == nil || got.Status.TotalTokenUsage.TotalTokens != 2*total {
		t.Fatalf("totalTokenUsage = %+v, want %d (every event counted)", got.Status.TotalTokenUsage, 2*total)
	}
}

// Observability must never kill a run: the writer swallows resolution and
// patch failures (Event returns nil) and ignores events for other runs or
// of other types without touching the API.
func TestRunLLMStatusWriter_SwallowsFailuresAndIgnoresNoise(t *testing.T) {
	const ns = "bmad-squad"
	run := llmTestRun(ns, llmRunUID)
	c := fake.NewClientBuilder().WithScheme(llmWriterScheme(t)).WithObjects(run).WithStatusSubresource(run).Build()
	w := NewRunLLMStatusWriter(c, nil)
	ctx := context.Background()

	// Unresolvable run (no CR with that uid) — logged, not surfaced.
	if err := w.Event(ctx, usageEvent("99999999-9999-9999-9999-999999999999", 1, "m", 1, 1)); err != nil {
		t.Fatalf("unresolvable run must not error the sink: %v", err)
	}
	// Unrelated event types — nil-cost pass-throughs.
	for _, typ := range []wire.EventType{wire.EventMessage, wire.EventTool, wire.EventSkillLoad, wire.EventArtifactRef} {
		if err := w.Event(ctx, wire.Event{Seq: 9, A2ATaskID: llmTaskID, TS: time.Now().UTC(), Type: typ, Payload: map[string]any{}}); err != nil {
			t.Fatalf("%s event: %v", typ, err)
		}
	}
	// Status event without a trace id — no projection owed.
	if err := w.Event(ctx, wire.Event{Seq: 10, A2ATaskID: llmTaskID, TS: time.Now().UTC(),
		Type: wire.EventStatus, Payload: wire.StatusPayload{State: wire.TaskWorking}}); err != nil {
		t.Fatalf("untraced status: %v", err)
	}
	var got api.Run
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: run.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.LLMInteractions) != 0 || got.Status.TraceID != "" {
		t.Fatalf("run status unexpectedly mutated: %+v", got.Status)
	}
}
