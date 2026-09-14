<!-- GENERATED FILE — DO NOT EDIT BY HAND.
     Source of truth: pkg/telemetry/semconv/registry.go
     Regenerate: go test ./pkg/telemetry/semconv/ -run TestDocInSync -update-semconv-docs -->

# K8squad telemetry semantic conventions

Canonical attribute + event schema for the K8squad run trace and the NATS
domain-event spine (ISI-4380 / ADR-0021 WS-F). K8squad domain attributes use
the `ksquad.*` namespace; LLM attributes follow the OpenTelemetry gen-AI
semantic conventions (`gen_ai.*`).

**Stability.** `stable` attributes are emitted by the code today and are
enforced by the conformance test (`pkg/telemetry/semconv/conformance_test.go`).
`planned` attributes are the specified contract for a not-yet-landed ADR-0021
workstream (the **WS** column names it); they are documented and
regression-guarded now, and the conformance test begins enforcing each one the
moment its workstream flips it to `stable`.

## Run-trace spans

The run trace is `run.start → llm.call / gen_ai.tool.call / mcp.call /
skill.load → run.end`. `run.start` is the root; every other span joins its
trace. Its trace id is the Run's `TraceID`.

### `run.start`

Root span of a run's trace: opens at dispatch, stays open for the run's lifetime; every other run-trace span joins it. Its trace id IS the run's TraceID.

| Attribute | Type | Requirement | Stability | WS | Description |
|---|---|---|---|---|---|
| `ksquad.run.id` | string | required | stable |  | The Run's stable id; the run trace's correlation key. High-cardinality — spans/events only, never a metric label. |
| `ksquad.agent.name` | string | required | stable |  | The agent executing the run (e.g. "coder"). |
| `ksquad.team.name` | string | required | planned | WS-A | The team/squad the run belongs to. Drill-down axis for the WS-F dashboard. |
| `ksquad.project.name` | string | required | planned | WS-A | The project the run belongs to. Drill-down axis. |
| `ksquad.work_item.ref` | string | required | planned | WS-A | The work item / ticket the run is servicing. Ticket→run→spans drill-down key. |
| `ksquad.sandbox.pod` | string | recommended | planned | WS-A | The sandbox pod hosting the run (data-plane locality). |
| `ksquad.run.state` | string | recommended | stable |  | Terminal state stamped onto the root at RunEnd (completed|failed|canceled|unknown). |
| `ksquad.outcome` | string | recommended | stable |  | Mapped outcome stamped at RunEnd (success|error|unknown). |

### `run.end`

Terminal marker child emitted at RunEnd; the run's end instant, queryable on its own.

| Attribute | Type | Requirement | Stability | WS | Description |
|---|---|---|---|---|---|
| `ksquad.run.state` | string | required | stable |  | Terminal a2a task state (completed|failed|canceled|unknown). |
| `ksquad.outcome` | string | required | stable |  | Mapped outcome (success|error|unknown). |
| `ksquad.run.trace_id` | string | recommended | stable |  | The run root's trace id, so the marker back-references the whole trace. |

### `llm.call`

One model round-trip (step). Carries OTel gen-AI semconv model + token attributes and the provider-reported cost; span duration is the truthful step duration.

| Attribute | Type | Requirement | Stability | WS | Description |
|---|---|---|---|---|---|
| `ksquad.run.id` | string | required | stable |  | The Run's stable id; the run trace's correlation key. High-cardinality — spans/events only, never a metric label. |
| `ksquad.agent.name` | string | required | stable |  | The agent executing the run (e.g. "coder"). |
| `ksquad.team.name` | string | required | planned | WS-A | The team/squad the run belongs to. Drill-down axis for the WS-F dashboard. |
| `ksquad.project.name` | string | required | planned | WS-A | The project the run belongs to. Drill-down axis. |
| `ksquad.work_item.ref` | string | required | planned | WS-A | The work item / ticket the run is servicing. Ticket→run→spans drill-down key. |
| `ksquad.sandbox.pod` | string | recommended | planned | WS-A | The sandbox pod hosting the run (data-plane locality). |
| `gen_ai.request.model` | string | required | stable |  | The model requested for this step. |
| `gen_ai.usage.input_tokens` | int | required | stable |  | Prompt/input token count for the step. |
| `gen_ai.usage.output_tokens` | int | required | stable |  | Completion/output token count (reasoning currently folded in; WS-B splits it out). |
| `gen_ai.usage.cache_read_tokens` | int | conditional | stable |  | Prompt-cache read tokens, when the runtime reports them. |
| `gen_ai.usage.cache_write_tokens` | int | conditional | stable |  | Prompt-cache write tokens, when the runtime reports them. |
| `ksquad.llm.cost.usd` | double | conditional | stable |  | Provider-reported step cost in USD, when reported. Best-effort, not authoritative for billing. |
| `gen_ai.system` | string | recommended | planned | WS-B | The gen-AI system/provider (e.g. anthropic, openai). |
| `gen_ai.operation.name` | string | recommended | planned | WS-B | The gen-AI operation (e.g. chat). |
| `gen_ai.response.model` | string | recommended | planned | WS-B | The model that actually served the response. Differs from gen_ai.request.model when the backup/fallback model served it. |
| `gen_ai.usage.reasoning_tokens` | int | conditional | planned | WS-B | Thinking-model reasoning tokens, split out from output_tokens. |
| `gen_ai.response.finish_reasons` | string[] | conditional | planned | WS-B | Finish reasons the runtime reported for the step. |
| `gen_ai.response.id` | string | conditional | planned | WS-B | Provider response id, when available. |
| `ksquad.llm.fallback` | boolean | conditional | planned | WS-B | True when the backup/fallback model served this step (correlate with ksquad_fallback_activations_total). |

