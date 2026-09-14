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

// The ISI-4387 (ADR-0021 WS-F) semconv conformance suite. It drives the REAL
// pkg/telemetry/toolusage.Mapper — the single mapper the in-pod shim and the
// operator sink both feed — through a fully-populated run trace and asserts the
// emitted spans satisfy the registry contract:
//
//   - every Stable Required attribute is present on its span (the contract
//     WS-A/B land against — flip Planned→Stable in registry.go and this suite
//     starts enforcing it automatically);
//   - every Stable Conditional attribute is present when its source field is
//     set (fixtures populate all fields);
//   - no `ksquad.*` / `gen_ai.*` key escapes the emitter that the registry
//     does not document (the regression guard the acceptance criteria call for).
package ksqsemconv_test

import (
	"context"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/K8squad/K8squad/pkg/a2a"
	ksqsemconv "github.com/K8squad/K8squad/pkg/telemetry/semconv"
	"github.com/K8squad/K8squad/pkg/telemetry/toolusage"
)

// emitFullRunTrace drives the mapper through one fully-populated run trace so
// every conditional attribute is exercised, and returns the ended spans keyed
// by span name.
func emitFullRunTrace(t *testing.T) map[string]sdktrace.ReadOnlySpan {
	t.Helper()

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	m := toolusage.NewMapper(tp.Tracer("conformance"), nil)
	labels := toolusage.Labels{RunID: "run-conformance", Agent: "coder"}
	const task = "task-1"

	ctx := context.Background()
	ctx, _ = m.RunStart(ctx, labels, task)

	// llm.call — every reported field populated so all Stable conditionals fire.
	m.UsageEvent(ctx, labels, task, a2a.UsagePayload{
		Model:      "claude-opus-4-8",
		Input:      1200,
		Output:     340,
		CacheRead:  800,
		CacheWrite: 64,
		Reasoning:  50,
		CostUSD:    0.0123,
		DurationMS: 900,
	})

	// gen_ai.tool.call — skill-scoped, hashed args, settled result.
	m.ToolEvent(ctx, labels, task, a2a.ToolPayload{
		Name: "kubectl", Phase: "start", ArgsSHA256: "deadbeef", Skill: "restart-deploy",
	})
	m.ToolEvent(ctx, labels, task, a2a.ToolPayload{
		Name: "kubectl", Phase: "result", ArgsSHA256: "deadbeef", Skill: "restart-deploy", OK: boolPtr(true),
	})

	// mcp.call — server-served, settled result.
	m.ToolEvent(ctx, labels, task, a2a.ToolPayload{
		Name: "search", Phase: "start", Server: "memory-mcp", ArgsSHA256: "cafebabe",
	})
	m.ToolEvent(ctx, labels, task, a2a.ToolPayload{
		Name: "search", Phase: "result", Server: "memory-mcp", ArgsSHA256: "cafebabe", OK: boolPtr(true),
	})

	// skill.load — with a pinned source SHA.
	m.SkillEvent(ctx, labels, a2a.SkillLoadPayload{
		Name: "restart-deploy", SHA256: "abc123", OK: boolPtr(true),
	})

	m.RunEnd(ctx, task, "completed", "")

	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range sr.Ended() {
		byName[s.Name()] = s
	}
	return byName
}

func boolPtr(b bool) *bool { return &b }

func keysOf(s sdktrace.ReadOnlySpan) map[string]bool {
	m := map[string]bool{}
	for _, kv := range s.Attributes() {
		m[string(kv.Key)] = true
	}
	return m
}

// TestConformance_StableAttributesPresent asserts every Stable attribute the
// registry marks Required (and, given fully-populated fixtures, Conditional) is
// emitted on its span. This is the contract WS-A/B extend: flipping a Planned
// attribute to Stable in registry.go makes this test require it.
func TestConformance_StableAttributesPresent(t *testing.T) {
	spans := emitFullRunTrace(t)

	for _, sc := range ksqsemconv.SpanConventions {
		span, ok := spans[sc.Name]
		if !ok {
			t.Errorf("span %q was never emitted by the mapper", sc.Name)
			continue
		}
		got := keysOf(span)

		for _, key := range ksqsemconv.RequiredStable(sc.Name) {
			if !got[key] {
				t.Errorf("span %q missing REQUIRED stable attribute %q (contract: registry.go)", sc.Name, key)
			}
		}
		for _, key := range ksqsemconv.ConditionalStable(sc.Name) {
			if !got[key] {
				t.Errorf("span %q missing CONDITIONAL stable attribute %q despite a fully-populated fixture", sc.Name, key)
			}
		}
	}
}

