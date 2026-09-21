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

package toolusage

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/K8squad/K8squad/pkg/a2a"
)

func boolPtr(b bool) *bool { return &b }

// newTestMapper wires a Mapper to an in-memory span recorder + a private
// registry, restoring the enable gate around each test.
func newTestMapper(t *testing.T) (*Mapper, *tracetest.SpanRecorder, *prometheus.Registry) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	reg := prometheus.NewRegistry()
	m := NewMapper(tp.Tracer("test"), reg)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return m, sr, reg
}

func attrMap(kv []attribute.KeyValue) map[string]string {
	m := map[string]string{}
	for _, a := range kv {
		m[string(a.Key)] = a.Value.String()
	}
	return m
}

func findSpan(t *testing.T, sr *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range sr.Ended() {
		if s.Name() == name {
			return s
		}
	}
	t.Fatalf("no span named %q; ended=%d", name, len(sr.Ended()))
	return nil
}

// TestToolEventSpan covers D1 AC1: an EventTool start+result fixture maps to
// one gen_ai.tool.call span carrying ALL required attributes — tool name,
// hashed args, outcome, run/agent correlation — with a settled status.
func TestToolEventSpan(t *testing.T) {
	m, sr, _ := newTestMapper(t)
	ctx := context.Background()
	labels := Labels{RunID: "run-42", Agent: "coder"}

	m.ToolEvent(ctx, labels, "task-1", a2a.ToolPayload{
		Name: "kubectl", Phase: "start", ArgsSHA256: "deadbeef", Skill: "restart-deploy",
	})
	m.ToolEvent(ctx, labels, "task-1", a2a.ToolPayload{
		Name: "kubectl", Phase: "result", OK: boolPtr(true),
	})

	span := findSpan(t, sr, SpanToolCall)
	attrs := attrMap(span.Attributes())
	for key, want := range map[string]string{
		"gen_ai.tool.name":           "kubectl",
		"gen_ai.tool.call.arguments": "deadbeef",
		"ksquad.skill.name":          "restart-deploy",
		"ksquad.run.id":              "run-42",
		"ksquad.agent.name":          "coder",
		"ksquad.outcome":             "success",
	} {
		if attrs[key] != want {
			t.Errorf("attr %s = %q, want %q", key, attrs[key], want)
		}
	}
	if span.Status().Code != codes.Ok {
		t.Errorf("status = %v, want Ok", span.Status().Code)
	}
}

// TestWSAIdentityAttrsRideSpans covers WS-A (ISI-4382): the widened Labels
// set — team, project, ticket (work_item.ref), sandbox pod — rides every
// run-trace span kind (run.start, gen_ai.tool.call, llm.call, skill.load),
// so a single trace filters by the full identity set, not just run+agent.
func TestWSAIdentityAttrsRideSpans(t *testing.T) {
	m, sr, _ := newTestMapper(t)
	ctx := context.Background()
	labels := Labels{
		RunID:       "run-42",
		Agent:       "coder",
		Team:        "alpha",
		Project:     "proj-x",
		WorkItemRef: "TKT-9",
		SandboxPod:  "ksquad-sbx-abc123",
	}

	runCtx, _ := m.RunStart(ctx, labels, "task-1")
	m.ToolEvent(runCtx, labels, "task-1", a2a.ToolPayload{Name: "kubectl", Phase: "start"})
	m.ToolEvent(runCtx, labels, "task-1", a2a.ToolPayload{Name: "kubectl", Phase: "result", OK: boolPtr(true)})
	m.UsageEvent(runCtx, labels, "task-1", a2a.UsagePayload{Model: "sonnet", Input: 3, Output: 5})
	m.SkillEvent(runCtx, labels, a2a.SkillLoadPayload{Name: "restart-deploy"})
	m.RunEnd(runCtx, "task-1", "completed", "")

	want := map[string]string{
		"ksquad.team.name":     "alpha",
		"ksquad.project.name":  "proj-x",
		"ksquad.work_item.ref": "TKT-9",
		"ksquad.sandbox.pod":   "ksquad-sbx-abc123",
	}
	for _, name := range []string{SpanRunStart, SpanToolCall, SpanLLMCall, SpanSkillLoad} {
		attrs := attrMap(findSpan(t, sr, name).Attributes())
		for key, val := range want {
			if attrs[key] != val {
				t.Errorf("span %s attr %s = %q, want %q", name, key, attrs[key], val)
			}
		}
	}
}

