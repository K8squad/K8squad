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
	"encoding/json"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	wire "github.com/K8squad/K8squad/pkg/a2a"
)

// runllm_status.go — the ISI-4238 operator-side LLM-observability projection:
// the run-drive consumer that turns the §4 wire event stream into
// Run.status fields the console and APIs can read.
//
//	Run.Status.TraceID        ← EventStatus.TraceID   (live, first status
//	                             event after the run span opened)
//	Run.Status.LLMInteractions← EventUsage            (one bounded entry per
//	                             model round-trip, idempotent on event seq)
//	Run.Status.TotalTokenUsage← EventUsage            (running sum over ALL
//	                             usage events, not just the retained window)
//
// It implements the internal-a2a EventSink contract and chains AHEAD of
// RunEvents (the ProgressMirror) inside the TelemetrySink
// (OperatorDispatchConfig.LLMStatus → sinkFor), sharing the layering of
// RunStatusSandboxWriter: it patches ONLY the effects-owned status fields
// the run status projector preserves (pkg/controller/run/status.go never
// writes these), so the writers cannot fight.
//
// Failure posture — observability must never kill a run: the EventSink
// contract aborts the dispatch on a sink error, so this sink swallows its
// own failures internally (logs, returns nil), exactly what the contract
// prescribes for tolerant sinks.

// llmInteractionsCap mirrors the CRD schema bound
// (MaxItems=128 on RunStatus.LLMInteractions): the CR carries the bounded
// recent window; the full-fidelity record rides the §4 event stream.
const llmInteractionsCap = 128

// RunLLMStatusWriter projects LLM observability facts from a run's wire
// events onto that Run's status subresource (ISI-4238). It is stateless
// beyond the k8s client — every event resolves its Run from the
// a2a_task_id (equal to the Run uid, spec §3 V1) — so one shared instance
// safely serves every in-flight Run (each follow goroutine is
// single-threaded per Run, and different Runs patch different CRs).
type RunLLMStatusWriter struct {
	client client.Client
	now    func() time.Time
	log    func(format string, args ...any)
}

// NewRunLLMStatusWriter binds the writer over the manager's client. A nil
// client yields nil (projection-off), mirroring NewRunStatusSandboxWriter.
// log receives the never-fatal projection failures; nil falls back to a
// no-op (tests).
func NewRunLLMStatusWriter(c client.Client, log func(string, ...any)) *RunLLMStatusWriter {
	if c == nil {
		return nil
	}
	if log == nil {
		log = func(string, ...any) {}
	}
	return &RunLLMStatusWriter{client: c, now: time.Now, log: log}
}

// Event implements a2a.EventSink. Only EventUsage and EventStatus with a
// TraceID touch the CR; every other event type is a nil-cost pass-through.
// It NEVER returns an error (see the failure posture above).
func (w *RunLLMStatusWriter) Event(ctx context.Context, ev wire.Event) error {
	if w == nil {
		return nil
	}
	switch ev.Type {
	case wire.EventUsage:
		p, ok := llmUsagePayload(ev.Payload)
		if !ok {
			return nil
		}
		w.projectUsage(ctx, ev, p)
	case wire.EventStatus:
		p, ok := llmStatusPayload(ev.Payload)
		if !ok || p.TraceID == "" {
			return nil
		}
		w.projectTraceID(ctx, ev, p.TraceID)
	}
	return nil
}

