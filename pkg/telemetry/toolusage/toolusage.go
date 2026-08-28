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

// Package toolusage is the Epic D instrumentation core (plan §2.4, D1/D2):
// it maps A2A tool/skill activity events (pkg/a2a EventTool / EventSkillLoad)
// onto OpenTelemetry GenAI-semconv spans and ksquad_* metrics.
//
// Spans (names per OTel GenAI agent conventions + plan §2.4):
//
//	gen_ai.tool.call   — a local/CLI tool call: gen_ai.tool.name, hashed
//	                     args (gen_ai.tool.call.arguments carries the hex
//	                     sha256 — raw arguments NEVER travel), outcome, duration
//	skill.load         — a skill entering the runtime session: skill name +
//	                     pinned source SHA
//	mcp.call           — a tool call served by an MCPServer: mcp server +
//	                     gen_ai.tool.name, outcome, duration
//
// Metrics (13.x bounded-cardinality style; run.id is an attribute, never a
// metric label):
//
//	ksquad_tool_calls_total{tool,agent,skill}      counter
//	ksquad_skill_loads_total{skill,agent}          counter
//	ksquad_mcp_call_duration_seconds{server,tool}  histogram
//
// Every emission is gated by a process-wide enable flag wired to the
// OTelConfig CRD tool-usage pipeline toggle (D2): flag off → no spans, no
// metric samples, nothing exported. The flag defaults to enabled; absent
// configuration is backward-compatible (plan §5.4 opt-out).
//
// The Mapper is deliberately emitter-agnostic: the in-pod shim hook and the
// core-side a2a EventSink both feed it, so the mapping exists exactly once
// (story D1 "map existing A2A EventTool events").
package toolusage

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/K8squad/K8squad/pkg/a2a"
)

// Span names (plan §2.4). gen_ai.tool.call follows the GenAI semconv agent
// conventions; skill.load and mcp.call are the plan's names for the two
// adjacent activities the conventions do not yet standardize.
const (
	SpanToolCall  = "gen_ai.tool.call"
	SpanSkillLoad = "skill.load"
	SpanMCPCall   = "mcp.call"
)

// K8squad-namespaced attribute keys: run/agent correlation (spans are
// "queryable per run/agent", plan §5.4) and the plan's hashed-args / source
// SHA attributes that GenAI semconv does not define.
const (
	attrRunID     = attribute.Key("ksquad.run.id")
	attrAgentName = attribute.Key("ksquad.agent.name")
	attrSkillName = attribute.Key("ksquad.skill.name")
	attrSkillSHA  = attribute.Key("ksquad.skill.source.sha")
	attrMCPServer = attribute.Key("ksquad.mcp.server")
	// attrOutcome records the mapped outcome ("success" | "error" |
	// "unknown") — D1 AC: unknown outcomes map safely, never panic, never
	// drop the span.
	attrOutcome = attribute.Key("ksquad.outcome")

	outcomeSuccess = "success"
	outcomeError   = "error"
	outcomeUnknown = "unknown"
)

// enabled is the process-wide tool-usage pipeline gate (D2). Zero value
// (false) would disable; NewMapper flips it on at construction so the
// default posture is emit. The OTelConfig watcher (operator wiring) may
// clear it at runtime; metrics and spans stop mid-process, in both the shim
// hook and the core sink, with no restart.
var enabled atomic.Bool

// SetEnabled flips the tool-usage pipeline gate. It is safe for concurrent
// use and idempotent.
func SetEnabled(v bool) { enabled.Store(v) }

// Enabled reports the current gate value (test seam + wiring assertions).
func Enabled() bool { return enabled.Load() }

func init() { enabled.Store(true) }

// Labels identify the emitting Run/Agent. They ride every span (attributes)
// so a backend can filter per run / per agent, but only agent flows into
// metric label sets (run.id would explode counter cardinality; it stays an
// attribute, same discipline as 13.6).
type Labels struct {
	RunID string
	Agent string
}

func (l Labels) spanAttrs() []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 2)
	if l.RunID != "" {
		attrs = append(attrs, attrRunID.String(l.RunID))
	}
	if l.Agent != "" {
		attrs = append(attrs, attrAgentName.String(l.Agent))
	}
	return attrs
}

// Instruments is the metric set (D2). Exposed for registry wiring; the
// Mapper holds one set for its lifetime.
type Instruments struct {
	ToolCalls  *prometheus.CounterVec
	SkillLoads *prometheus.CounterVec
	MCPDur     *prometheus.HistogramVec
	// PipelineUp is the D2 pipeline-liveness marker: a childless CounterVec
	// never appears in a Prometheus exposition, so an operator that registered
	// the ksquad_* set but has not yet mapped a single event would be
	// indistinguishable from one whose instrumentation is dead. The marker
	// reports 1 while the pipeline gate is on and exports NOTHING while it
	// is off (the D2 gate contract: no samples at all), so its presence in
	// an exposition proves the pipeline is wired, gate on, and scrapeable;
	// its absence lets the D3 read model render an explicit degraded state
	// instead of a quiet "no activity yet" (review ISI-3348 finding 1).
	PipelineUp prometheus.Collector
}

