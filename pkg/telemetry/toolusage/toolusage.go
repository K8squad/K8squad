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
// it maps A2A tool/skill/usage activity events (pkg/a2a EventTool /
// EventSkillLoad / EventUsage) onto OpenTelemetry GenAI-semconv spans and
// ksquad_* metrics.
//
// Spans (names per OTel GenAI agent conventions + plan §2.4 + ISI-4238):
//
//	run.start / run.end — the run's dispatch/terminal markers opening and
//	                     closing the run's root trace: every span emitted
//	                     for the task between them joins that trace, so a
//	                     failing Run is one end-to-end debuggable unit
//	gen_ai.tool.call   — a local/CLI tool call: gen_ai.tool.name, hashed
//	                     args (gen_ai.tool.call.arguments carries the hex
//	                     sha256 — raw arguments NEVER travel), outcome, duration
//	llm.call           — one model round-trip (step) with the full gen-AI
//	                     semconv surface (ISI-4238, ISI-4383): gen_ai.system
//	                     (provider), gen_ai.operation.name, request + response
//	                     model, input/output/reasoning tokens (reasoning
//	                     distinct, not folded), cache read/write, finish
//	                     reason, response id, provider cost, truthful step
//	                     duration; ksquad.llm.fallback=true marks a
//	                     backup-model-served step. Prompt/response bodies stay
//	                     OFF by default (D3 PII posture) — captured as gated
//	                     span events only under KSQUAD_TRACE_CONTENT.
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
//	ksquad_llm_tokens_total{model,agent,direction} counter (ISI-4238)
//	ksquad_llm_calls_total{model,agent}            counter (ISI-4238)
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
	"fmt"
	"os"
	"strconv"
	"strings"
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

// Span names (plan §2.4, ISI-4238). gen_ai.tool.call follows the GenAI
// semconv agent conventions; run.start/run.end and llm.call are the
// ISI-4238 run-trace vocabulary (Henrik: "run.start, llm.call, tool.call,
// run.end"); skill.load and mcp.call are the plan's names for the two
// adjacent activities the conventions do not yet standardize.
const (
	SpanRunStart  = "run.start"
	SpanRunEnd    = "run.end"
	SpanToolCall  = "gen_ai.tool.call"
	SpanLLMCall   = "llm.call"
	SpanSkillLoad = "skill.load"
	SpanMCPCall   = "mcp.call"
)