### `gen_ai.tool.call`

A local/CLI tool call. Follows OTel gen-AI tool-call conventions; raw arguments NEVER travel — only a SHA-256 (PII posture).

| Attribute | Type | Requirement | Stability | WS | Description |
|---|---|---|---|---|---|
| `ksquad.run.id` | string | required | stable |  | The Run's stable id; the run trace's correlation key. High-cardinality — spans/events only, never a metric label. |
| `ksquad.agent.name` | string | required | stable |  | The agent executing the run (e.g. "coder"). |
| `ksquad.team.name` | string | required | planned | WS-A | The team/squad the run belongs to. Drill-down axis for the WS-F dashboard. |
| `ksquad.project.name` | string | required | planned | WS-A | The project the run belongs to. Drill-down axis. |
| `ksquad.work_item.ref` | string | required | planned | WS-A | The work item / ticket the run is servicing. Ticket→run→spans drill-down key. |
| `ksquad.sandbox.pod` | string | recommended | planned | WS-A | The sandbox pod hosting the run (data-plane locality). |
| `gen_ai.tool.name` | string | required | stable |  | The tool invoked. |
| `gen_ai.tool.call.arguments` | string | conditional | stable |  | Hex SHA-256 of the call arguments (the hash IS the argument surface; raw args never leave the process). |
| `ksquad.skill.name` | string | conditional | stable |  | The skill this tool call belongs to, when the call is skill-scoped. |
| `ksquad.outcome` | string | required | stable |  | Call outcome on the result phase (success|error|unknown). |
| `ksquad.duration.ms` | int | required | stable | WS-C | Wall-clock call duration in ms, measured start→result (ISI-4385). |

### `mcp.call`

A tool call served by an MCPServer. Same shape as gen_ai.tool.call plus the serving MCP server.

| Attribute | Type | Requirement | Stability | WS | Description |
|---|---|---|---|---|---|
| `ksquad.run.id` | string | required | stable |  | The Run's stable id; the run trace's correlation key. High-cardinality — spans/events only, never a metric label. |
| `ksquad.agent.name` | string | required | stable |  | The agent executing the run (e.g. "coder"). |
| `ksquad.team.name` | string | required | planned | WS-A | The team/squad the run belongs to. Drill-down axis for the WS-F dashboard. |
| `ksquad.project.name` | string | required | planned | WS-A | The project the run belongs to. Drill-down axis. |
| `ksquad.work_item.ref` | string | required | planned | WS-A | The work item / ticket the run is servicing. Ticket→run→spans drill-down key. |
| `ksquad.sandbox.pod` | string | recommended | planned | WS-A | The sandbox pod hosting the run (data-plane locality). |
| `gen_ai.tool.name` | string | required | stable |  | The tool invoked via MCP. |
| `gen_ai.tool.call.arguments` | string | conditional | stable |  | Hex SHA-256 of the call arguments. |
| `ksquad.mcp.server` | string | required | stable |  | The MCPServer that served the call. |
| `ksquad.outcome` | string | required | stable |  | Call outcome on the result phase (success|error|unknown). |
| `ksquad.duration.ms` | int | required | stable | WS-C | Wall-clock call duration in ms, measured start→result (ISI-4385). |

### `skill.load`

A skill entering the runtime session.