// TestModelTierOnRunStartOnly covers ISI-4430 S5: the resolved model-tier
// origin is stamped as ksquad.model.tier on the run.start root ONLY — it is a
// run-level dispatch fact, so it must not ride the per-step llm.call/tool.call
// spans (whose serving model is carried by gen_ai.response.model instead).
func TestModelTierOnRunStartOnly(t *testing.T) {
	m, sr, _ := newTestMapper(t)
	ctx := context.Background()
	labels := Labels{RunID: "run-tier", Agent: "coder", ModelTier: "role"}

	runCtx, _ := m.RunStart(ctx, labels, "task-1")
	m.UsageEvent(runCtx, labels, "task-1", a2a.UsagePayload{Model: "sonnet", Input: 3, Output: 5})
	m.ToolEvent(runCtx, labels, "task-1", a2a.ToolPayload{Name: "kubectl", Phase: "result", OK: boolPtr(true)})
	m.RunEnd(runCtx, "task-1", "completed", "")

	if got := attrMap(findSpan(t, sr, SpanRunStart).Attributes())["ksquad.model.tier"]; got != "role" {
		t.Errorf("run.start ksquad.model.tier = %q, want %q", got, "role")
	}
	for _, name := range []string{SpanLLMCall, SpanToolCall} {
		if _, ok := attrMap(findSpan(t, sr, name).Attributes())["ksquad.model.tier"]; ok {
			t.Errorf("span %s carries ksquad.model.tier; it must ride run.start only", name)
		}
	}
}

// TestModelTierEmptyOmitted covers the runtime-default path: an unresolved
// tier (empty ModelTier) must omit the attribute rather than emit "" (matching
// the Recommended semconv posture — never fabricate an origin).
func TestModelTierEmptyOmitted(t *testing.T) {
	m, sr, _ := newTestMapper(t)
	ctx := context.Background()
	runCtx, _ := m.RunStart(ctx, Labels{RunID: "run-nodefault", Agent: "coder"}, "task-1")
	m.RunEnd(runCtx, "task-1", "completed", "")

	if _, ok := attrMap(findSpan(t, sr, SpanRunStart).Attributes())["ksquad.model.tier"]; ok {
		t.Error("run.start emitted ksquad.model.tier for an empty tier; it must be omitted")
	}
}

// TestWSAIdentityNeverMetricLabels covers the ADR-0021 D1 cardinality note:
// the WS-A identity fields are span attributes ONLY — they must never leak
// into a metric label set (only agent does), or per-ticket/per-pod series
// would explode the counters.
func TestWSAIdentityNeverMetricLabels(t *testing.T) {
	m, _, reg := newTestMapper(t)
	labels := Labels{Agent: "coder", Team: "alpha", Project: "proj-x", WorkItemRef: "TKT-9", SandboxPod: "pod-1"}
	m.ToolEvent(context.Background(), labels, "task-1", a2a.ToolPayload{Name: "kubectl", Phase: "result", OK: boolPtr(true)})

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	forbidden := map[string]bool{"team": true, "project": true, "work_item_ref": true, "sandbox_pod": true}
	for _, mf := range mfs {
		for _, mtr := range mf.GetMetric() {
			for _, lp := range mtr.GetLabel() {
				if forbidden[lp.GetName()] {
					t.Errorf("metric %s carries forbidden WS-A label %q", mf.GetName(), lp.GetName())
				}
			}
		}
	}
}

// TestToolEventFailureOutcome asserts a failed result settles the span as an
// error with outcome=error.
func TestToolEventFailureOutcome(t *testing.T) {
	m, sr, _ := newTestMapper(t)
	labels := Labels{RunID: "r", Agent: "a"}
	m.ToolEvent(context.Background(), labels, "t", a2a.ToolPayload{Name: "shell", Phase: "start"})
	m.ToolEvent(context.Background(), labels, "t", a2a.ToolPayload{Name: "shell", Phase: "result", OK: boolPtr(false)})

	span := findSpan(t, sr, SpanToolCall)
	if span.Status().Code != codes.Error {
		t.Errorf("status = %v, want Error", span.Status().Code)
	}
	if got := attrMap(span.Attributes())["ksquad.outcome"]; got != "error" {
		t.Errorf("outcome = %q, want error", got)
	}
}

// TestToolEventUnknownPhaseAndOutcome covers D1 AC1's "unknown outcomes
// mapped safely": an unrecognized phase and an absent OK never panic and
// never drop the span.
func TestToolEventUnknownPhaseAndOutcome(t *testing.T) {
	m, sr, _ := newTestMapper(t)
	labels := Labels{}

	// Unknown phase → standalone span, outcome unknown.
	m.ToolEvent(context.Background(), labels, "t", a2a.ToolPayload{Name: "shell", Phase: "?????"})
	span := findSpan(t, sr, SpanToolCall)
	if got := attrMap(span.Attributes())["ksquad.outcome"]; got != "unknown" {
		t.Errorf("unknown phase outcome = %q, want unknown", got)
	}

	// Result with absent OK (nil) → outcome unknown, not guessed.
	m2, sr2, _ := newTestMapper(t)
	m2.ToolEvent(context.Background(), labels, "t", a2a.ToolPayload{Name: "shell", Phase: "start"})
	m2.ToolEvent(context.Background(), labels, "t", a2a.ToolPayload{Name: "shell", Phase: "result"})
	s2 := findSpan(t, sr2, SpanToolCall)
	if got := attrMap(s2.Attributes())["ksquad.outcome"]; got != "unknown" {
		t.Errorf("absent OK outcome = %q, want unknown", got)
	}
}