// K8squad-namespaced attribute keys: run/agent correlation (spans are
// "queryable per run/agent", plan §5.4) and the plan's hashed-args / source
// SHA attributes that GenAI semconv does not define.
const (
	attrRunID     = attribute.Key("ksquad.run.id")
	attrAgentName = attribute.Key("ksquad.agent.name")
	// WS-A run-trace correlation (ISI-4382, ADR-0021 D1): every run-trace
	// span carries team/project/ticket/sandbox identity, not only run+agent,
	// so a single trace in the backend answers "whose run, which project,
	// which ticket, which pod" without joining back to the CR. Span
	// attributes ONLY — deliberately never metric labels (cardinality note).
	attrTeamName    = attribute.Key("ksquad.team.name")
	attrProjectName = attribute.Key("ksquad.project.name")
	attrWorkItemRef = attribute.Key("ksquad.work_item.ref")
	attrSandboxPod  = attribute.Key("ksquad.sandbox.pod")
	attrSkillName   = attribute.Key("ksquad.skill.name")
	attrSkillSHA    = attribute.Key("ksquad.skill.source.sha")
	attrMCPServer   = attribute.Key("ksquad.mcp.server")
	// attrToolType categorizes the tool (git|mcp|docker|npm|bash|…) so the
	// trace answers "which KIND of tool ran" without parsing gen_ai.tool.name
	// (ISI-4540: tools were all surfacing as bare names with no category).
	attrToolType = attribute.Key("ksquad.tool.type")
	// attrToolCommand names the executable a shell-family tool call actually
	// ran (git|kubectl|npm|…) when the runtime models it as an argument to a
	// single "bash" tool (ISI-4720). gen_ai.tool.name stays "bash" (the tool
	// the runtime exposed); this attribute + ksquad.tool.type answer "a bash
	// span, but it ran git" so git/kubectl/docker calls are no longer
	// invisible. The shim populates it (a2a.ToolPayload.Command) with only the
	// bounded, recognized head token — never the argument line.
	attrToolCommand = attribute.Key("ksquad.tool.command")
	// attrOutcome records the mapped outcome ("success" | "error" |
	// "unknown") — D1 AC: unknown outcomes map safely, never panic, never
	// drop the span.
	attrOutcome = attribute.Key("ksquad.outcome")
	// attrDurationMS is the span's measured wall-clock duration in
	// milliseconds, stamped as an explicit attribute so a backend can query /
	// aggregate call latency as a dimension without deriving it from the span
	// start/end timestamps (ISI-4385: WS-C "tool/skill calls appear ... with
	// duration"). It rides tool / mcp.call spans (measured start→result) and
	// skill.load spans (the point-event mapping instant); llm.call already
	// carries its truthful step duration via the span timestamps (ISI-4238).
	attrDurationMS = attribute.Key("ksquad.duration.ms")
	// attrModelTier records which Model-Per-Role tier supplied the run's
	// effective model at dispatch ("agent" | "role" | "default") — the resolved
	// origin so an operator sees WHY the run used its model without re-deriving
	// the tier walk (ISI-4430 S5). Stamped on the run.start root only.
	attrModelTier = attribute.Key("ksquad.model.tier")
	// attrRunState carries the §3.1 terminal state on run.end
	// (completed|failed|canceled) so the trace answers "how did it end"
	// without joining back to the CR (ISI-4238).
	attrRunState = attribute.Key("ksquad.run.state")
	// GenAI semconv keys the v1.40 stable set does not export as typed
	// constants yet (ISI-4238 / ISI-4383 llm.call spans). Kept as raw keys
	// so the whole gen_ai.* surface reads in one place.
	attrGenAIRequestModel     = attribute.Key("gen_ai.request.model")
	attrGenAIResponseModel    = attribute.Key("gen_ai.response.model")
	attrGenAISystem           = attribute.Key("gen_ai.system")
	attrGenAIOperationName    = attribute.Key("gen_ai.operation.name")
	attrGenAIInputTokens      = attribute.Key("gen_ai.usage.input_tokens")
	attrGenAIOutputTokens     = attribute.Key("gen_ai.usage.output_tokens")
	attrGenAIReasoningTokens  = attribute.Key("gen_ai.usage.reasoning_tokens")
	attrGenAICacheReadTokens  = attribute.Key("gen_ai.usage.cache_read_tokens")
	attrGenAICacheWriteTokens = attribute.Key("gen_ai.usage.cache_write_tokens")
	attrGenAIFinishReasons    = attribute.Key("gen_ai.response.finish_reasons")
	attrGenAIResponseID       = attribute.Key("gen_ai.response.id")
	attrLLMCostUSD            = attribute.Key("ksquad.llm.cost.usd")
	// attrLLMFallback marks an llm.call whose step was served by the Run's
	// backup/fallback model (ISI-4383): true correlates the span to a
	// ksquad_fallback_activations_total increment (story 5.11).
	attrLLMFallback = attribute.Key("ksquad.llm.fallback")

	// attrLLMDurationMeasured marks an llm.call whose duration was measured by
	// the shim's step clock rather than reported by the runtime wire (ISI-4238):
	// true means the runtime (e.g. opencode v1.18.27) did not carry a step
	// duration, so the span's latency is the wall-clock between step boundaries.
	attrLLMDurationMeasured = attribute.Key("ksquad.llm.duration_measured")

	// operationChat is the gen_ai.operation.name for a model round-trip
	// (llm.call). The OTel gen-AI semconv "chat" operation.
	operationChat = "chat"

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

// contentTracing is the D3 prompt/response content-capture gate (ISI-4383,
// ADR-0021 D3). It is OFF by default: the PII posture means llm.call never
// carries prompt/response bodies in prod (tool args are SHA-256 only). It
// flips on ONLY via the KSQUAD_TRACE_CONTENT env flag (dev/non-prod, with the
// documented risk) so an operator can opt in to prompt/response span events
// for local debugging. Nothing populates UsagePayload.Prompt/Response in the
// default build, so even with the flag on the events appear only when a
// content-carrying event is deliberately produced.
var contentTracing atomic.Bool

// SetContentTracing flips the D3 content-capture gate (test seam + explicit
// wiring). Safe for concurrent use.
func SetContentTracing(v bool) { contentTracing.Store(v) }

// ContentTracingEnabled reports the current D3 content-capture gate value.
func ContentTracingEnabled() bool { return contentTracing.Load() }

// envTruthy parses an env flag the same lenient way across the package:
// strconv.ParseBool ("1"/"t"/"true"/…) with "" and unparseable → false.
func envTruthy(v string) bool {
	b, err := strconv.ParseBool(v)
	return err == nil && b
}

func init() {
	enabled.Store(true)
	// D3: opt in to content capture only when the env flag is explicitly
	// truthy. Absent or unparseable → stays off (default-safe PII posture).
	contentTracing.Store(envTruthy(os.Getenv("KSQUAD_TRACE_CONTENT")))
}

// Labels identify the emitting Run and its context. They ride every span
// (attributes) so a backend can filter one trace by run / agent / team /
// project / ticket / sandbox pod (WS-A, ISI-4382). Only agent flows into
// metric label sets — run.id and the WS-A identity fields would explode
// counter cardinality, so they stay span attributes only (same discipline
// as 13.6; ADR-0021 D1 cardinality note).
type Labels struct {
	RunID string
	Agent string
	// Team is the tenant team (Run.Spec.TeamRef / shim env KSQUAD_SQUAD).
	Team string
	// Project is the owning project (Run.Spec.ProjectRef / KSQUAD_PROJECT).
	Project string
	// WorkItemRef is the ticket/work-item the Run serves
	// (Run.Spec.WorkItemRef) — the ticket identity on the trace.
	WorkItemRef string
	// SandboxPod is the sandbox pod hosting the Run (Run.Status.SandboxRef /
	// the shim's own pod name) — one run per pod.
	SandboxPod string
	// ModelTier is the Model-Per-Role origin (agent|role|default) the operator
	// resolved for the run's effective model at dispatch (ISI-4430 S5). Unlike
	// the fields above it rides ONLY the run.start span (via RunStart), not
	// spanAttrs — it is a run-level dispatch fact, and the per-step serving
	// model of a fallback is already carried by gen_ai.response.model +
	// ksquad.llm.fallback on the llm.call span. Empty when unresolved.
	ModelTier string
}

func (l Labels) spanAttrs() []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 6)
	if l.RunID != "" {
		attrs = append(attrs, attrRunID.String(l.RunID))
	}
	if l.Agent != "" {
		attrs = append(attrs, attrAgentName.String(l.Agent))
	}
	if l.Team != "" {
		attrs = append(attrs, attrTeamName.String(l.Team))
	}
	if l.Project != "" {
		attrs = append(attrs, attrProjectName.String(l.Project))
	}
	if l.WorkItemRef != "" {
		attrs = append(attrs, attrWorkItemRef.String(l.WorkItemRef))
	}
	if l.SandboxPod != "" {
		attrs = append(attrs, attrSandboxPod.String(l.SandboxPod))
	}
	return attrs
}