// pipelineUpDesc is the marker's descriptor (shared by Describe/Collect).
var pipelineUpDesc = prometheus.NewDesc(
	"ksquad_tool_usage_pipeline_up",
	"1 while this process carries the Epic D tool-usage pipeline with its gate ON and exports it on its metrics surface; absence means the pipeline is not reporting (D3 degraded-state signal).",
	nil, nil,
)

// pipelineUp is the gate-aware marker collector: Collect emits the constant 1
// only while the process-wide gate is enabled — disabled, it emits no sample
// at all, keeping the D2 gate contract (nothing exported).
type pipelineUp struct{}

func (pipelineUp) Describe(ch chan<- *prometheus.Desc) { ch <- pipelineUpDesc }

func (pipelineUp) Collect(ch chan<- prometheus.Metric) {
	if !enabled.Load() {
		return
	}
	ch <- prometheus.MustNewConstMetric(pipelineUpDesc, prometheus.GaugeValue, 1)
}

// newInstruments builds the metric set. reg nil → instruments exist but are
// not registered (pure-mapper tests).
func newInstruments(reg prometheus.Registerer) *Instruments {
	ins := &Instruments{
		ToolCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ksquad_tool_calls_total",
			Help: "Tool calls mapped from A2A EventTool events (Epic D, plan §2.4). Local/CLI tool calls only — MCP-served calls ride ksquad_mcp_call_duration_seconds instead. Bounded labels {tool,agent,skill}; run.id is a span attribute, never a label.",
		}, []string{"tool", "agent", "skill"}),
		SkillLoads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ksquad_skill_loads_total",
			Help: "Skill loads mapped from A2A EventSkillLoad events (Epic D, plan §2.4).",
		}, []string{"skill", "agent"}),
		MCPDur: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "ksquad_mcp_call_duration_seconds",
			Help:    "Duration of tool calls served by MCPServers (Epic D, plan §2.4).",
			Buckets: prometheus.ExponentialBuckets(0.005, 2, 14), // 5ms .. ~80s
		}, []string{"server", "tool"}),
		PipelineUp: pipelineUp{},
	}
	if reg != nil {
		reg.MustRegister(ins.ToolCalls, ins.SkillLoads, ins.MCPDur, ins.PipelineUp)
	}
	return ins
}

// Mapper maps A2A activity onto spans + metrics. It pairs EventTool
// start/result phases into one span per call, keyed by (task, tool name):
// the start opens the span, the result settles outcome and duration. An
// orphan result (no start seen — at-least-once redelivery, mid-stream
// attach) synthesizes a complete zero-history span so no call is lost; an
// orphan start is swept when the task settles (FinishTask).
//
// A Mapper is safe for concurrent use.
type Mapper struct {
	tracer trace.Tracer
	now    func() func() float64
	ins    *Instruments

	mu      sync.Mutex
	pending map[string]pendingSpan // "task\x00tool" → open span
}

// pendingSpan is one open tool/MCP call: its span and the wall-clock start
// so the result phase can observe a truthful duration.
type pendingSpan struct {
	span  trace.Span
	start func() float64
}

// NewMapper builds a Mapper over tracer. reg non-nil registers the metric
// set on it (pass the controller-runtime metrics registry in the operator,
// or a dedicated registry in tests); nil keeps the instruments unregistered.
// A nil tracer degrades to no-op spans (the telemetry spine's pre-Setup
// posture: safe before Setup, exporting after).
func NewMapper(tracer trace.Tracer, reg prometheus.Registerer) *Mapper {
	return &Mapper{
		tracer:  tracer,
		now:     stopwatch,
		ins:     newInstruments(reg),
		pending: map[string]pendingSpan{},
	}
}

// stopwatch returns a duration function in seconds (test-seam replaceable).
func stopwatch() func() float64 {
	t0 := time.Now()
	return func() float64 { return time.Since(t0).Seconds() }
}

// Instruments exposes the metric set (handler/collector assertions).
func (m *Mapper) Instruments() *Instruments { return m.ins }

func spanKey(taskID, tool string) string { return taskID + "\x00" + tool }

