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
| `ksquad.model.tier` | string | recommended | stable |  | Which Model-Per-Role tier supplied the run's effective model at dispatch (agent|role|default) — the resolved origin so an operator sees WHY the run used its model, without re-deriving the tier walk (ISI-4430 S5). Empty on runtime-default runs. Run root only; the per-step serving model of a fallback rides gen_ai.response.model + ksquad.llm.fallback on llm.call. |

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
| `ksquad.step.index` | int | required | stable |  | The run's 1-based model-turn counter: a model round-trip increments it, and every gen_ai.tool.call / mcp.call span emitted before the next round-trip shares it, so a backend can group a turn's model call with the tool calls it triggered (ISI-4970, GH #636). |
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
| `gen_ai.operation.name` | string | required | stable |  | The gen-AI operation; always "execute_tool" on a tool-execution span (ISI-4970, GH #636). |
| `ksquad.tool.type` | string | required | stable | WS-C | Tool category (bash|git|docker|kubectl|helm|node|python|mcp|system) so traces group by tool kind (ISI-4540). |
| `gen_ai.tool.call.arguments` | string | conditional | stable |  | Hex SHA-256 of the call arguments (the hash IS the argument surface; raw args never leave the process). |
| `ksquad.skill.name` | string | conditional | stable |  | The skill this tool call belongs to, when the call is skill-scoped. |
| `ksquad.step.index` | int | required | stable |  | The model-turn index this tool call belongs to — shares the value of the llm.call that requested it (ISI-4970, GH #636). |
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
| `gen_ai.operation.name` | string | required | stable |  | The gen-AI operation; always "execute_tool" on a tool-execution span (ISI-4970, GH #636). |
| `ksquad.tool.type` | string | required | stable | WS-C | Tool category; always "mcp" on this span (ISI-4540). |
| `gen_ai.tool.call.arguments` | string | conditional | stable |  | Hex SHA-256 of the call arguments. |
| `ksquad.mcp.server` | string | required | stable |  | The MCPServer that served the call. |
| `ksquad.step.index` | int | required | stable |  | The model-turn index this MCP call belongs to — shares the value of the llm.call that requested it (ISI-4970, GH #636). |
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

## GitHub-sync (SCM) telemetry

The GitHub sync path (webhook ingress → operator reposync → GitHub-status tab)
emits its own span family and metric set (ISI-4395 / WS-GH) so a dropped
webhook, a recovered panic, a provider error and a stale mirror are
distinguishable from outside. These spans are emitted by the scm-webhook,
operator and apiserver processes (not the run-trace mapper) and are guarded by
`pkg/telemetry/semconv/scm_conformance_test.go`. The webhook payload is never
persisted and never travels on span attributes.

### SCM spans

#### `scm.webhook.receive`

One inbound SCM webhook delivery at the scm-webhook ingress (GH-1). Opens the sync trace; a W3C traceparent is injected into the trigger-annotation patch so the operator's scm.sync joins it. Webhook payload is never persisted and never travels on span attributes (PII posture).

| Attribute | Type | Requirement | Stability | WS | Description |
|---|---|---|---|---|---|
| `scm.webhook.event` | string | required | stable | WS-GH | The webhook event type (e.g. push, pull_request); "unknown" when absent. |
| `scm.webhook.outcome` | string | required | stable | WS-GH | Delivery outcome (accepted|rejected|error). rejected is the uniform-401 verify gate; error is a processing failure. |
| `scm.project` | string | conditional | stable | WS-GH | The Project the delivery maps to, when resolved (bare scm.* namespace — see NAMESPACE NOTE). |
| `scm.namespace` | string | conditional | stable | WS-GH | The Project's namespace, when resolved. |
| `scm.provider` | string | conditional | stable | WS-GH | The SCM provider the trigger is forwarded to, when resolved. |

#### `scm.sync`

One operator reposync Reconcile / mirror pass (GH-2). Joins the inbound webhook trace via the traceparent annotation. A deferred recover records the error and increments ksquad.scm.sync.panics.total before re-panicking, so a nil Client/Providers/Store deref is visible rather than silent.

| Attribute | Type | Requirement | Stability | WS | Description |
|---|---|---|---|---|---|
| `ksquad.scm.provider` | string | required | stable | WS-GH | The SCM provider driving the pass (e.g. github). |
| `ksquad.scm.trigger` | string | required | stable | WS-GH | What kicked the pass (webhook|poll) — proves whether a refresh was event-driven or the scheduled requeue. |
| `ksquad.scm.reason` | string | conditional | stable | WS-GH | The reconcile reason/condition (Synced|ProviderError|MirrorWriteError|CredentialMissing|…); the SyncReady condition vocabulary. err.Error() on the CredentialMissing path never embeds the BYO token. |
| `ksquad.scm.mirror.record_count` | int | conditional | stable | WS-GH | Total mirror records upserted this pass, when the pass reached the write phase. |

#### `scm.fetch.<kind>`

A child of scm.sync per entity kind (GH-2): scm.fetch.pull_requests, scm.fetch.issues, scm.fetch.check_runs, scm.fetch.artifacts, scm.fetch.releases, scm.fetch.branches. Isolates which provider fetch is slow or failing. Carries the outbound provider call's HTTP client semantics (method, path, server.address, response status) so RED analysis can group per route/status per SCM call (ISI-5013).

| Attribute | Type | Requirement | Stability | WS | Description |
|---|---|---|---|---|---|
| `ksquad.scm.record_count` | int | required | stable | WS-GH | Number of records the per-kind fetcher returned. |
| `http.request.method` | string | required | stable | WS-GH | The outbound provider API method (e.g. GET) stamped on the span by the provider HTTP transport. |
| `url.path` | string | required | stable | WS-GH | The outbound provider API path (e.g. /repos/{owner}/{repo}/issues), stamped from the request URL. |
| `server.address` | string | required | stable | WS-GH | The provider API host (e.g. api.github.com) the outbound call targeted. |
| `http.response.status_code` | int | required | stable | WS-GH | The provider API response status code for the outbound call. |

#### `scm.sync.trigger`

The apiserver manual "Sync now" path (GH-4). Carries trigger=manual so a dashboard can PROVE a mirror refresh was an operator kick, not the automatic webhook/poll pipeline.

| Attribute | Type | Requirement | Stability | WS | Description |
|---|---|---|---|---|---|
| `ksquad.scm.trigger` | string | required | stable | WS-GH | Always "manual" for this span. |
| `ksquad.scm.project` | string | required | stable | WS-GH | namespace/name of the Project whose mirror was kicked. |

#### `scm.status.read`

The apiserver GET /api/projects/{id}/github server span, enriched in place (GH-4) with domain attrs + the freshness SLI so the GitHub-status tab read carries a mirror-age signal rather than being a blind spot.

| Attribute | Type | Requirement | Stability | WS | Description |
|---|---|---|---|---|---|
| `ksquad.scm.project` | string | required | stable | WS-GH | namespace/name of the Project being read. |
| `ksquad.scm.repo_url` | string | recommended | stable | WS-GH | The mirrored repository URL (never the credential). |
| `ksquad.scm.mirror.age_seconds` | double | conditional | stable | WS-GH | Freshness SLI: now − Project.status.sync.lastSuccess, present when a successful mirror exists. |
| `ksquad.scm.result.pull_requests` | int | recommended | stable | WS-GH | Pull-request records served from the mirror. |
| `ksquad.scm.result.issues` | int | recommended | stable | WS-GH | Issue records served from the mirror. |
| `ksquad.scm.result.check_runs` | int | recommended | stable | WS-GH | Check-run records served from the mirror. |
| `ksquad.scm.result.artifacts` | int | recommended | stable | WS-GH | Artifact records served from the mirror. |
| `ksquad.scm.result.releases` | int | recommended | stable | WS-GH | Release records served from the mirror. |
| `ksquad.scm.result.branches` | int | recommended | stable | WS-GH | Branch records served from the mirror. |

### SCM metrics

Operator metrics on `telemetry.Meter()`. Names are the OTel (dotted) instrument
names; the Prometheus exporter renders them with underscores
(`ksquad.scm.webhook.total` → `ksquad_scm_webhook_total`). Labels never include
`repo` or `run.id` (cardinality).

| Metric | Instrument | Unit | Labels | Stability | WS | Description |
|---|---|---|---|---|---|---|
| `ksquad.scm.webhook.total` | counter | 1 | `event`, `outcome` | stable | WS-GH | SCM webhook deliveries by event and outcome (GH-1). outcome=accepted proves ingress is alive; a dropped webhook stops incrementing. |
| `ksquad.scm.sync.total` | counter | 1 | `provider`, `trigger`, `reason` | stable | WS-GH | SCM reconcile passes by provider, trigger and reason (GH-3). The reason label reuses the SyncReady condition taxonomy so success rate = Synced / total. |
| `ksquad.scm.sync.duration` | histogram | s | `provider`, `trigger` | stable | WS-GH | SCM reconcile pass latency in seconds (GH-3). Freshness SLO source: p99 should stay under pollInterval+60s. |
| `ksquad.scm.sync.panics.total` | counter | 1 | `provider` | stable | WS-GH | SCM reconcile panics recovered and re-raised (GH-2). The nil Client/Providers/Store deref SLO: this must stay 0. |
| `ksquad.scm.mirror.age` | observable_gauge | s | `project` | stable | WS-GH | Seconds since each Project's last successful mirror pass (GH-3). A stalled reconcile shows an ever-growing age rather than dropping off the series. |
| `ksquad.scm.provider.rate_limit.remaining` | observable_gauge | 1 | `provider` | stable | WS-GH | Last-seen provider rate-limit headroom (requests remaining) by provider (GH-3). Approaching 0 explains stale mirrors that are not errors. |

