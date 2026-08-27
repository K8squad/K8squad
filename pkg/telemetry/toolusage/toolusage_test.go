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