// TestMCPCallSpanAndHistogram covers the mcp.call mapping: a tool call with
// Server set emits an mcp.call span (server + tool attributes) and observes
// the duration histogram instead of the tool counter.
func TestMCPCallSpanAndHistogram(t *testing.T) {
	m, sr, reg := newTestMapper(t)
	labels := Labels{RunID: "r1", Agent: "agent-7"}

	m.ToolEvent(context.Background(), labels, "t", a2a.ToolPayload{
		Name: "create_pull_request", Phase: "start", Server: "github-mcp",
	})
	m.ToolEvent(context.Background(), labels, "t", a2a.ToolPayload{
		Name: "create_pull_request", Phase: "result", OK: boolPtr(true), Server: "github-mcp",
	})

	span := findSpan(t, sr, SpanMCPCall)
	attrs := attrMap(span.Attributes())
	if attrs["ksquad.mcp.server"] != "github-mcp" || attrs["gen_ai.tool.name"] != "create_pull_request" {
		t.Errorf("mcp.call attrs = %v", attrs)
	}

	mf, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, mf := range mf {
		if mf.GetName() == "ksquad_mcp_call_duration_seconds" {
			found = true
			if len(mf.GetMetric()) == 0 || mf.GetMetric()[0].GetHistogram().GetSampleCount() == 0 {
				t.Errorf("histogram has no samples")
			}
		}
		if mf.GetName() == "ksquad_tool_calls_total" {
			t.Errorf("MCP-served call must not hit the tool counter")
		}
	}
	if !found {
		t.Errorf("ksquad_mcp_call_duration_seconds not exported")
	}
}

// TestSkillLoadSpan covers D1 AC3: skill.load carries the skill name and the
// pinned source SHA.
func TestSkillLoadSpan(t *testing.T) {
	m, sr, reg := newTestMapper(t)
	m.SkillEvent(context.Background(), Labels{RunID: "r", Agent: "a"}, a2a.SkillLoadPayload{
		Name: "restart-deploy", SHA256: "abc123def", OK: boolPtr(true),
	})

	span := findSpan(t, sr, SpanSkillLoad)
	attrs := attrMap(span.Attributes())
	if attrs["ksquad.skill.name"] != "restart-deploy" {
		t.Errorf("skill name attr = %v", attrs)
	}
	if attrs["ksquad.skill.source.sha"] != "abc123def" {
		t.Errorf("source SHA attr missing: %v", attrs)
	}

	if got := counterValue(t, reg, "ksquad_skill_loads_total"); got != 1 {
		t.Errorf("skill loads counter = %v, want 1", got)
	}
}

// fixedNow forces the mapper stopwatch to report a deterministic elapsed so
// duration assertions are stable regardless of wall-clock (ISI-4385).
func fixedNow(seconds float64) func() func() float64 {
	return func() func() float64 { return func() float64 { return seconds } }
}

// TestToolCallSpanCarriesDuration (ISI-4385, WS-C): a settled tool call span
// carries the explicit ksquad.duration.ms attribute measured start→result,
// alongside its outcome.
func TestToolCallSpanCarriesDuration(t *testing.T) {
	m, sr, _ := newTestMapper(t)
	m.now = fixedNow(0.25) // 250ms
	labels := Labels{RunID: "r", Agent: "a"}

	m.ToolEvent(context.Background(), labels, "t", a2a.ToolPayload{Name: "kubectl", Phase: "start"})
	m.ToolEvent(context.Background(), labels, "t", a2a.ToolPayload{Name: "kubectl", Phase: "result", OK: boolPtr(true)})

	attrs := attrMap(findSpan(t, sr, SpanToolCall).Attributes())
	if got := attrs["ksquad.duration.ms"]; got != "250" {
		t.Errorf("tool.call duration.ms = %q, want 250", got)
	}
	if got := attrs["ksquad.outcome"]; got != "success" {
		t.Errorf("tool.call outcome = %q, want success", got)
	}
}

// TestMCPCallSpanCarriesDuration (ISI-4385): an mcp.call span carries the same
// explicit duration attribute (belt-and-braces with the histogram observation).
func TestMCPCallSpanCarriesDuration(t *testing.T) {
	m, sr, _ := newTestMapper(t)
	m.now = fixedNow(1.5) // 1500ms
	labels := Labels{RunID: "r", Agent: "a"}

	m.ToolEvent(context.Background(), labels, "t", a2a.ToolPayload{Name: "create_pr", Phase: "start", Server: "gh"})
	m.ToolEvent(context.Background(), labels, "t", a2a.ToolPayload{Name: "create_pr", Phase: "result", OK: boolPtr(true), Server: "gh"})

	attrs := attrMap(findSpan(t, sr, SpanMCPCall).Attributes())
	if got := attrs["ksquad.duration.ms"]; got != "1500" {
		t.Errorf("mcp.call duration.ms = %q, want 1500", got)
	}
}

