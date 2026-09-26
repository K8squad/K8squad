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

// Package a2a is the internal stable A2A southbound interface the K8squad
// core speaks to every agent-runtime shim (agent-shim-interface-spec §3). It
// defines the six MUST-verbs (V1 SubmitTask, V2 StreamEvents, V3 GetStatus,
// V4 CancelTask, V6 GetAgentCard — V5 EmitArtifact is shim-initiated and
// lives on the coord client), the task state machine (§3.1), the SSE event
// schema (§4), and the Agent Card schema (§6.1).
//
// This package is transport + type only: it is deliberately runtime-agnostic
// so upstream A2A wire churn is absorbed at the adapter seam (§8, R11) and the
// pinned protocol revisions are advertised from internal/protocol, never
// inlined here.
package a2a

import (
	"context"
	"path"
	"strings"
	"time"

	"github.com/K8squad/K8squad/internal/protocol"
	"github.com/K8squad/K8squad/pkg/capability"
)

// SchemaVersion is the Agent Card schema tag (spec §6.1). It is versioned
// independently of the pinned A2A wire revision (internal/protocol.A2AVersion).
const SchemaVersion = "ksquad.a2a/v1"

// TaskState is one node of the A2A task state machine (spec §3.1). It is
// distinct from the Run/work-item lifecycle: it tracks a single agent task
// keyed on the deterministic a2a_task_id (== run_id).
type TaskState string

// Task states (spec §3.1). completed/failed/canceled are terminal; a
// re-SubmitTask on a terminal task returns the terminal status (dedup) and
// does not restart the runtime.
const (
	// TaskSubmitted is the initial state before the runtime is driven.
	TaskSubmitted TaskState = "submitted"
	// TaskWorking is the active-execution state.
	TaskWorking TaskState = "working"
	// TaskInputRequired is reachable ONLY when the Agent Card advertises
	// capabilities.interactivePrompt (spec §3.1, C6). A runtime without it
	// MUST NOT reach this state.
	TaskInputRequired TaskState = "input-required"
	// TaskAuthRequired is a first-class pause signal (spec §7/§11), NOT a
	// failure: it maps to the Run→Paused path, not to TaskFailed (C7).
	TaskAuthRequired TaskState = "auth-required"
	// TaskCompleted is the terminal success state.
	TaskCompleted TaskState = "completed"
	// TaskFailed is the terminal generic-failure state.
	TaskFailed TaskState = "failed"
	// TaskCanceled is the terminal state reached via V4 CancelTask.
	TaskCanceled TaskState = "canceled"
)

// IsTerminal reports whether s is a terminal task state (spec §3.1).
func (s TaskState) IsTerminal() bool {
	switch s {
	case TaskCompleted, TaskFailed, TaskCanceled:
		return true
	default:
		return false
	}
}

// Envelope is the context envelope the control plane assembles (spec §8.5);
// the shim only transports it as the task's system/context input, it does not
// assemble it.
type Envelope struct {
	// SystemContext is the assembled system/context prompt driven into the
	// runtime as the initial message (spec §3 V1).
	SystemContext string `json:"systemContext,omitempty"`
	// Input is the concrete work instruction for this Run.
	Input string `json:"input,omitempty"`
	// Metadata is opaque pass-through context; the shim never interprets it.
	Metadata map[string]string `json:"metadata,omitempty"`
}

// ModelRoute is the resolved model-provider seam (spec §11, byoModelEndpoint).
// For a fixed-vendor runtime it is empty; for a byoModelEndpoint runtime it
// carries the OpenAI-compatible base URL (e.g. an Ollama endpoint) and model.
type ModelRoute struct {
	// Endpoint is the OpenAI-compatible base URL (e.g. http://ollama:11434/v1).
	Endpoint string `json:"endpoint,omitempty"`
	// Model is the model id/name served at Endpoint.
	Model string `json:"model,omitempty"`
	// Token is an optional bearer for the endpoint; empty for Ollama (the
	// zero-credential CI lane, spec §11/C9).
	Token string `json:"token,omitempty"`
}

