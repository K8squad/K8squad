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

// Package ksqsemconv is the canonical, machine-readable registry of K8squad's
// telemetry semantic conventions (ISI-4387 / ADR-0021 WS-F). It is the single
// source of truth for:
//
//   - the K8squad-namespaced (`ksquad.*`) and OpenTelemetry gen-AI (`gen_ai.*`)
//     span attributes carried by the run trace
//     (run.start / llm.call / gen_ai.tool.call / mcp.call / skill.load /
//     run.end), and
//   - the five NATS domain lifecycle events on the outbox→relay spine
//     (work_item.assigned, run.scheduled, run.sandbox_bound, run.started,
//     run.ended).
//
// Two artefacts are DERIVED from this registry so they can never silently
// drift from the code:
//
//   - conformance_test.go drives the real pkg/telemetry/toolusage.Mapper and
//     asserts every attribute the registry marks Stable is actually emitted on
//     the right span, and that no undocumented `ksquad.*`/`gen_ai.*` key leaks.
//   - docs/observability/semantic-conventions.md is rendered from Render() and
//     kept honest by a golden test (run `go test ./pkg/telemetry/semconv/
//     -run TestDocInSync -update-semconv-docs` to regenerate).
//
// STABILITY MODEL. Each attribute/event carries a Stability:
//
//   - Stable  — emitted by the code today; the conformance test REQUIRES it.
//   - Planned — specified here as the contract for a not-yet-landed ADR-0021
//     workstream (WS-A/B/D). Documented and regression-guarded (if it is
//     emitted early it must already be documented), but NOT required until the
//     owning workstream lands and flips it Stable. This is what lets the
//     conformance test stay green now and start enforcing the enriched set the
//     moment WS-A/B/D merge — the guard the acceptance criteria call for.
package ksqsemconv

// Stability marks whether an attribute/event is emitted today (Stable) or is
// the specified contract for a not-yet-landed ADR-0021 workstream (Planned).
type Stability string

const (
	// Stable attributes are emitted by the current code; the conformance test
	// requires them.
	Stable Stability = "stable"
	// Planned attributes are the contract for a pending ADR-0021 workstream
	// (Workstream names which one). Documented + regression-guarded, required
	// only once that workstream flips them Stable.
	Planned Stability = "planned"
)

// Requirement is the semconv requirement level for an attribute on its span.
type Requirement string

const (
	// Required attributes MUST be present on every emission of the span (given
	// a fully-populated source event); the conformance test asserts them.
	Required Requirement = "required"
	// Conditional attributes are present only when their source field is set
	// (e.g. cache token counts, hashed tool args, skill source SHA). The
	// conformance test drives fully-populated fixtures so they appear, then
	// asserts them; they may legitimately be absent in production emissions.
	Conditional Requirement = "conditional"
	// Recommended attributes SHOULD be present but their absence is not a
	// conformance failure.
	Recommended Requirement = "recommended"
)

// AttrType is the OTel attribute value type.
type AttrType string

const (
	TypeString      AttrType = "string"
	TypeInt         AttrType = "int"
	TypeDouble      AttrType = "double"
	TypeBool        AttrType = "boolean"
	TypeStringSlice AttrType = "string[]"
)

// Attribute is one entry in a span (or event payload) convention.
type Attribute struct {
	Key         string
	Type        AttrType
	Requirement Requirement
	Stability   Stability
	// Workstream names the ADR-0021 child that lands a Planned attribute
	// (e.g. "WS-A", "WS-B"); empty for Stable attributes already shipped.
	Workstream string
	Brief      string
}

// SpanConvention is the attribute contract for one run-trace span.
type SpanConvention struct {
	Name       string
	Brief      string
	Attributes []Attribute
}

// Span name constants mirror pkg/telemetry/toolusage so the registry and the
// emitter share one vocabulary (the conformance test asserts they agree).
const (
	SpanRunStart  = "run.start"
	SpanRunEnd    = "run.end"
	SpanLLMCall   = "llm.call"
	SpanToolCall  = "gen_ai.tool.call"
	SpanMCPCall   = "mcp.call"
	SpanSkillLoad = "skill.load"
)