// TestSkillLoadSpanCarriesDurationAndOutcome (ISI-4385): a skill.load span
// carries both ksquad.duration.ms (present, point-event instant) and outcome.
func TestSkillLoadSpanCarriesDurationAndOutcome(t *testing.T) {
	m, sr, _ := newTestMapper(t)
	m.now = fixedNow(0.03) // 30ms
	m.SkillEvent(context.Background(), Labels{RunID: "r", Agent: "a"}, a2a.SkillLoadPayload{
		Name: "restart-deploy", OK: boolPtr(true),
	})

	attrs := attrMap(findSpan(t, sr, SpanSkillLoad).Attributes())
	if _, ok := attrs["ksquad.duration.ms"]; !ok {
		t.Errorf("skill.load missing ksquad.duration.ms: %v", attrs)
	}
	if got := attrs["ksquad.outcome"]; got != "success" {
		t.Errorf("skill.load outcome = %q, want success", got)
	}
}

// TestOrphanResultSynthesizesSpan: a result with no start (at-least-once
// redelivery) still produces a complete span — never dropped.
func TestOrphanResultSynthesizesSpan(t *testing.T) {
	m, sr, _ := newTestMapper(t)
	m.ToolEvent(context.Background(), Labels{Agent: "a"}, "t", a2a.ToolPayload{
		Name: "git", Phase: "result", OK: boolPtr(true),
	})
	findSpan(t, sr, SpanToolCall) // presence asserted inside
}

// TestFinishTaskSweepsPending: a start with no result is swept when the task
// settles, ending with unknown outcome.
func TestFinishTaskSweepsPending(t *testing.T) {
	m, sr, _ := newTestMapper(t)
	m.ToolEvent(context.Background(), Labels{}, "t1", a2a.ToolPayload{Name: "docker", Phase: "start"})
	m.FinishTask(context.Background(), "t1")

	span := findSpan(t, sr, SpanToolCall)
	if got := attrMap(span.Attributes())["ksquad.outcome"]; got != "unknown" {
		t.Errorf("swept outcome = %q, want unknown", got)
	}
	if n := len(m.pending); n != 0 {
		t.Errorf("pending not swept: %d", n)
	}
}

// TestGateDisableCoversD2AC2: with the pipeline toggle off, no spans are
// emitted and no metric series exist.
func TestGateDisableCoversD2AC2(t *testing.T) {
	m, sr, reg := newTestMapper(t)
	t.Cleanup(func() { SetEnabled(true) })
	SetEnabled(false)

	m.ToolEvent(context.Background(), Labels{Agent: "a"}, "t", a2a.ToolPayload{Name: "x", Phase: "result", OK: boolPtr(true)})
	m.SkillEvent(context.Background(), Labels{Agent: "a"}, a2a.SkillLoadPayload{Name: "s"})

	if n := len(sr.Ended()); n != 0 {
		t.Errorf("disabled pipeline emitted %d spans", n)
	}
	mf, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range mf {
		t.Errorf("disabled pipeline exported metric %q", f.GetName())
	}
}

// TestRawArgsNeverOnWire is the D1 AC3 discipline check: the payload type
// has no raw-args field at all — only the hash travels.
func TestRawArgsNeverOnWire(t *testing.T) {
	p := a2a.ToolPayload{Name: "n", Phase: "result", ArgsSHA256: "hash"}
	if p.ArgsSHA256 == "" {
		t.Fatal("hash must travel")
	}
	// The attribute set built from a payload may not contain any value
	// longer than a sha256 hex string under the args keys: hashes only.
	m, sr, _ := newTestMapper(t)
	m.ToolEvent(context.Background(), Labels{}, "t", p)
	attrs := attrMap(findSpan(t, sr, SpanToolCall).Attributes())
	for _, k := range []string{"gen_ai.tool.call.arguments"} {
		if v := attrs[k]; v != "hash" {
			t.Errorf("%s = %q, want the hash only", k, v)
		}
	}
}

// TestNilTracerSafe: pre-Setup posture (no tracer) must not panic.
func TestNilTracerSafe(t *testing.T) {
	m := NewMapper(nil, nil)
	m.ToolEvent(context.Background(), Labels{}, "t", a2a.ToolPayload{Name: "x", Phase: "start"})
	m.ToolEvent(context.Background(), Labels{}, "t", a2a.ToolPayload{Name: "x", Phase: "result", OK: boolPtr(true)})
	m.SkillEvent(context.Background(), Labels{}, a2a.SkillLoadPayload{Name: "s"})
	m.FinishTask(context.Background(), "t")
}

func counterValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	mf, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range mf {
		if f.GetName() == name {
			var total float64
			for _, mm := range f.GetMetric() {
				total += mm.GetCounter().GetValue()
			}
			return total
		}
	}
	return 0
}