// TestConformance_NoUndocumentedKeys is the regression guard: any ksquad.* /
// gen_ai.* attribute the emitter produces MUST be documented in the registry.
// A new attribute added by WS-A/B without a registry entry fails here, forcing
// the schema doc to stay complete.
func TestConformance_NoUndocumentedKeys(t *testing.T) {
	spans := emitFullRunTrace(t)
	documented := ksqsemconv.DocumentedSpanKeys()

	for name, span := range spans {
		for _, kv := range span.Attributes() {
			key := string(kv.Key)
			if !isDomainKey(key) {
				continue // SDK/resource keys are not in scope for this contract.
			}
			if !documented[key] {
				t.Errorf("span %q emits UNDOCUMENTED attribute %q — add it to pkg/telemetry/semconv/registry.go", name, key)
			}
		}
	}
}

// TestConformance_SpanNamesAgree asserts the registry's span-name constants
// match the toolusage emitter's, so the two vocabularies cannot drift.
func TestConformance_SpanNamesAgree(t *testing.T) {
	pairs := []struct {
		registry, emitter string
	}{
		{ksqsemconv.SpanRunStart, toolusage.SpanRunStart},
		{ksqsemconv.SpanRunEnd, toolusage.SpanRunEnd},
		{ksqsemconv.SpanLLMCall, toolusage.SpanLLMCall},
		{ksqsemconv.SpanToolCall, toolusage.SpanToolCall},
		{ksqsemconv.SpanMCPCall, toolusage.SpanMCPCall},
		{ksqsemconv.SpanSkillLoad, toolusage.SpanSkillLoad},
	}
	for _, p := range pairs {
		if p.registry != p.emitter {
			t.Errorf("span-name drift: registry %q != emitter %q", p.registry, p.emitter)
		}
	}
}

// TestConformance_LifecycleEventsWellFormed asserts the WS-D event registry is
// self-consistent against the outbox entity vocabulary — the contract WS-D
// lands against. (The events do not exist on the bus yet; this validates the
// spec, not a live emission.)
func TestConformance_LifecycleEventsWellFormed(t *testing.T) {
	// Mirrors pkg/events.Entities (kept as a literal to avoid a heavy import
	// into the semconv contract package).
	validEntities := map[string]bool{"run": true, "work_item": true, "artifact": true, "memory": true, "scm": true}

	seen := map[string]bool{}
	for _, ec := range ksqsemconv.EventConventions {
		if !validEntities[ec.Entity] {
			t.Errorf("event %q uses entity %q not in the outbox vocabulary", ec.EventType, ec.Entity)
		}
		if seen[ec.EventType] {
			t.Errorf("duplicate event type %q in the registry", ec.EventType)
		}
		seen[ec.EventType] = true

		hasTrace := false
		for _, f := range ec.PayloadFields {
			if f.Key == "trace_id" {
				hasTrace = true
			}
		}
		if !hasTrace {
			t.Errorf("event %q payload is missing trace_id — every lifecycle event must join the trace spine (D4)", ec.EventType)
		}
	}

	// The five ADR-0021 §3 D4 lifecycle events must all be specified.
	for _, want := range []string{"work_item.assigned", "run.scheduled", "run.sandbox_bound", "run.started", "run.ended"} {
		if !seen[want] {
			t.Errorf("ADR-0021 lifecycle event %q is not in the registry", want)
		}
	}
}

// isDomainKey reports whether an attribute key is in this contract's scope
// (the K8squad domain namespace or the gen-AI semconv namespace).
func isDomainKey(key string) bool {
	return strings.HasPrefix(key, "ksquad.") || strings.HasPrefix(key, "gen_ai.")
}