// runIdentity is the correlation set every run-trace span should carry. run.id
// and agent.name ship today (WS-A prereq already met by ISI-4238); team,
// project, work_item.ref and sandbox.pod are the WS-A enrichment — specified
// here so WS-A lands against a written contract and F's dashboard can rely on
// them existing on EVERY span.
func runIdentity() []Attribute {
	return []Attribute{
		{Key: "ksquad.run.id", Type: TypeString, Requirement: Required, Stability: Stable,
			Brief: "The Run's stable id; the run trace's correlation key. High-cardinality — spans/events only, never a metric label."},
		{Key: "ksquad.agent.name", Type: TypeString, Requirement: Required, Stability: Stable,
			Brief: "The agent executing the run (e.g. \"coder\")."},
		{Key: "ksquad.team.name", Type: TypeString, Requirement: Required, Stability: Planned, Workstream: "WS-A",
			Brief: "The team/squad the run belongs to. Drill-down axis for the WS-F dashboard."},
		{Key: "ksquad.project.name", Type: TypeString, Requirement: Required, Stability: Planned, Workstream: "WS-A",
			Brief: "The project the run belongs to. Drill-down axis."},
		{Key: "ksquad.work_item.ref", Type: TypeString, Requirement: Required, Stability: Planned, Workstream: "WS-A",
			Brief: "The work item / ticket the run is servicing. Ticket→run→spans drill-down key."},
		{Key: "ksquad.sandbox.pod", Type: TypeString, Requirement: Recommended, Stability: Planned, Workstream: "WS-A",
			Brief: "The sandbox pod hosting the run (data-plane locality)."},
	}
}