// TestUsageEventLLMCallSpan (ISI-4238): an EventUsage maps to one llm.call
// span carrying the GenAI model + token attributes and the provider cost,
// with the span duration set from the reported step duration; the counters
// increment per model/agent/direction.
func TestUsageEventLLMCallSpan(t *testing.T) {
	m, sr, reg := newTestMapper(t)
	ctx := context.Background()

	m.UsageEvent(ctx, Labels{RunID: "run-1", Agent: "dev"}, "run-1", a2a.UsagePayload{
		Model: "anthropic/claude-sonnet-4", Input: 1200, Output: 340, Reasoning: 50,
		CacheRead: 8000, CacheWrite: 400, CostUSD: 0.0057, DurationMS: 4200,
	})

	s := findSpan(t, sr, SpanLLMCall)
	attrs := attrMap(s.Attributes())
	if attrs["gen_ai.request.model"] != "anthropic/claude-sonnet-4" {
		t.Errorf("model attr = %q", attrs["gen_ai.request.model"])
	}
	if attrs["gen_ai.usage.input_tokens"] != "1200" {
		t.Errorf("input tokens attr = %q", attrs["gen_ai.usage.input_tokens"])
	}
	// ISI-4383: reasoning is now DISTINCT on the span — output_tokens is the
	// bare output count, reasoning rides gen_ai.usage.reasoning_tokens.
	if attrs["gen_ai.usage.output_tokens"] != "340" {
		t.Errorf("output tokens attr = %q, want 340 (reasoning no longer folded)", attrs["gen_ai.usage.output_tokens"])
	}
	if attrs["gen_ai.usage.reasoning_tokens"] != "50" {
		t.Errorf("reasoning tokens attr = %q, want 50", attrs["gen_ai.usage.reasoning_tokens"])
	}
	if attrs["gen_ai.operation.name"] != "chat" {
		t.Errorf("operation attr = %q, want chat", attrs["gen_ai.operation.name"])
	}
	if attrs["gen_ai.usage.cache_read_tokens"] != "8000" {
		t.Errorf("cache read attr = %q", attrs["gen_ai.usage.cache_read_tokens"])
	}
	if attrs["ksquad.llm.cost.usd"] != "0.0057" {
		t.Errorf("cost attr = %q", attrs["ksquad.llm.cost.usd"])
	}
	if attrs["ksquad.run.id"] != "run-1" {
		t.Errorf("run id attr = %q", attrs["ksquad.run.id"])
	}
	if d := s.EndTime().Sub(s.StartTime()); d < 4150*time.Millisecond || d > 4250*time.Millisecond {
		t.Errorf("llm.call duration = %v, want ~4.2s (truthful step duration)", d)
	}

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var calls, tokensIn, tokensOut float64
	for _, mf := range mfs {
		for _, metric := range mf.Metric {
			var direction string
			for _, l := range metric.Label {
				if l.GetName() == "direction" {
					direction = l.GetValue()
				}
			}
			switch {
			case mf.GetName() == "ksquad_llm_calls_total":
				calls = metric.Counter.GetValue()
			case mf.GetName() == "ksquad_llm_tokens_total" && direction == "input":
				tokensIn = metric.Counter.GetValue()
			case mf.GetName() == "ksquad_llm_tokens_total" && direction == "output":
				tokensOut = metric.Counter.GetValue()
			}
		}
	}
	if calls != 1 {
		t.Errorf("ksquad_llm_calls_total = %v, want 1", calls)
	}
	if tokensIn != 1200 {
		t.Errorf("input tokens = %v, want 1200", tokensIn)
	}
	if tokensOut != 390 {
		t.Errorf("output tokens = %v, want 390 (output+reasoning)", tokensOut)
	}
}

// TestUsageEventMeasuredDuration (ISI-4238): a runtime whose usage wire omits
// the step duration (opencode v1.18.27 — the "7µs llm.call" Henrik flagged)
// gets a truthful span duration measured by the run's step clock (now − the
// previous step boundary), and the span is marked ksquad.llm.duration_measured.
func TestUsageEventMeasuredDuration(t *testing.T) {
	m, sr, _ := newTestMapper(t)
	// Deterministic step clock: the run's stopwatch reads 3.0s elapsed at the
	// first step boundary, so a wire without a duration is measured as 3000ms.
	m.now = func() func() float64 { return func() float64 { return 3.0 } }
	ctx := context.Background()

	m.RunStart(ctx, Labels{RunID: "run-dur", Agent: "dev"}, "run-dur")
	m.UsageEvent(ctx, Labels{RunID: "run-dur", Agent: "dev"}, "run-dur", a2a.UsagePayload{
		Model: "ollama/qwen2.5-coder", Input: 100, Output: 50, DurationMS: 0,
	})

	s := findSpan(t, sr, SpanLLMCall)
	if d := s.EndTime().Sub(s.StartTime()); d < 2950*time.Millisecond || d > 3050*time.Millisecond {
		t.Errorf("llm.call measured duration = %v, want ~3s (wall-clock fallback, not ~7µs)", d)
	}
	if attrMap(s.Attributes())["ksquad.llm.duration_measured"] != "true" {
		t.Errorf("expected ksquad.llm.duration_measured=true for a wire without a step duration")
	}
}