// ToolEvent maps one EventTool payload for taskID under the given labels.
// Phase "start" opens the span; phase "result" settles it. Any other phase
// is mapped safely as a standalone unknown-outcome span (D1 AC: unknown
// outcomes never dropped, never panic).
func (m *Mapper) ToolEvent(ctx context.Context, labels Labels, taskID string, p a2a.ToolPayload) {
	if !enabled.Load() || p.Name == "" {
		return
	}
	isMCP := p.Server != ""
	name := SpanToolCall
	if isMCP {
		name = SpanMCPCall
	}

	attrs := labels.spanAttrs()
	attrs = append(attrs, semconv.GenAIToolName(p.Name))
	if p.ArgsSHA256 != "" {
		// The hash IS the argument surface — raw args never reach this
		// package (emitters hash before the event leaves the process). The
		// semconv arguments attribute carries the hex sha256.
		attrs = append(attrs, semconv.GenAIToolCallArgumentsKey.String(p.ArgsSHA256))
	}
	if p.Skill != "" {
		attrs = append(attrs, attrSkillName.String(p.Skill))
	}
	if isMCP {
		attrs = append(attrs, attrMCPServer.String(p.Server))
	}

	switch p.Phase {
	case "start":
		_, span := m.start(ctx, name, attrs)
		elapsed := m.now()
		m.mu.Lock()
		m.pending[spanKey(taskID, p.Name)] = pendingSpan{span: span, start: elapsed}
		m.mu.Unlock()
	case "result":
		outcome := mapOutcome(p.OK)
		attrs = append(attrs, attrOutcome.String(outcome))
		m.settle(ctx, taskID, p.Name, name, attrs, outcome, func(d float64) {
			if isMCP {
				m.ins.MCPDur.WithLabelValues(p.Server, p.Name).Observe(d)
			}
		})
		if !isMCP {
			m.ins.ToolCalls.WithLabelValues(p.Name, labels.Agent, p.Skill).Inc()
		}
	default:
		// Unknown phase: emit a complete standalone span so the activity is
		// visible rather than silently dropped; outcome unknown.
		attrs = append(attrs, attrOutcome.String(outcomeUnknown))
		_, span := m.start(ctx, name, attrs)
		span.SetStatus(codes.Unset, "")
		span.End()
	}
}

// SkillEvent maps one EventSkillLoad payload: a skill.load span (skill name
// + pinned source SHA) and a ksquad_skill_loads_total increment.
func (m *Mapper) SkillEvent(ctx context.Context, labels Labels, p a2a.SkillLoadPayload) {
	if !enabled.Load() || p.Name == "" {
		return
	}
	attrs := labels.spanAttrs()
	attrs = append(attrs, attrSkillName.String(p.Name))
	if p.SHA256 != "" {
		attrs = append(attrs, attrSkillSHA.String(p.SHA256))
	}
	attrs = append(attrs, attrOutcome.String(mapOutcome(p.OK)))

	_, span := m.start(ctx, SpanSkillLoad, attrs)
	if p.Err != "" {
		span.SetStatus(codes.Error, p.Err)
		span.RecordError(errString(p.Err))
	} else if p.OK != nil && *p.OK {
		span.SetStatus(codes.Ok, "")
	}
	span.End()

	m.ins.SkillLoads.WithLabelValues(p.Name, labels.Agent).Inc()
}

// FinishTask sweeps any still-open spans for taskID (a runtime that crashed
// between start and result must not leak pending entries). The spans end
// with unknown outcome — the call started, its result never arrived.
func (m *Mapper) FinishTask(ctx context.Context, taskID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, ps := range m.pending {
		if taskIDOf(k) != taskID {
			continue
		}
		ps.span.SetAttributes(attrOutcome.String(outcomeUnknown))
		ps.span.SetStatus(codes.Unset, "")
		ps.span.End()
		delete(m.pending, k)
	}
}

func taskIDOf(key string) string {
	for i := 0; i < len(key); i++ {
		if key[i] == '\x00' {
			return key[:i]
		}
	}
	return key
}

func (m *Mapper) start(ctx context.Context, name string, attrs []attribute.KeyValue) (context.Context, trace.Span) {
	if m.tracer == nil {
		return ctx, noopSpan()
	}
	return m.tracer.Start(ctx, name, trace.WithAttributes(attrs...))
}

// settle closes the open span for (taskID, tool) — or, when none is pending
// (orphan result), records a fresh complete span. onDuration receives the
// measured start→result seconds when a pending span existed (never called
// for synthesized ones — duration is only truthfully measurable
// start→result).
func (m *Mapper) settle(ctx context.Context, taskID, tool, name string, attrs []attribute.KeyValue, outcome string, onDuration func(float64)) {
	key := spanKey(taskID, tool)
	m.mu.Lock()
	ps, ok := m.pending[key]
	if ok {
		delete(m.pending, key)
	}
	m.mu.Unlock()

	if !ok {
		_, span := m.start(ctx, name, attrs)
		span.End()
		return
	}
	ps.span.SetAttributes(attrs...)
	switch outcome {
	case outcomeError:
		ps.span.SetStatus(codes.Error, "")
	case outcomeSuccess:
		ps.span.SetStatus(codes.Ok, "")
	}
	ps.span.End()
	if onDuration != nil {
		onDuration(ps.start())
	}
}

// noopSpan is a non-recording span used when the tracer is nil (pre-Setup).
func noopSpan() trace.Span {
	return trace.SpanFromContext(context.Background())
}

// mapOutcome maps the wire's tri-state OK (true / false / absent) onto the
// span outcome vocabulary. Absent (a result event with no OK field — the
// emitter could not tell) maps to unknown — never guessed.
func mapOutcome(ok *bool) string {
	if ok == nil {
		return outcomeUnknown
	}
	if *ok {
		return outcomeSuccess
	}
	return outcomeError
}

// errString adapts a wire error string to an error for RecordError without
// inventing a type: the wire carries only text.
type errString string

func (e errString) Error() string { return string(e) }