// SpanConventions is the full run-trace span contract. Order is stable so the
// rendered markdown is deterministic.
var SpanConventions = []SpanConvention{
	{
		Name:  SpanRunStart,
		Brief: "Root span of a run's trace: opens at dispatch, stays open for the run's lifetime; every other run-trace span joins it. Its trace id IS the run's TraceID.",
		Attributes: append(runIdentity(),
			Attribute{Key: "ksquad.run.state", Type: TypeString, Requirement: Recommended, Stability: Stable,
				Brief: "Terminal state stamped onto the root at RunEnd (completed|failed|canceled|unknown)."},
			Attribute{Key: "ksquad.outcome", Type: TypeString, Requirement: Recommended, Stability: Stable,
				Brief: "Mapped outcome stamped at RunEnd (success|error|unknown)."},
		),
	},
	{
		Name:  SpanRunEnd,
		Brief: "Terminal marker child emitted at RunEnd; the run's end instant, queryable on its own.",
		Attributes: []Attribute{
			{Key: "ksquad.run.state", Type: TypeString, Requirement: Required, Stability: Stable,
				Brief: "Terminal a2a task state (completed|failed|canceled|unknown)."},
			{Key: "ksquad.outcome", Type: TypeString, Requirement: Required, Stability: Stable,
				Brief: "Mapped outcome (success|error|unknown)."},
			{Key: "ksquad.run.trace_id", Type: TypeString, Requirement: Recommended, Stability: Stable,
				Brief: "The run root's trace id, so the marker back-references the whole trace."},
		},
	},
	{
		Name:  SpanLLMCall,
		Brief: "One model round-trip (step). Carries OTel gen-AI semconv model + token attributes and the provider-reported cost; span duration is the truthful step duration.",
		Attributes: append(runIdentity(),
			Attribute{Key: "gen_ai.request.model", Type: TypeString, Requirement: Required, Stability: Stable,
				Brief: "The model requested for this step."},
			Attribute{Key: "gen_ai.usage.input_tokens", Type: TypeInt, Requirement: Required, Stability: Stable,
				Brief: "Prompt/input token count for the step."},
			Attribute{Key: "gen_ai.usage.output_tokens", Type: TypeInt, Requirement: Required, Stability: Stable,
				Brief: "Completion/output token count (reasoning currently folded in; WS-B splits it out)."},
			Attribute{Key: "gen_ai.usage.cache_read_tokens", Type: TypeInt, Requirement: Conditional, Stability: Stable,
				Brief: "Prompt-cache read tokens, when the runtime reports them."},
			Attribute{Key: "gen_ai.usage.cache_write_tokens", Type: TypeInt, Requirement: Conditional, Stability: Stable,
				Brief: "Prompt-cache write tokens, when the runtime reports them."},
			Attribute{Key: "ksquad.llm.cost.usd", Type: TypeDouble, Requirement: Conditional, Stability: Stable,
				Brief: "Provider-reported step cost in USD, when reported. Best-effort, not authoritative for billing."},
			// WS-B — complete the gen-AI semconv set + surface the backup model.
			Attribute{Key: "gen_ai.system", Type: TypeString, Requirement: Recommended, Stability: Planned, Workstream: "WS-B",
				Brief: "The gen-AI system/provider (e.g. anthropic, openai)."},
			Attribute{Key: "gen_ai.operation.name", Type: TypeString, Requirement: Recommended, Stability: Planned, Workstream: "WS-B",
				Brief: "The gen-AI operation (e.g. chat)."},
			Attribute{Key: "gen_ai.response.model", Type: TypeString, Requirement: Recommended, Stability: Planned, Workstream: "WS-B",
				Brief: "The model that actually served the response. Differs from gen_ai.request.model when the backup/fallback model served it."},
			Attribute{Key: "gen_ai.usage.reasoning_tokens", Type: TypeInt, Requirement: Conditional, Stability: Planned, Workstream: "WS-B",
				Brief: "Thinking-model reasoning tokens, split out from output_tokens."},
			Attribute{Key: "gen_ai.response.finish_reasons", Type: TypeStringSlice, Requirement: Conditional, Stability: Planned, Workstream: "WS-B",
				Brief: "Finish reasons the runtime reported for the step."},
			Attribute{Key: "gen_ai.response.id", Type: TypeString, Requirement: Conditional, Stability: Planned, Workstream: "WS-B",
				Brief: "Provider response id, when available."},
			Attribute{Key: "ksquad.llm.fallback", Type: TypeBool, Requirement: Conditional, Stability: Planned, Workstream: "WS-B",
				Brief: "True when the backup/fallback model served this step (correlate with ksquad_fallback_activations_total)."},
		),
	},
	{
		Name:  SpanToolCall,
		Brief: "A local/CLI tool call. Follows OTel gen-AI tool-call conventions; raw arguments NEVER travel — only a SHA-256 (PII posture).",
		Attributes: append(runIdentity(),
			Attribute{Key: "gen_ai.tool.name", Type: TypeString, Requirement: Required, Stability: Stable,
				Brief: "The tool invoked."},
			Attribute{Key: "gen_ai.tool.call.arguments", Type: TypeString, Requirement: Conditional, Stability: Stable,
				Brief: "Hex SHA-256 of the call arguments (the hash IS the argument surface; raw args never leave the process)."},
			Attribute{Key: "ksquad.skill.name", Type: TypeString, Requirement: Conditional, Stability: Stable,
				Brief: "The skill this tool call belongs to, when the call is skill-scoped."},
			Attribute{Key: "ksquad.outcome", Type: TypeString, Requirement: Required, Stability: Stable,
				Brief: "Call outcome on the result phase (success|error|unknown)."},
		),
	},
	{
		Name:  SpanMCPCall,
		Brief: "A tool call served by an MCPServer. Same shape as gen_ai.tool.call plus the serving MCP server.",
		Attributes: append(runIdentity(),
			Attribute{Key: "gen_ai.tool.name", Type: TypeString, Requirement: Required, Stability: Stable,
				Brief: "The tool invoked via MCP."},
			Attribute{Key: "gen_ai.tool.call.arguments", Type: TypeString, Requirement: Conditional, Stability: Stable,
				Brief: "Hex SHA-256 of the call arguments."},
			Attribute{Key: "ksquad.mcp.server", Type: TypeString, Requirement: Required, Stability: Stable,
				Brief: "The MCPServer that served the call."},
			Attribute{Key: "ksquad.outcome", Type: TypeString, Requirement: Required, Stability: Stable,
				Brief: "Call outcome on the result phase (success|error|unknown)."},
		),
	},
	{
		Name:  SpanSkillLoad,
		Brief: "A skill entering the runtime session.",
		Attributes: append(runIdentity(),
			Attribute{Key: "ksquad.skill.name", Type: TypeString, Requirement: Required, Stability: Stable,
				Brief: "The skill loaded."},
			Attribute{Key: "ksquad.skill.source.sha", Type: TypeString, Requirement: Conditional, Stability: Stable,
				Brief: "Pinned source SHA of the loaded skill, when known."},
			Attribute{Key: "ksquad.outcome", Type: TypeString, Requirement: Required, Stability: Stable,
				Brief: "Load outcome (success|error|unknown)."},
		),
	},
}

// EventConvention is the payload contract for one NATS domain lifecycle event.
// Entity + EventType are the outbox COLUMNS the relay composes the subject from
// (ksquad.{entity}.{project}.{squad}.{event_type}); PayloadFields document the
// versioned jsonb body.
type EventConvention struct {
	Entity        string
	EventType     string
	Stability     Stability
	Workstream    string
	Brief         string
	PayloadFields []Attribute
}