// Task is the V1 SubmitTask payload (spec §3). The id is deterministic
// (== run_id): a second submit with the same id reattaches (C1).
type Task struct {
	// A2ATaskID is the deterministic task id, equal to the Run id (spec §3 V1).
	A2ATaskID string `json:"a2a_task_id"`
	// WorkItemID is the item the Run holds; carried onto emitted artifacts (§5).
	WorkItemID string `json:"work_item_id"`
	// FenceToken is the Run's current fence (spec §6.2); attached to every
	// artifact write and rejected by the coord API if stale (C3).
	FenceToken string `json:"fence_token"`
	// Envelope is the transported context envelope (spec §8.5).
	Envelope Envelope `json:"envelope"`
	// CredentialsMounted reflects that the reconciler env-injected the
	// credential Secret into the runtime container (spec §7). The shim never
	// reads the raw secret; it only observes this flag and the auth shape.
	CredentialsMounted bool `json:"credentials_ref_mounted"`
	// ModelRoute is the resolved model-provider route (spec §11).
	ModelRoute ModelRoute `json:"model_route"`
	// Identity is the per-run agent identity (name/squad/project) the operator
	// resolves at dispatch from the Run CR (ISI-4439). It rides the submit
	// payload because a warm-pool sandbox pod boots GENERIC — its env carries
	// no KSQUAD_AGENT_NAME/SQUAD/PROJECT (the pod is provisioned before a run is
	// assigned), so the shim's process-static launch Identity is empty on that
	// path and the run/llm/tool spans would otherwise omit agent/team/project.
	// The shim prefers this per-task identity over its launch config when set;
	// the operator-spawned stdio path leaves it empty and keeps using the env.
	// +optional
	Identity AgentIdentity `json:"identity,omitempty"`
	// ModelTier names which Model-Per-Role tier supplied the run's effective
	// model at dispatch — "agent" (Agent.spec.model), "role" (Role.spec.model),
	// or "default" (the system-default ModelConfig singleton) — the resolved
	// origin the operator computes alongside the model endpoint (ISI-4430 S4).
	// It rides the submit payload so the shim can stamp it as ksquad.model.tier
	// on the run.start span (ISI-4430 S5), letting an operator see WHY a run
	// used a given model directly on its trace, without re-deriving the tier
	// walk. Empty when no agents resolve a tier (e.g. a runtime-default run).
	// +optional
	ModelTier string `json:"model_tier,omitempty"`
	// MCPEndpoints is the Run's resolved MCP IR delivered on the submit
	// payload (Epic C, ADR-044 step 6; ISI-5017). A warm-pool sandbox pod
	// boots GENERIC and is immutable after Bind — its volumes/env cannot be
	// extended per run — so the IR rides the task envelope exactly the way
	// per-run Identity does, and the shim renders the runtime's native MCP
	// config from it per task. It is the IR CONTENT (what K8SQUAD_MCP_CONFIG
	// would name), never the ConfigMap name. Empty when the Run demanded no
	// MCP servers (or on the operator-spawned stdio path, where the shim
	// instead reads K8SQUAD_MCP_CONFIG from its env). +optional
	MCPEndpoints []capability.Endpoint `json:"mcp_endpoints,omitempty"`
	// MCPTokenEnv carries the resolved MCP credential VALUES keyed by the
	// env var NAME the IR's endpoints reference (capability.CredentialEnvName
	// — e.g. KSQUAD_MCP_KSQUAD_MEMORY_AUTHORING_TOKEN). The runtime's rendered
	// native config references the NAME (Bearer {env:NAME}); a warm-pool pod
	// cannot gain the SecretKeyRef env at Bind time, so the operator resolves
	// the per-run Secret at dispatch and ships the value here, and the shim
	// layers it onto the runtime subprocess env. Values are scrubbed from
	// logs/telemetry; this map is secret material and MUST NOT be logged.
	// +optional
	MCPTokenEnv map[string]string `json:"mcp_token_env,omitempty"`
}

// Status is the V3 GetStatus result (spec §3 V3): the current task state, an
// optional human reason, and the last SSE seq delivered (resume anchor, C4).
type Status struct {
	State   TaskState `json:"state"`
	Reason  string    `json:"reason,omitempty"`
	LastSeq uint64    `json:"lastSeq"`
	// TraceID is the run's root OTel trace id (ISI-4238): stamped from the
	// shim's run span when telemetry is attached, so the core can project
	// it onto Run.Status.TraceID and a failing run's spans/logs/metrics
	// are correlatable end-to-end. Empty when telemetry is off. +optional
	TraceID string `json:"traceID,omitempty"`
}

// EventType enumerates the SSE event types (spec §4).
type EventType string

