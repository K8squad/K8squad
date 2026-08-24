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
	"time"

	"github.com/K8squad/K8squad/internal/protocol"
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
}

// Status is the V3 GetStatus result (spec §3 V3): the current task state, an
// optional human reason, and the last SSE seq delivered (resume anchor, C4).
type Status struct {
	State   TaskState `json:"state"`
	Reason  string    `json:"reason,omitempty"`
	LastSeq uint64    `json:"lastSeq"`
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
	// EventArtifactRef points to a §5 artifact already committed to coord.
	EventArtifactRef EventType = "artifact-ref"
	// EventUsage is best-effort token counts for metering (spec §11).
	EventUsage EventType = "usage"
	// EventAuthRequired is the auth-failure pause signal (spec §7/§11, C7).
	EventAuthRequired EventType = "auth-required"
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
	Name    string `json:"name"`
	Phase   string `json:"phase"`
	OK      bool   `json:"ok,omitempty"`
	Summary string `json:"summary,omitempty"`
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
}

// AuthRequiredPayload is the payload of an EventAuthRequired event (spec §4/§7).
type AuthRequiredPayload struct {
	Provider  string `json:"provider"`
	SecretRef string `json:"secretRef"`
	Detail    string `json:"detail"`
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
// input-required (C6); byoModelEndpoint gates the model-provider seam (§11).
type Capabilities struct {
	Streaming         bool     `json:"streaming"`
	ToolCalls         bool     `json:"toolCalls"`
	InteractivePrompt bool     `json:"interactivePrompt"`
	BYOModelEndpoint  bool     `json:"byoModelEndpoint"`
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