| Attribute | Type | Requirement | Stability | WS | Description |
|---|---|---|---|---|---|
| `ksquad.run.id` | string | required | stable |  | The Run's stable id; the run trace's correlation key. High-cardinality — spans/events only, never a metric label. |
| `ksquad.agent.name` | string | required | stable |  | The agent executing the run (e.g. "coder"). |
| `ksquad.team.name` | string | required | planned | WS-A | The team/squad the run belongs to. Drill-down axis for the WS-F dashboard. |
| `ksquad.project.name` | string | required | planned | WS-A | The project the run belongs to. Drill-down axis. |
| `ksquad.work_item.ref` | string | required | planned | WS-A | The work item / ticket the run is servicing. Ticket→run→spans drill-down key. |
| `ksquad.sandbox.pod` | string | recommended | planned | WS-A | The sandbox pod hosting the run (data-plane locality). |
| `ksquad.skill.name` | string | required | stable |  | The skill loaded. |
| `ksquad.skill.source.sha` | string | conditional | stable |  | Pinned source SHA of the loaded skill, when known. |
| `ksquad.outcome` | string | required | stable |  | Load outcome (success|error|unknown). |
| `ksquad.duration.ms` | int | required | stable | WS-C | Stamped for uniformity across activity spans; skill.load is a point event so this is the mapping instant (ISI-4385). |

## NATS domain lifecycle events

Events ride the existing outbox→relay→JetStream spine. The relay composes the
subject `ksquad.{entity}.{project}.{squad}.{event_type}` from the outbox
columns (never from the payload). Each payload is a versioned JSON body carrying
the identity + `trace_id` correlation set so a subscribing plugin has full
context and the event spine joins the trace spine.

| Event type | Entity | WS | Description |
|---|---|---|---|
| `work_item.assigned` | `work_item` | WS-D | A ticket has been assigned to an agent/team (the run has an owner but no sandbox yet). |
| `run.scheduled` | `run` | WS-D | The operator has scheduled the agent run. |
| `run.sandbox_bound` | `run` | WS-D | The run has been bound to a sandbox pod. |
| `run.started` | `run` | WS-D | The agent run has started executing in the sandbox. |
| `run.ended` | `run` | WS-D | The agent run has reached a terminal state. |

### `work_item.assigned` payload

| Attribute | Type | Requirement | Stability | WS | Description |
|---|---|---|---|---|---|
| `trace_id` | string | required | planned | WS-D | The run's W3C trace id — joins this event to the trace spine. |
| `run_id` | string | required | planned | WS-D | The Run's id. |
| `agent` | string | required | planned | WS-D | The agent name. |
| `team` | string | recommended | planned | WS-D | The team/squad (may be empty). |
| `project` | string | required | planned | WS-D | The project id. |
| `work_item_ref` | string | required | planned | WS-D | The work item / ticket ref. |

### `run.scheduled` payload

| Attribute | Type | Requirement | Stability | WS | Description |
|---|---|---|---|---|---|
| `trace_id` | string | required | planned | WS-D | The run's W3C trace id — joins this event to the trace spine. |
| `run_id` | string | required | planned | WS-D | The Run's id. |
| `agent` | string | required | planned | WS-D | The agent name. |
| `team` | string | recommended | planned | WS-D | The team/squad (may be empty). |
| `project` | string | required | planned | WS-D | The project id. |
| `work_item_ref` | string | required | planned | WS-D | The work item / ticket ref. |

### `run.sandbox_bound` payload

| Attribute | Type | Requirement | Stability | WS | Description |
|---|---|---|---|---|---|
| `trace_id` | string | required | planned | WS-D | The run's W3C trace id — joins this event to the trace spine. |
| `run_id` | string | required | planned | WS-D | The Run's id. |
| `agent` | string | required | planned | WS-D | The agent name. |
| `team` | string | recommended | planned | WS-D | The team/squad (may be empty). |
| `project` | string | required | planned | WS-D | The project id. |
| `work_item_ref` | string | required | planned | WS-D | The work item / ticket ref. |
| `sandbox_pod` | string | required | planned | WS-D | The sandbox pod the run is bound to. |

### `run.started` payload

| Attribute | Type | Requirement | Stability | WS | Description |
|---|---|---|---|---|---|
| `trace_id` | string | required | planned | WS-D | The run's W3C trace id — joins this event to the trace spine. |
| `run_id` | string | required | planned | WS-D | The Run's id. |
| `agent` | string | required | planned | WS-D | The agent name. |
| `team` | string | recommended | planned | WS-D | The team/squad (may be empty). |
| `project` | string | required | planned | WS-D | The project id. |
| `work_item_ref` | string | required | planned | WS-D | The work item / ticket ref. |
| `sandbox_pod` | string | required | planned | WS-D | The sandbox pod executing the run. |

### `run.ended` payload

| Attribute | Type | Requirement | Stability | WS | Description |
|---|---|---|---|---|---|
| `trace_id` | string | required | planned | WS-D | The run's W3C trace id — joins this event to the trace spine. |
| `run_id` | string | required | planned | WS-D | The Run's id. |
| `agent` | string | required | planned | WS-D | The agent name. |
| `team` | string | recommended | planned | WS-D | The team/squad (may be empty). |
| `project` | string | required | planned | WS-D | The project id. |
| `work_item_ref` | string | required | planned | WS-D | The work item / ticket ref. |
| `state` | string | required | planned | WS-D | Terminal state (completed|failed|canceled). |