// SSE event types (spec §4).
const (
	// EventStatus mirrors the §3.1 state machine.
	EventStatus EventType = "status"
	// EventMessage is untrusted agent progress text (F16) — display only.
	EventMessage EventType = "message"
	// EventTool is a tool-call start/result activity event.
	EventTool EventType = "tool"
	// EventSkillLoad is a skill-load activity event (Epic D, plan §2.4):
	// the runtime/shim reports a skill being loaded into the session so the
	// telemetry spine can turn it into a skill.load span + counter. Payload
	// is SkillLoadPayload.
	EventSkillLoad EventType = "skill-load"
	// EventArtifactRef points to a §5 artifact already committed to coord.
	EventArtifactRef EventType = "artifact-ref"
	// EventUsage is best-effort token counts for metering (spec §11).
	EventUsage EventType = "usage"
	// EventAuthRequired is the auth-failure pause signal (spec §7/§11, C7).
	EventAuthRequired EventType = "auth-required"
	// EventRateLimited is the standardized rate_limited signal (story 5.10,
	// gap ISI-2894): the runtime's model provider returned a 429. Unlike
	// EventAuthRequired it is NOT a pause state — it is a progress signal the
	// core normalizes (modelendpoint.NormalizeRateLimited) and hands to the
	// 5.11 decision core (SwitchModel) or the 2.11 durable resume (PauseRun).
	// The task keeps running/failing on its own terms; the signal only tells
	// the core WHY and, via Retry-After, HOW LONG the provider asked us to wait.
	EventRateLimited EventType = "rate-limited"
)

// Event is one SSE progress event (spec §4). seq is monotonic + gap-free per
// task and is the resume/ordering key (C4); the core dedups on
// (a2a_task_id, seq) under at-least-once delivery.
type Event struct {
	Seq       uint64    `json:"seq"`
	A2ATaskID string    `json:"a2a_task_id"`
	TS        time.Time `json:"ts"`
	Type      EventType `json:"type"`
	Payload   any       `json:"payload"`
}

// StatusPayload is the payload of an EventStatus event (spec §4).
type StatusPayload struct {
	State  TaskState `json:"state"`
	Reason string    `json:"reason,omitempty"`
	// TraceID is the run's root OTel trace id (ISI-4238), stamped onto
	// every status event once the run span opened: the run-drive consumer
	// projects it onto Run.Status.TraceID the moment the first status
	// event carrying it lands, so a failing run is debuggable end-to-end
	// WHILE it runs, not only post-terminal. Empty when telemetry is off.
	// +optional
	TraceID string `json:"traceID,omitempty"`
}

// MessagePayload is the payload of an EventMessage event (spec §4). Trust is
// always "untrusted": agent text is never executed, only displayed (F16).
type MessagePayload struct {
	Role  string `json:"role"`
	Text  string `json:"text"`
	Trust string `json:"trust"`
}

// ToolPayload is the payload of an EventTool event (spec §4). Phase is
// "start" or "result".
type ToolPayload struct {
	Name  string `json:"name"`
	Phase string `json:"phase"`
	// OK is tri-state on the wire (Epic D): true = success, false = error,
	// absent = the emitter could not tell (mapped to outcome "unknown" by
	// the telemetry spine — never guessed, D1 AC).
	OK      *bool  `json:"ok,omitempty"`
	Summary string `json:"summary,omitempty"`
	// ArgsSHA256 is the hex SHA-256 of the tool-call arguments, computed by
	// the emitter BEFORE the event leaves the process (Epic D, plan §2.4:
	// args are hashed, never transported raw — they may carry secrets).
	// +optional
	ArgsSHA256 string `json:"argsSHA256,omitempty"`
	// Skill attributes the call to the skill that requested it, when known
	// (labels ksquad_tool_calls_total{skill}). +optional
	Skill string `json:"skill,omitempty"`
	// Server names the MCP server serving this tool when the call rode an
	// MCPServer (Epic A/C); empty for local/CLI tool calls. A set Server
	// makes the telemetry mapping emit an mcp.call span instead of
	// gen_ai.tool.call and observe the MCP duration histogram. +optional
	Server string `json:"server,omitempty"`
	// Command is the invoked executable's head token for shell-family tools
	// (bash/sh), extracted IN-PROCESS from the raw command before args are
	// hashed (ISI-4720). Runtimes model git/npm/kubectl/… as arguments to a
	// single "bash" tool, so without this every git or kubectl call surfaces
	// only as an opaque "bash" span; Command lets the telemetry spine
	// categorize a bash-wrapped call by what it actually ran. It carries ONLY
	// the executable name (basename, env-assignment prefixes stripped), never
	// arguments — the low-PII, low-cardinality head token, not the command
	// line. Empty for MCP/file/native tool calls (their Name is already
	// distinct) and for shell calls whose head token is not a recognized
	// tool. +optional
	Command string `json:"command,omitempty"`
}