// lifecyclePayload is the identity + correlation set every ADR-0021 lifecycle
// event payload carries so a plugin gets full context AND the event spine
// correlates to the trace spine (D4).
func lifecyclePayload(extra ...Attribute) []Attribute {
	base := []Attribute{
		{Key: "trace_id", Type: TypeString, Requirement: Required, Stability: Planned, Workstream: "WS-D",
			Brief: "The run's W3C trace id — joins this event to the trace spine."},
		{Key: "run_id", Type: TypeString, Requirement: Required, Stability: Planned, Workstream: "WS-D",
			Brief: "The Run's id."},
		{Key: "agent", Type: TypeString, Requirement: Required, Stability: Planned, Workstream: "WS-D",
			Brief: "The agent name."},
		{Key: "team", Type: TypeString, Requirement: Recommended, Stability: Planned, Workstream: "WS-D",
			Brief: "The team/squad (may be empty)."},
		{Key: "project", Type: TypeString, Requirement: Required, Stability: Planned, Workstream: "WS-D",
			Brief: "The project id."},
		{Key: "work_item_ref", Type: TypeString, Requirement: Required, Stability: Planned, Workstream: "WS-D",
			Brief: "The work item / ticket ref."},
	}
	return append(base, extra...)
}

// EventConventions is the five ADR-0021 lifecycle events (WS-D). They do not
// exist on the bus yet; this registry is the contract WS-D lands against, and
// the events ride the existing run/work_item entities + subject taxonomy (no
// outbox CHECK-constraint change).
var EventConventions = []EventConvention{
	{
		Entity: "work_item", EventType: "work_item.assigned", Stability: Planned, Workstream: "WS-D",
		Brief:         "A ticket has been assigned to an agent/team (the run has an owner but no sandbox yet).",
		PayloadFields: lifecyclePayload(),
	},
	{
		Entity: "run", EventType: "run.scheduled", Stability: Planned, Workstream: "WS-D",
		Brief:         "The operator has scheduled the agent run.",
		PayloadFields: lifecyclePayload(),
	},
	{
		Entity: "run", EventType: "run.sandbox_bound", Stability: Planned, Workstream: "WS-D",
		Brief: "The run has been bound to a sandbox pod.",
		PayloadFields: lifecyclePayload(Attribute{Key: "sandbox_pod", Type: TypeString, Requirement: Required, Stability: Planned, Workstream: "WS-D",
			Brief: "The sandbox pod the run is bound to."}),
	},
	{
		Entity: "run", EventType: "run.started", Stability: Planned, Workstream: "WS-D",
		Brief: "The agent run has started executing in the sandbox.",
		PayloadFields: lifecyclePayload(Attribute{Key: "sandbox_pod", Type: TypeString, Requirement: Required, Stability: Planned, Workstream: "WS-D",
			Brief: "The sandbox pod executing the run."}),
	},
	{
		Entity: "run", EventType: "run.ended", Stability: Planned, Workstream: "WS-D",
		Brief: "The agent run has reached a terminal state.",
		PayloadFields: lifecyclePayload(Attribute{Key: "state", Type: TypeString, Requirement: Required, Stability: Planned, Workstream: "WS-D",
			Brief: "Terminal state (completed|failed|canceled)."}),
	},
}

// DocumentedSpanKeys returns the set of every span attribute key the registry
// documents (Stable or Planned). The conformance test uses it as the
// regression guard: any `ksquad.*`/`gen_ai.*` key the emitter produces that is
// NOT in this set is an undocumented leak and fails the test.
func DocumentedSpanKeys() map[string]bool {
	keys := map[string]bool{}
	for _, sc := range SpanConventions {
		for _, a := range sc.Attributes {
			keys[a.Key] = true
		}
	}
	return keys
}

// RequiredStable returns the keys the given span MUST carry today (Requirement
// Required AND Stability Stable). This is what the conformance test asserts.
func RequiredStable(spanName string) []string {
	var out []string
	for _, sc := range SpanConventions {
		if sc.Name != spanName {
			continue
		}
		for _, a := range sc.Attributes {
			if a.Requirement == Required && a.Stability == Stable {
				out = append(out, a.Key)
			}
		}
	}
	return out
}

// ConditionalStable returns the Stable keys that are present only when their
// source field is set. The conformance test drives fully-populated fixtures so
// these appear and asserts them too.
func ConditionalStable(spanName string) []string {
	var out []string
	for _, sc := range SpanConventions {
		if sc.Name != spanName {
			continue
		}
		for _, a := range sc.Attributes {
			if a.Requirement == Conditional && a.Stability == Stable {
				out = append(out, a.Key)
			}
		}
	}
	return out
}