// projectUsage appends one LLMInteraction per EventUsage and keeps the
// running TotalTokenUsage. Idempotent on the event's (task, seq) — the
// EventSink delivery contract is at-least-once, so a replayed event must
// not double-count (the interaction ID embeds the seq; a known ID is a
// no-op).
func (w *RunLLMStatusWriter) projectUsage(ctx context.Context, ev wire.Event, p wire.UsagePayload) {
	run, err := runByUIDFrom(ctx, w.client, cleanRunID(ev.A2ATaskID))
	if err != nil {
		w.log("rundrive.RunLLMStatusWriter: resolve run %s for usage: %v", cleanRunID(ev.A2ATaskID), err)
		return
	}
	id := llmInteractionID(ev)
	for i := range run.Status.LLMInteractions {
		if run.Status.LLMInteractions[i].ID == id {
			return // replayed event (at-least-once delivery): already projected
		}
	}
	ts := ev.TS
	if ts.IsZero() {
		ts = w.now()
	}
	inter := api.LLMInteraction{
		ID:         id,
		Type:       api.InteractionResponse,
		Model:      p.Model,
		Timestamp:  metav1.Time{Time: ts},
		DurationMs: p.DurationMS,
		TokenUsage: &api.TokenUsage{
			InputTokens:  int64(p.Input),
			OutputTokens: int64(p.Output),
			TotalTokens:  int64(p.Input + p.Output),
		},
	}
	next := append(run.Status.LLMInteractions, inter)
	if len(next) > llmInteractionsCap {
		// Schema bound: keep the MOST RECENT window. The dropped head is
		// not lost — the full record stays queryable in the event stream.
		next = next[len(next)-llmInteractionsCap:]
	}
	total := run.Status.TotalTokenUsage.DeepCopy()
	if total == nil {
		total = &api.TokenUsage{}
	}
	total.InputTokens += int64(p.Input)
	total.OutputTokens += int64(p.Output)
	total.TotalTokens = total.InputTokens + total.OutputTokens

	patch := map[string]any{
		"llmInteractions": next,
		"totalTokenUsage": total,
	}
	if err := w.patchStatus(ctx, run, patch); err != nil {
		w.log("rundrive.RunLLMStatusWriter: patch usage onto Run %s/%s: %v", run.Namespace, run.Name, err)
	}
}

// projectTraceID stamps the shim-reported root trace id onto
// Run.Status.TraceID — only-when-different, so the live status events
// replaying after a reconnect are harmless no-ops.
func (w *RunLLMStatusWriter) projectTraceID(ctx context.Context, ev wire.Event, traceID string) {
	run, err := runByUIDFrom(ctx, w.client, cleanRunID(ev.A2ATaskID))
	if err != nil {
		w.log("rundrive.RunLLMStatusWriter: resolve run %s for trace: %v", cleanRunID(ev.A2ATaskID), err)
		return
	}
	if run.Status.TraceID == traceID {
		return
	}
	if err := w.patchStatus(ctx, run, map[string]any{"traceID": traceID}); err != nil {
		w.log("rundrive.RunLLMStatusWriter: patch traceID onto Run %s/%s: %v", run.Namespace, run.Name, err)
	}
}

// patchStatus applies a merge patch confined to the named status fields —
// never a full-object write, so concurrent effects writers (sandboxRef,
// artifactRefs, the step projector) are untouched by construction.
func (w *RunLLMStatusWriter) patchStatus(ctx context.Context, run *api.Run, fields map[string]any) error {
	body, err := json.Marshal(map[string]any{"status": fields})
	if err != nil {
		return fmt.Errorf("marshal status patch: %w", err)
	}
	return w.client.Status().Patch(ctx, run, client.RawPatch(types.MergePatchType, body))
}

// llmInteractionID derives the idempotent interaction id from the event's
// identity: (a2a_task_id, seq) is the EventSink dedup key, so it embeds
// both. Capped well under the schema's MaxLength=128 (UUID + seq).
func llmInteractionID(ev wire.Event) string {
	id := cleanRunID(ev.A2ATaskID) + ":" + fmt.Sprint(ev.Seq)
	if len(id) > 128 {
		id = id[:128]
	}
	return id
}

// llmUsagePayload normalizes an EventUsage payload into the typed
// UsagePayload: the in-process transports carry the concrete type; the
// stdio/supervisor transports decode payloads as generic JSON.
func llmUsagePayload(payload any) (wire.UsagePayload, bool) {
	switch v := payload.(type) {
	case wire.UsagePayload:
		return v, true
	case *wire.UsagePayload:
		if v == nil {
			return wire.UsagePayload{}, false
		}
		return *v, true
	default:
		b, err := json.Marshal(payload)
		if err != nil {
			return wire.UsagePayload{}, false
		}
		var p wire.UsagePayload
		if err := json.Unmarshal(b, &p); err != nil || p.Model == "" {
			return wire.UsagePayload{}, false
		}
		return p, true
	}
}

// llmStatusPayload normalizes an EventStatus payload into the typed
// StatusPayload (same tolerance as llmUsagePayload).
func llmStatusPayload(payload any) (wire.StatusPayload, bool) {
	switch v := payload.(type) {
	case wire.StatusPayload:
		return v, true
	case *wire.StatusPayload:
		if v == nil {
			return wire.StatusPayload{}, false
		}
		return *v, true
	default:
		b, err := json.Marshal(payload)
		if err != nil {
			return wire.StatusPayload{}, false
		}
		var p wire.StatusPayload
		if err := json.Unmarshal(b, &p); err != nil || p.State == "" {
			return wire.StatusPayload{}, false
		}
		return p, true
	}
}