// recognizedShellHeads is the bounded allowlist of executable head tokens a
// shell-family tool call is enriched with (ISI-4720). It intentionally mirrors
// the head tokens the telemetry spine categorizes
// (pkg/telemetry/toolusage.categorizeTool): only these become a
// ToolPayload.Command, so the value is always low-cardinality and PII-safe (a
// known tool name, never an arbitrary script path or argument). Drift in
// either list is caught by both packages' tests.
var recognizedShellHeads = map[string]struct{}{
	"git": {}, "docker": {}, "kubectl": {}, "helm": {},
	"npm": {}, "node": {}, "pip": {}, "python": {}, "python3": {},
}

// ShellCommandHead extracts the invoked executable's head token from a raw
// shell command string (ISI-4720): it strips any leading `VAR=value`
// environment-assignment prefixes, takes the first remaining whitespace-split
// token, reduces it to its basename, and returns it ONLY when it is a
// recognized tool (recognizedShellHeads) — otherwise "". It parses the head
// token alone and never returns arguments, so a bash-wrapped `git`/`kubectl`
// call can be categorized by what it ran without transporting the (possibly
// secret) command line. Extraction happens in-process at the shim before the
// raw args are hashed away.
func ShellCommandHead(command string) string {
	fields := strings.Fields(command)
	i := 0
	// Skip leading environment-assignment prefixes ("FOO=bar cmd ..."): a token
	// is an assignment only up to the first non-assignment token.
	for i < len(fields) && isEnvAssignment(fields[i]) {
		i++
	}
	if i >= len(fields) {
		return ""
	}
	head := path.Base(fields[i])
	if _, ok := recognizedShellHeads[head]; ok {
		return head
	}
	return ""
}

// isEnvAssignment reports whether a shell token is a `NAME=value`
// environment-assignment prefix (a `=` that follows a non-empty run of
// identifier characters, i.e. not a flag like `-x` and not a bare `=value`).
func isEnvAssignment(tok string) bool {
	eq := strings.IndexByte(tok, '=')
	if eq <= 0 {
		return false
	}
	for _, r := range tok[:eq] {
		if !isIdentChar(r) {
			return false
		}
	}
	return true
}