// TestUsageEventWireDurationWins (ISI-4238): when the runtime DOES report a
// step duration it is honored verbatim and the measured-fallback marker is
// absent — the step clock never overrides a truthful wire duration.
func TestUsageEventWireDurationWins(t *testing.T) {
	m, sr, _ := newTestMapper(t)
	m.now = func() func() float64 { return func() float64 { return 99.0 } }
	ctx := context.Background()

	m.RunStart(ctx, Labels{RunID: "run-w", Agent: "dev"}, "run-w")
	m.UsageEvent(ctx, Labels{RunID: "run-w", Agent: "dev"}, "run-w", a2a.UsagePayload{
		Model: "anthropic/claude-sonnet-4", Input: 10, Output: 5, DurationMS: 1500,
	})

	s := findSpan(t, sr, SpanLLMCall)
	if d := s.EndTime().Sub(s.StartTime()); d < 1450*time.Millisecond || d > 1550*time.Millisecond {
		t.Errorf("llm.call duration = %v, want ~1.5s (wire-reported, not the 99s clock)", d)
	}
	if _, ok := attrMap(s.Attributes())["ksquad.llm.duration_measured"]; ok {
		t.Errorf("wire-reported duration must not be marked measured")
	}
}

// TestUsageEventGenAISemconv (ISI-4383, ADR-0021 D2): the llm.call span
// carries the full gen-AI semconv surface — system (provider), served
// response model, finish reason and response id — when the runtime reports
// them, alongside the request model already covered above.
func TestUsageEventGenAISemconv(t *testing.T) {
	m, sr, _ := newTestMapper(t)
	m.UsageEvent(context.Background(), Labels{RunID: "run-9", Agent: "dev"}, "run-9", a2a.UsagePayload{
		Model:         "anthropic/claude-opus-4",
		Provider:      "anthropic",
		ResponseModel: "anthropic/claude-opus-4-20260101",
		FinishReason:  "stop",
		ResponseID:    "resp_abc123",
		Input:         100, Output: 40,
	})

	s := findSpan(t, sr, SpanLLMCall)
	attrs := attrMap(s.Attributes())
	if attrs["gen_ai.system"] != "anthropic" {
		t.Errorf("gen_ai.system = %q, want anthropic", attrs["gen_ai.system"])
	}
	if attrs["gen_ai.response.model"] != "anthropic/claude-opus-4-20260101" {
		t.Errorf("gen_ai.response.model = %q", attrs["gen_ai.response.model"])
	}
	if attrs["gen_ai.response.id"] != "resp_abc123" {
		t.Errorf("gen_ai.response.id = %q", attrs["gen_ai.response.id"])
	}
	// finish_reasons is a semconv string array — assert on the typed slice.
	var reasons []string
	for _, kv := range s.Attributes() {
		if string(kv.Key) == "gen_ai.response.finish_reasons" {
			reasons = kv.Value.AsStringSlice()
		}
	}
	if len(reasons) != 1 || reasons[0] != "stop" {
		t.Errorf("gen_ai.response.finish_reasons = %v, want [stop]", reasons)
	}
}

// TestUsageEventFallbackMarker (ISI-4383, ADR-0021 D2): a step served by the
// backup/fallback model is visibly flagged with ksquad.llm.fallback=true, and
// requested-vs-served model is legible as request.model vs response.model.
func TestUsageEventFallbackMarker(t *testing.T) {
	m, sr, _ := newTestMapper(t)

	// Primary-served step: no fallback marker at all (absent, not "false").
	m.UsageEvent(context.Background(), Labels{Agent: "dev"}, "r", a2a.UsagePayload{
		Model: "primary/model-a", ResponseModel: "primary/model-a", Input: 1, Output: 1,
	})
	primary := findSpan(t, sr, SpanLLMCall)
	if _, ok := attrMap(primary.Attributes())["ksquad.llm.fallback"]; ok {
		t.Error("primary-served call must not carry ksquad.llm.fallback")
	}

	// Fallback-served step: requested != served, marker present + true.
	m.UsageEvent(context.Background(), Labels{Agent: "dev"}, "r2", a2a.UsagePayload{
		Model: "primary/model-a", ResponseModel: "backup/model-b", Fallback: true, Input: 1, Output: 1,
	})
	var fb sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == SpanLLMCall {
			fb = s // last llm.call is the fallback one
		}
	}
	fattrs := attrMap(fb.Attributes())
	if fattrs["ksquad.llm.fallback"] != "true" {
		t.Errorf("ksquad.llm.fallback = %q, want true", fattrs["ksquad.llm.fallback"])
	}
	if fattrs["gen_ai.request.model"] != "primary/model-a" || fattrs["gen_ai.response.model"] != "backup/model-b" {
		t.Errorf("requested/served = %q/%q, want primary/model-a / backup/model-b",
			fattrs["gen_ai.request.model"], fattrs["gen_ai.response.model"])
	}
}