// Instruments is the metric set (D2). Exposed for registry wiring; the
// Mapper holds one set for its lifetime.
type Instruments struct {
	ToolCalls  *prometheus.CounterVec
	SkillLoads *prometheus.CounterVec
	MCPDur     *prometheus.HistogramVec
	// LLMCalls / LLMTokens are the ISI-4238 LLM-observability set: one
	// counter per model round-trip and token totals split input/output
	// via the direction label (bounded: {model, agent, direction}).
	LLMCalls  *prometheus.CounterVec
	LLMTokens *prometheus.CounterVec
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
		LLMCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ksquad_llm_calls_total",
			Help: "Model round-trips mapped from A2A EventUsage events (ISI-4238). Bounded labels {model,agent}.",
		}, []string{"model", "agent"}),
		LLMTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ksquad_llm_tokens_total",
			Help: "LLM token totals mapped from A2A EventUsage events (ISI-4238); reasoning tokens count as output-class. Bounded labels {model,agent,direction}.",
		}, []string{"model", "agent", "direction"}),
		PipelineUp: pipelineUp{},
	}
	if reg != nil {
		reg.MustRegister(ins.ToolCalls, ins.SkillLoads, ins.MCPDur, ins.LLMCalls, ins.LLMTokens, ins.PipelineUp)
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
// Run traces (ISI-4238): RunStart opens a root span named "run.start" that
// stays open for the task's lifetime — every span emitted with the returned
// context (llm.call, gen_ai.tool.call, …) joins that one trace — and RunEnd
// stamps the terminal outcome onto it, emits a "run.end" marker child and
// closes the root. The root's trace id is the run's TraceID.
//
// A Mapper is safe for concurrent use.
type Mapper struct {
	tracer trace.Tracer
	now    func() func() float64
	ins    *Instruments

	mu      sync.Mutex
	pending map[string]pendingSpan // "task\x00tool" → open span
	runs    map[string]trace.Span  // taskID → open run root span (ISI-4238)
	steps   map[string]*stepTimer  // taskID → per-run llm.call step clock
}

// pendingSpan is one open tool/MCP call: its span and the wall-clock start
// so the result phase can observe a truthful duration.
type pendingSpan struct {
	span  trace.Span
	start func() float64
}

// stepTimer measures the wall-clock latency of each llm.call step for a run
// whose usage wire does NOT carry a step duration (opencode v1.18.27's
// step-finish omits it — the source of the "7µs llm.call" Henrik flagged: with
// no duration the span collapsed to its own open/close overhead). It rides the
// run's stopwatch (elapsed seconds since run.start) and remembers the elapsed
// value at the previous step boundary, so each step's measured duration is the
// gap between consecutive step_finish events (the first step measured from
// run.start) — a truthful step latency instead of an instantaneous point.
type stepTimer struct {
	elapsed func() float64
	last    float64
}

// measureStepMS advances the run's step clock and returns the measured latency
// of the step that just finished, in milliseconds. Zero when no timer exists
// (a UsageEvent without a preceding RunStart) so the caller falls back to the
// prior instantaneous behavior rather than fabricating a duration.
func (m *Mapper) measureStepMS(taskID string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.steps[taskID]
	if st == nil {
		return 0
	}
	cur := st.elapsed()
	stepSecs := cur - st.last
	st.last = cur
	if stepSecs <= 0 {
		return 0
	}

	// Validate timing is realistic - local LLM should not respond in microseconds
	ms := int64(stepSecs * 1000)
	if ms > 0 && ms < 10 {
		// Log unrealistic timing for debugging but still return the measured value
		fmt.Fprintf(os.Stderr, "WARNING: Unrealistic LLM step timing detected: %dms for task %s\n", ms, taskID)
	}

	return ms
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
		runs:    map[string]trace.Span{},
		steps:   map[string]*stepTimer{},
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

// categorizeTool maps a tool event onto its category for the ksquad.tool.type
// span attribute (ISI-4540). An MCP-served tool (Server set) is always "mcp";
// otherwise the head token (up to the first dot) decides, with the dotted
// server.tool form as the MCP fallback for emitters that don't set Server.
func categorizeTool(name, server string) string {
	if server != "" {
		return "mcp"
	}
	head := name
	if i := strings.IndexByte(name, '.'); i >= 0 {
		head = name[:i]
	}
	switch head {
	case "bash", "sh":
		return "bash"
	case "git":
		return "git"
	case "docker":
		return "docker"
	case "kubectl":
		return "kubectl"
	case "helm":
		return "helm"
	case "npm", "node":
		return "node"
	case "pip", "python", "python3":
		return "python"
	case "mcp":
		return "mcp"
	case "system", "internal":
		return "system"
	}
	if strings.Contains(name, ".") && !strings.Contains(name, "/") {
		return "mcp"
	}
	return "system"
}

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
	// ISI-4720: a shell-family call (bash) whose real executable the shim
	// resolved (p.Command: git|kubectl|npm|…) is categorized by what it RAN,
	// and the executable rides ksquad.tool.command, so a bash-wrapped git call
	// is no longer an opaque "bash" span. gen_ai.tool.name stays the tool the
	// runtime exposed. The command is only trusted for local (non-MCP) calls.
	toolType := categorizeTool(p.Name, p.Server)
	attrs = append(attrs, semconv.GenAIToolName(p.Name))
	if !isMCP && p.Command != "" {
		toolType = categorizeTool(p.Command, "")
		attrs = append(attrs, attrToolCommand.String(p.Command))
	}
	attrs = append(attrs, attrToolType.String(toolType))
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
		// ISI-4540: mcp.call is an outbound request to the MCP server —
		// SpanKindClient; local tool.call stays internal (in-process exec).
		opts := []trace.SpanStartOption{trace.WithAttributes(attrs...)}
		if isMCP {
			opts = append(opts, trace.WithSpanKind(trace.SpanKindClient))
		}
		_, span := m.startWithOptions(ctx, name, opts)
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

	// skill.load is a point event on the wire (a completed load, not a
	// start/result pair — no runtime reports a load latency), so its duration
	// is the mapping instant. Stamp it anyway (ISI-4385) so ksquad.duration.ms
	// is present uniformly across every activity span (tool / mcp / skill), and
	// so the attribute extends truthfully if a runtime ever pairs skill loads.
	elapsed := m.now()
	_, span := m.start(ctx, SpanSkillLoad, attrs)
	if p.Err != "" {
		span.SetStatus(codes.Error, p.Err)
		span.RecordError(errString(p.Err))
	} else if p.OK != nil && *p.OK {
		span.SetStatus(codes.Ok, "")
	}
	span.SetAttributes(attrDurationMS.Int64(durationMS(elapsed())))
	span.End()

	m.ins.SkillLoads.WithLabelValues(p.Name, labels.Agent).Inc()
}

// RunStart opens the run's root trace (ISI-4238): a span named "run.start"
// that stays open for the task's lifetime. The returned context carries it,
// so every telemetry call the emitter makes with that context (llm.call,
// gen_ai.tool.call, skill.load, mcp.call) joins the same trace — a run is
// no longer a black box between dispatch and exit. RunEnd closes the root.
// The span is returned so the emitter can surface its trace id onto the
// wire (a2a.Status.TraceID → Run.Status.TraceID).
func (m *Mapper) RunStart(ctx context.Context, labels Labels, taskID string) (context.Context, trace.Span) {
	if !enabled.Load() || taskID == "" {
		return ctx, noopSpan()
	}
	attrs := labels.spanAttrs()
	// ksquad.model.tier rides the run root ONLY (ISI-4430 S5): it is the
	// run-level dispatch origin, not a per-span fact, so it stays off
	// spanAttrs() and is appended here.
	if labels.ModelTier != "" {
		attrs = append(attrs, attrModelTier.String(labels.ModelTier))
	}
	runCtx, span := m.start(ctx, SpanRunStart, attrs)
	m.mu.Lock()
	m.runs[taskID] = span
	// Arm the per-run step clock so an llm.call whose wire omits a duration is
	// measured from run.start onward (ISI-4238, the "7µs" fix).
	m.steps[taskID] = &stepTimer{elapsed: m.now()}
	m.mu.Unlock()
	return runCtx, span
}

// RunEnd closes the run's root span with its terminal outcome and emits a
// "run.end" marker child carrying the §3.1 state and reason (ISI-4238).
// state is the a2a TaskState string ("completed" | "failed" |
// "canceled" | …); it maps onto the outcome vocabulary for the status code.
func (m *Mapper) RunEnd(ctx context.Context, taskID, state, reason string) {
	if !enabled.Load() || taskID == "" {
		return
	}
	outcome := outcomeFromState(state)

	m.mu.Lock()
	root, ok := m.runs[taskID]
	delete(m.runs, taskID)
	delete(m.steps, taskID)
	m.mu.Unlock()

	// run.end marker: the terminal instant, queryable on its own.
	endAttrs := []attribute.KeyValue{attrRunState.String(state), attrOutcome.String(outcome)}
	if root != nil {
		endAttrs = append(rootAttrs(root), endAttrs...)
	}
	_, marker := m.start(ctx, SpanRunEnd, endAttrs)
	if reason != "" {
		marker.SetStatus(codes.Error, reason)
	}
	marker.End()

	if !ok || root == nil {
		return // RunEnd without RunStart: the marker is still emitted.
	}
	root.SetAttributes(attrRunState.String(state), attrOutcome.String(outcome))
	if reason != "" {
		root.SetStatus(codes.Error, reason)
	} else if outcome == outcomeSuccess {
		root.SetStatus(codes.Ok, "")
	}
	root.End()
}

// rootAttrs re-derives the labels attributes from an open run root span so
// the run.end marker carries the same run/agent correlation without the
// caller re-passing Labels (the span already holds them).
func rootAttrs(span trace.Span) []attribute.KeyValue {
	attrs := []attribute.KeyValue{}
	if span == nil {
		return attrs
	}
	sc := span.SpanContext()
	if sc.HasTraceID() {
		attrs = append(attrs, attribute.String("ksquad.run.trace_id", sc.TraceID().String()))
	}
	return attrs
}

// outcomeFromState maps an a2a TaskState onto the outcome vocabulary for
// run terminal spans. Terminal-completed is success; the failure-family
// (failed/canceled/anything else) is error-shaped; the mapping never
// guesses "success" for an unrecognized state.
func outcomeFromState(state string) string {
	if state == "completed" {
		return outcomeSuccess
	}
	return outcomeError
}

// UsageEvent maps one EventUsage payload (ISI-4238, ISI-4383): a complete
// llm.call span carrying the OTel gen-AI semconv surface — system (provider),
// operation, requested + served model, input/output/reasoning tokens, cache
// read/write, finish reason, response id — plus the provider-reported cost,
// with the span's duration set truthfully from the runtime-reported step
// duration (start = now−duration, end = now); plus the ksquad_llm_calls_total
// / ksquad_llm_tokens_total counters. A fallback-served step is flagged with
// ksquad.llm.fallback=true (correlates to ksquad_fallback_activations_total).
// With no tracer attached the metrics still count. A usage without a model id
// is dropped (unattributable — never fabricate "unknown" buckets).
//
// Reasoning tokens are reported DISTINCTLY on the span
// (gen_ai.usage.reasoning_tokens), no longer folded into output_tokens (D2).
// The ksquad_llm_tokens_total "output" metric direction deliberately keeps
// reasoning folded in — reasoning is billed as output-class, and that metric
// is the billing/output view (its Help documents this).
func (m *Mapper) UsageEvent(ctx context.Context, labels Labels, taskID string, p a2a.UsagePayload) {
	if !enabled.Load() || p.Model == "" {
		return
	}
	attrs := labels.spanAttrs()
	attrs = append(attrs,
		attrGenAIOperationName.String(operationChat),
		attrGenAIRequestModel.String(p.Model),
		attrGenAIInputTokens.Int(p.Input),
		attrGenAIOutputTokens.Int(p.Output),
	)
	if p.Provider != "" {
		attrs = append(attrs, attrGenAISystem.String(p.Provider))
	}
	if p.ResponseModel != "" {
		attrs = append(attrs, attrGenAIResponseModel.String(p.ResponseModel))
	}
	if p.Reasoning != 0 {
		attrs = append(attrs, attrGenAIReasoningTokens.Int(p.Reasoning))
	}
	if p.CacheRead != 0 {
		attrs = append(attrs, attrGenAICacheReadTokens.Int(p.CacheRead))
	}
	if p.CacheWrite != 0 {
		attrs = append(attrs, attrGenAICacheWriteTokens.Int(p.CacheWrite))
	}
	if p.FinishReason != "" {
		// semconv types finish_reasons as a string array: one step reports
		// one reason, carried as a single-element slice.
		attrs = append(attrs, attrGenAIFinishReasons.StringSlice([]string{p.FinishReason}))
	}
	if p.ResponseID != "" {
		attrs = append(attrs, attrGenAIResponseID.String(p.ResponseID))
	}
	if p.CostUSD != 0 {
		attrs = append(attrs, attrLLMCostUSD.Float64(p.CostUSD))
	}
	if p.Fallback {
		attrs = append(attrs, attrLLMFallback.Bool(true))
	}

	// Step duration: prefer the runtime-reported step duration; when the wire
	// omits it (opencode v1.18.27's step-finish carries no `duration`, so the
	// span otherwise collapsed to ~7µs — Henrik's finding), fall back to the
	// wall-clock the run's step clock measured between this and the previous
	// step boundary. attrGenAIStepDurationMeasured marks the fallback so the
	// backend can tell a measured latency from a wire-reported one.
	durMS := p.DurationMS
	measured := false
	if durMS <= 0 {
		if ms := m.measureStepMS(taskID); ms > 0 {
			durMS = ms
			measured = true
		}
	}
	var opts []trace.SpanStartOption
	if durMS > 0 {
		start := time.Now().Add(-time.Duration(durMS) * time.Millisecond)
		opts = append(opts, trace.WithTimestamp(start))
	}
	if measured {
		attrs = append(attrs, attrLLMDurationMeasured.Bool(true))
	}
	// ISI-4540: llm.call is an outbound request to the model endpoint — emit it
	// as SpanKindClient so backends (Dynatrace) model it as a service call
	// instead of collapsing the run into internal-only spans.
	opts = append(opts,
		trace.WithAttributes(attrs...),
		trace.WithSpanKind(trace.SpanKindClient))
	_, span := m.startWithOptions(ctx, SpanLLMCall, opts)
	// D3: prompt/response bodies ride as gated span events, never as
	// attributes, and only when the content gate is on AND the payload
	// actually carries them (default-off PII posture — see contentTracing).
	if contentTracing.Load() {
		recordContentEvents(span, p)
	}
	span.End()

	m.ins.LLMCalls.WithLabelValues(p.Model, labels.Agent).Inc()
	m.ins.LLMTokens.WithLabelValues(p.Model, labels.Agent, "input").Add(float64(p.Input))
	m.ins.LLMTokens.WithLabelValues(p.Model, labels.Agent, "output").Add(float64(p.Output + p.Reasoning))
}

// recordContentEvents adds the D3 opt-in prompt/response bodies as span
// events (gen_ai.content.prompt / gen_ai.content.completion). It is only
// reached with the content gate on; it still no-ops on empty bodies so an
// enabled-but-content-free step stays clean. Content NEVER becomes a span
// attribute (attributes are indexed/queryable; events are the semconv-blessed
// carrier for bulky, sensitive payloads).
func recordContentEvents(span trace.Span, p a2a.UsagePayload) {
	if p.Prompt != "" {
		span.AddEvent("gen_ai.content.prompt",
			trace.WithAttributes(attribute.String("gen_ai.prompt", p.Prompt)))
	}
	if p.Response != "" {
		span.AddEvent("gen_ai.content.completion",
			trace.WithAttributes(attribute.String("gen_ai.completion", p.Response)))
	}
}

// FinishTask sweeps any still-open spans for taskID (a runtime that crashed
// between start and result must not leak pending entries). The spans end
// with unknown outcome — the call started, its result never arrived. An
// unclosed run root (RunEnd never reached) is closed here the same way.
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
	if root, ok := m.runs[taskID]; ok {
		root.SetAttributes(attrRunState.String("unknown"), attrOutcome.String(outcomeUnknown))
		root.SetStatus(codes.Unset, "")
		root.End()
		delete(m.runs, taskID)
	}
	delete(m.steps, taskID)
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

// startWithOptions is start with explicit SpanStartOptions (UsageEvent's
// truthful step-duration timestamps); it shares the nil-tracer degrade.
func (m *Mapper) startWithOptions(ctx context.Context, name string, opts []trace.SpanStartOption) (context.Context, trace.Span) {
	if m.tracer == nil {
		return ctx, noopSpan()
	}
	return m.tracer.Start(ctx, name, opts...)
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
		// Orphan result (no start seen): the span's extent is unknowable — the
		// start instant never arrived — so it carries outcome but no duration
		// (never fabricated). The synthesized span is still complete + visible.
		_, span := m.start(ctx, name, attrs)
		span.End()
		return
	}
	// Duration is truthfully measurable start→result for a paired call: stamp
	// it on the span (ISI-4385) AND feed the histogram observer, from the one
	// measurement so span and metric agree.
	d := ps.start()
	ps.span.SetAttributes(attrs...)
	ps.span.SetAttributes(attrDurationMS.Int64(durationMS(d)))
	switch outcome {
	case outcomeError:
		ps.span.SetStatus(codes.Error, "")
	case outcomeSuccess:
		ps.span.SetStatus(codes.Ok, "")
	}
	ps.span.End()
	if onDuration != nil {
		onDuration(d)
	}
}

// durationMS converts a seconds duration to whole milliseconds (rounded) for
// the ksquad.duration.ms span attribute. Negative inputs (a clock that ran
// backwards) clamp to 0 — a duration attribute is never negative.
func durationMS(seconds float64) int64 {
	if seconds <= 0 {
		return 0
	}
	return int64(seconds*1000 + 0.5)
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