// isIdentChar reports whether r is a shell identifier character
// ([A-Za-z0-9_]) — the character class a `NAME=value` prefix's name is made of.
func isIdentChar(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// SkillLoadPayload is the payload of an EventSkillLoad event (Epic D, plan
// §2.4). It reports one skill entering the runtime session: which skill, the
// immutable source it came from (git commit SHA when git-sourced; empty for
// inline bodies), and whether the load succeeded.
type SkillLoadPayload struct {
	// Name is the Skill object name (ns/name when cross-namespace).
	Name string `json:"name"`
	// SHA256 is the pinned git commit the body was fetched at (git-sourced
	// skills, arch §5.3.6); empty for inline skill bodies.
	SHA256 string `json:"sha256,omitempty"`
	// OK is tri-state like ToolPayload.OK: true = loaded, false = load
	// failed (Err carries why), absent = unknown. +optional
	OK  *bool  `json:"ok,omitempty"`
	Err string `json:"err,omitempty"`
}

// ArtifactRef is the payload of an EventArtifactRef event (spec §4): a pointer
// to a content-addressed artifact already committed to the coord record (§5).
type ArtifactRef struct {
	Kind       string `json:"kind"`
	WorkItemID string `json:"work_item_id"`
	URI        string `json:"uri"`
	SHA256     string `json:"sha256"`
}

// UsagePayload is the payload of an EventUsage event (spec §4): best-effort
// token counts, sanity-bounded and never authoritative for billing (§11).
type UsagePayload struct {
	Model      string `json:"model"`
	Input      int    `json:"input"`
	Output     int    `json:"output"`
	CacheRead  int    `json:"cacheRead,omitempty"`
	CacheWrite int    `json:"cacheWrite,omitempty"`
	// Reasoning is the thinking-model reasoning token count (billed as
	// output-class, §11); reported separately so the ISI-4238 views can
	// show it honestly instead of folding it into Output. +optional
	Reasoning int `json:"reasoning,omitempty"`
	// CostUSD is the provider-reported step cost when the runtime reports
	// one (ISI-4238). Best-effort like the token counts. +optional
	CostUSD float64 `json:"costUSD,omitempty"`
	// DurationMS is the step's wall-clock duration in milliseconds when
	// the runtime reports one (ISI-4238) — the llm.call span truthfully
	// carries it as its span duration. +optional
	DurationMS int64 `json:"durationMS,omitempty"`

	// Provider is the gen-AI system that served the step ("anthropic",
	// "openai", …), mapped onto gen_ai.system (ISI-4383 / ADR-0021 D2).
	// Empty when the runtime does not report it — the span then omits the
	// attribute rather than fabricating one. +optional
	Provider string `json:"provider,omitempty"`
	// ResponseModel is the model that actually SERVED the step, mapped onto
	// gen_ai.response.model (ISI-4383). Model (above) is the REQUESTED model
	// (gen_ai.request.model); when a fallback fired the two differ, which is
	// how a backup-served call is made visible on the span. Empty when the
	// runtime reports only one model. +optional
	ResponseModel string `json:"responseModel,omitempty"`
	// FinishReason is the provider's stop reason for the step ("stop",
	// "length", "tool_use", …), mapped onto gen_ai.response.finish_reasons
	// (ISI-4383). Empty when the runtime does not report it. +optional
	FinishReason string `json:"finishReason,omitempty"`
	// ResponseID is the provider's response identifier, mapped onto
	// gen_ai.response.id (ISI-4383). Empty when the runtime does not report
	// one. +optional
	ResponseID string `json:"responseID,omitempty"`
	// Fallback is true when this step was served by the Run's backup/fallback
	// model instead of the requested primary (story 5.11, correlated to
	// ksquad_fallback_activations_total). It sets the ksquad.llm.fallback=true
	// span marker so a fallback-served call is visibly flagged even when the
	// runtime cannot report requested-vs-served model separately (ISI-4383,
	// ADR-0021 D2 / limitation §74). +optional
	Fallback bool `json:"fallback,omitempty"`

	// Prompt / Response are the D3 opt-in content-capture fields (ISI-4383,
	// ADR-0021 D3). They are NEVER populated in prod: content stays off by
	// default (the PII posture — tool args are SHA-256 only). They ride the
	// span as gated span events ONLY when KSQUAD_TRACE_CONTENT is set
	// (dev/non-prod). Absent the flag, toolusage drops them even if present.
	// +optional
	Prompt string `json:"prompt,omitempty"`
	// Response is the model's response body — see Prompt. +optional
	Response string `json:"response,omitempty"`

	// Error is the step's failure message when the model round-trip failed
	// (timeout, provider error, …). Empty on success. When set, the llm.call
	// span records an exception event and an Error status (ISI-5015). +optional
	Error string `json:"error,omitempty"`
	// ErrorType is the failure classifier the runtime reported (the provider
	// error name — "APIError", "timeout", …), recorded as exception.type on
	// the llm.call span alongside Error. Empty when the runtime reports none.
	// +optional
	ErrorType string `json:"errorType,omitempty"`
}

// AuthRequiredPayload is the payload of an EventAuthRequired event (spec §4/§7).
type AuthRequiredPayload struct {
	Provider  string `json:"provider"`
	SecretRef string `json:"secretRef"`
	Detail    string `json:"detail"`
}

// RateLimitedPayload is the payload of an EventRateLimited event (spec §4,
// story 5.10). It is the wire form of the standardized rate_limited signal: the
// model the Run was serving from, plus the provider's Retry-After header value
// carried RAW and UNPARSED. The single canonical parse lives consumer-side in
// modelendpoint.ParseRetryAfter (RFC 7231 §7.1.3) — the shim must not pre-parse,
// so an HTTP-date window is resolved against the CONSUMER's clock, never the
// producer's, and there is exactly one Retry-After interpretation in the tree.
// An empty RetryAfter means the provider sent no window (the backoff path).
type RateLimitedPayload struct {
	// Model is the model that was rate-limited (maps to RawRateLimit.FromModel;
	// empty falls back to Agent.spec.model in OnRateLimited).
	Model string `json:"model"`
	// RetryAfter is the raw Retry-After header exactly as the provider sent it
	// ("120", "Wed, 21 Oct 2015 07:28:00 GMT", or "" for no window). The
	// consumer feeds it to modelendpoint.ParseRetryAfter.
	RetryAfter string `json:"retryAfter,omitempty"`
}

// AgentCard is the capability contract the core negotiates against (spec §6.1).
// It is generated at shim startup from the Agent CRD + resolved AgentRuntime +
// the runtime adapter's declared capabilities, and advertises the pinned
// protocol revisions (internal/protocol) so a peer negotiates against explicit
// wire revs rather than an implicit "latest".
type AgentCard struct {
	SchemaVersion string            `json:"schemaVersion"`
	Agent         AgentIdentity     `json:"agent"`
	Runtime       RuntimeInfo       `json:"runtime"`
	Model         ModelInfo         `json:"model"`
	Skills        []string          `json:"skills"`
	Auth          AuthInfo          `json:"auth"`
	Capabilities  Capabilities      `json:"capabilities"`
	Protocol      protocol.Versions `json:"protocol"`
}

// AgentIdentity is the identity block of the Agent Card (spec §6.1/§6.2),
// sourced from Agent.spec.
type AgentIdentity struct {
	Name    string `json:"name"`
	Squad   string `json:"squad"`
	Project string `json:"project"`
}

// RuntimeInfo is the runtime block of the Agent Card (spec §6.1), sourced from
// the resolved AgentRuntime.
type RuntimeInfo struct {
	Type         string `json:"type"`
	CLIVersion   string `json:"cliVersion"`
	ShimVersion  string `json:"shimVersion"`
	Experimental bool   `json:"experimental,omitempty"`
}

// ModelInfo is the model block of the Agent Card (spec §6.1). ContextWindow is
// runtime-declared and is the budget authority for the context Assembler (§6.2).
type ModelInfo struct {
	ID            string `json:"id"`
	ContextWindow int    `json:"contextWindow"`
}

// AuthInfo is the auth block of the Agent Card (spec §6.1). Type is one of the
// three §7 shapes; the shim knows only the shape, never the raw secret.
type AuthInfo struct {
	Type      string `json:"type"`
	SecretRef string `json:"secretRef"`
}

// Capabilities is the runtime-declared capability set (spec §6.1). streaming
// MUST be true (SSE V2 is mandatory); interactivePrompt gates §3.1
// input-required (C6); byoModelEndpoint gates the model-provider seam (§11);
// rateLimitSignal advertises that the runtime emits the standardized §4
// EventRateLimited signal (story 5.10) — a peer that negotiates it false must
// not rely on the signal and stays on the 2.11 backoff-only path.
type Capabilities struct {
	Streaming         bool     `json:"streaming"`
	ToolCalls         bool     `json:"toolCalls"`
	InteractivePrompt bool     `json:"interactivePrompt"`
	BYOModelEndpoint  bool     `json:"byoModelEndpoint"`
	RateLimitSignal   bool     `json:"rateLimitSignal"`
	ArtifactKinds     []string `json:"artifactKinds"`
	Docker            bool     `json:"docker"`
	GitHub            bool     `json:"github"`
	PackageInstall    bool     `json:"packageInstall"`
}

// Shim is the internal stable interface every conformant runtime shim
// implements (spec §3). The core speaks these verbs; the adapter maps them to
// the pinned A2A wire rev. V5 EmitArtifact is shim-initiated (coord client),
// so it is not part of this southbound interface.
type Shim interface {
	// SubmitTask (V1) drives a task. A second submit with an existing
	// a2a_task_id reattaches and MUST NOT start a second execution (C1); a
	// submit on a terminal task returns the terminal status.
	SubmitTask(ctx context.Context, t Task) (Status, error)
	// StreamEvents (V2) returns an SSE-style channel of events with seq >
	// fromSeq; a re-stream resumes gap-free from the last delivered seq (C4).
	StreamEvents(ctx context.Context, taskID string, fromSeq uint64) (<-chan Event, error)
	// GetStatus (V3) is a pure read of the current task status.
	GetStatus(ctx context.Context, taskID string) (Status, error)
	// CancelTask (V4) is idempotent: it drains a live task to canceled and is
	// a no-op success on an already-terminal or unknown task (C8).
	CancelTask(ctx context.Context, taskID, reason string) error
	// GetAgentCard (V6) returns the capability contract (spec §6).
	GetAgentCard(ctx context.Context) (AgentCard, error)
}