// TestUsageEventContentGate (ISI-4383, ADR-0021 D3): prompt/response bodies
// never ride the span by default (PII posture), and appear as gated span
// EVENTS — never attributes — only when the content gate is explicitly on.
func TestUsageEventContentGate(t *testing.T) {
	p := a2a.UsagePayload{Model: "m", Input: 1, Output: 1, Prompt: "secret prompt", Response: "secret reply"}

	// Default: gate off → no content events, no content attributes.
	m, sr, _ := newTestMapper(t)
	m.UsageEvent(context.Background(), Labels{}, "r", p)
	off := findSpan(t, sr, SpanLLMCall)
	if len(off.Events()) != 0 {
		t.Errorf("gate off: %d span events, want 0 (content stays off by default)", len(off.Events()))
	}

	// Opt in: gate on → prompt/response ride as span events (dev/non-prod).
	SetContentTracing(true)
	t.Cleanup(func() { SetContentTracing(false) })
	m2, sr2, _ := newTestMapper(t)
	m2.UsageEvent(context.Background(), Labels{}, "r", p)
	on := findSpan(t, sr2, SpanLLMCall)
	if len(on.Events()) != 2 {
		t.Fatalf("gate on: %d span events, want 2 (prompt + completion)", len(on.Events()))
	}
	// Content must never leak onto queryable attributes even when captured.
	for _, kv := range on.Attributes() {
		if kv.Value.AsString() == "secret prompt" || kv.Value.AsString() == "secret reply" {
			t.Errorf("content leaked onto attribute %q", kv.Key)
		}
	}
}

// TestUsageEventModellessDropped (ISI-4238): usage without a model id is
// unattributable — never fabricated into an "unknown" bucket.
func TestUsageEventModellessDropped(t *testing.T) {
	m, sr, _ := newTestMapper(t)
	m.UsageEvent(context.Background(), Labels{}, "t", a2a.UsagePayload{Input: 5})
	if got := len(sr.Ended()); got != 0 {
		t.Errorf("spans = %d, want 0", got)
	}
}

// TestRunTraceLifecycle (ISI-4238): RunStart opens a run.start root whose
// trace id is the run's correlation key; spans started with the returned
// context join that trace; RunEnd closes the root with the terminal state
// and emits a run.end marker child of the same trace.
func TestRunTraceLifecycle(t *testing.T) {
	m, sr, _ := newTestMapper(t)
	base := context.Background()

	ctx, root := m.RunStart(base, Labels{RunID: "r", Agent: "a"}, "r")
	if !root.SpanContext().HasTraceID() {
		t.Fatal("RunStart returned no trace id")
	}
	rootID := root.SpanContext().TraceID()

	// A child llm.call started with the returned ctx must join the trace.
	m.UsageEvent(ctx, Labels{RunID: "r", Agent: "a"}, "r", a2a.UsagePayload{Model: "m", Input: 1})
	m.RunEnd(ctx, "r", "completed", "")

	end := findSpan(t, sr, SpanRunEnd)
	if end.SpanContext().TraceID() != rootID {
		t.Errorf("run.end trace %v != run.start trace %v", end.SpanContext().TraceID(), rootID)
	}
	attrs := attrMap(end.Attributes())
	if attrs["ksquad.run.state"] != "completed" || attrs["ksquad.outcome"] != "success" {
		t.Errorf("run.end attrs = %v", attrs)
	}

	start := findSpan(t, sr, SpanRunStart)
	if start.SpanContext().TraceID() != rootID {
		t.Errorf("run.start root trace mismatch")
	}
	if !start.EndTime().IsZero() && start.EndTime().Before(start.StartTime()) {
		t.Errorf("run.start root closed with inverted timestamps")
	}
	// The root must be CLOSED by RunEnd with the terminal state.
	sattrs := attrMap(start.Attributes())
	if sattrs["ksquad.run.state"] != "completed" {
		t.Errorf("run.start root terminal state attr = %v", sattrs)
	}

	// The llm.call child joined the same trace.
	llm := findSpan(t, sr, SpanLLMCall)
	if llm.SpanContext().TraceID() != rootID {
		t.Errorf("llm.call trace %v != run trace %v", llm.SpanContext().TraceID(), rootID)
	}
}

// TestRunEndWithoutStartEmitsMarker (ISI-4238): a RunEnd with no matching
// RunStart still emits the run.end marker (core-side sink posture).
func TestRunEndWithoutStartEmitsMarker(t *testing.T) {
	m, sr, _ := newTestMapper(t)
	m.RunEnd(context.Background(), "solo", "failed", "boom")
	end := findSpan(t, sr, SpanRunEnd)
	attrs := attrMap(end.Attributes())
	if attrs["ksquad.run.state"] != "failed" || attrs["ksquad.outcome"] != "error" {
		t.Errorf("attrs = %v", attrs)
	}
}

// TestFinishTaskSweepsOrphanRunRoot (ISI-4238): a run root left open by a
// crashed emitter is swept by FinishTask with unknown outcome, not leaked.
func TestFinishTaskSweepsOrphanRunRoot(t *testing.T) {
	m, sr, _ := newTestMapper(t)
	_, root := m.RunStart(context.Background(), Labels{Agent: "a"}, "orphan-run")
	m.FinishTask(context.Background(), "orphan-run")
	start := findSpan(t, sr, SpanRunStart)
	attrs := attrMap(start.Attributes())
	if attrs["ksquad.outcome"] != "unknown" {
		t.Errorf("swept root outcome = %v, want unknown", attrs["ksquad.outcome"])
	}
	_ = root
}

// TestCategorizeTool (ISI-4540): the ksquad.tool.type attribute buckets tool
// events into their real category so traces show git/mcp/docker/etc. instead
// of everything reading as an uncategorized bare name.
func TestCategorizeTool(t *testing.T) {
	tests := []struct {
		name   string
		tool   string
		server string
		want   string
	}{
		{"bash prefix", "bash.execute", "", "bash"},
		{"sh alias", "sh.run", "", "bash"},
		{"git prefix", "git.clone", "", "git"},
		{"docker prefix", "docker.build", "", "docker"},
		{"kubectl prefix", "kubectl.apply", "", "kubectl"},
		{"helm prefix", "helm.install", "", "helm"},
		{"npm bucketed as node", "npm.install", "", "node"},
		{"node prefix", "node.run", "", "node"},
		{"pip bucketed as python", "pip.install", "", "python"},
		{"python prefix", "python.script", "", "python"},
		{"mcp prefix", "mcp.server.tool", "", "mcp"},
		{"dotted server.tool form", "github.create_issue", "", "mcp"},
		{"explicit MCP server wins", "whatever", "github", "mcp"},
		{"path-like name is system", "system/exec", "", "system"},
		{"plain unknown is system", "system.execute", "", "system"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := categorizeTool(tt.tool, tt.server); got != tt.want {
				t.Errorf("categorizeTool(%q, %q) = %q, want %q", tt.tool, tt.server, got, tt.want)
			}
		})
	}
}

// TestToolEventCarriesToolType (ISI-4540): the mapped tool.call span carries
// the ksquad.tool.type attribute.
func TestToolEventCarriesToolType(t *testing.T) {
	m, sr, _ := newTestMapper(t)
	ctx := context.Background()
	m.ToolEvent(ctx, Labels{Agent: "a"}, "task-1", a2a.ToolPayload{Name: "git.clone", Phase: "start"})
	m.ToolEvent(ctx, Labels{Agent: "a"}, "task-1", a2a.ToolPayload{Name: "git.clone", Phase: "result", OK: boolPtr(true)})
	span := findSpan(t, sr, SpanToolCall)
	if got := attrMap(span.Attributes())["ksquad.tool.type"]; got != "git" {
		t.Errorf("ksquad.tool.type = %v, want git", got)
	}
}

// TestToolEventShellCommandEnrichment (ISI-4720): a bash-family tool call the
// shim resolved to a real executable (Command="git") is categorized by what it
// RAN — ksquad.tool.type=git and ksquad.tool.command=git — while
// gen_ai.tool.name stays the tool the runtime exposed ("bash"). Without this a
// git/kubectl/npm call is an invisible, uncategorized "bash" span.
func TestToolEventShellCommandEnrichment(t *testing.T) {
	m, sr, _ := newTestMapper(t)
	ctx := context.Background()
	p := a2a.ToolPayload{Name: "bash", Command: "git"}
	p.Phase = "start"
	m.ToolEvent(ctx, Labels{Agent: "a"}, "task-1", p)
	p.Phase = "result"
	p.OK = boolPtr(true)
	m.ToolEvent(ctx, Labels{Agent: "a"}, "task-1", p)

	span := findSpan(t, sr, SpanToolCall)
	attrs := attrMap(span.Attributes())
	if got := attrs["gen_ai.tool.name"]; got != "bash" {
		t.Errorf("gen_ai.tool.name = %v, want bash (the exposed tool)", got)
	}
	if got := attrs["ksquad.tool.type"]; got != "git" {
		t.Errorf("ksquad.tool.type = %v, want git (categorized by what it ran)", got)
	}
	if got := attrs["ksquad.tool.command"]; got != "git" {
		t.Errorf("ksquad.tool.command = %v, want git", got)
	}
}

// TestToolEventMCPIgnoresCommand (ISI-4720): Command is only trusted for local
// tool calls — an MCP-served call keeps its mcp category and never grows a
// ksquad.tool.command attribute even if one leaked onto the payload.
func TestToolEventMCPIgnoresCommand(t *testing.T) {
	m, sr, _ := newTestMapper(t)
	ctx := context.Background()
	p := a2a.ToolPayload{Name: "create_issue", Server: "github", Command: "git", Phase: "result", OK: boolPtr(true)}
	m.ToolEvent(ctx, Labels{Agent: "a"}, "task-1", p)

	span := findSpan(t, sr, SpanMCPCall)
	attrs := attrMap(span.Attributes())
	if got := attrs["ksquad.tool.type"]; got != "mcp" {
		t.Errorf("ksquad.tool.type = %v, want mcp", got)
	}
	if _, ok := attrs["ksquad.tool.command"]; ok {
		t.Errorf("ksquad.tool.command must be absent on an MCP call, got %v", attrs["ksquad.tool.command"])
	}
}
