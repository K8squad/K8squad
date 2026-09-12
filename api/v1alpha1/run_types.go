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

package v1alpha1

import (
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RunPhase is the Run status state machine (arch §8):
//
//	Pending → Claiming → Running → {Succeeded | Failed | Cancelled | Paused}
//
// with retry/backoff edges Failed → Claiming and Paused → Running/Claiming, and
// the 3.3 operator-kill transitional Canceling (teardown owed → Cancelled).
// The phase is CEL/OpenAPI-validated to exactly this set — an out-of-set
// phase fails admission (fail-closed, story 1.2 AC6/AC8).
// +kubebuilder:validation:Enum=Pending;Claiming;Running;Paused;Canceling;Succeeded;Failed;Cancelled
type RunPhase string

const (
	// RunPhasePending is the initial phase: the Run is admitted but not yet
	// claimed.
	RunPhasePending RunPhase = "Pending"

	// RunPhaseClaiming: the reconciler requests a warm sandbox and assembles
	// the Run pod (§5.3.4, §8).
	RunPhaseClaiming RunPhase = "Claiming"

	// RunPhaseRunning: the shim is invoked over A2A and the agent works the
	// item(s) through the coordination record (§8).
	RunPhaseRunning RunPhase = "Running"

	// RunPhasePaused: credential expiry (§11) or rate-limit wait (§8
	// tier 2) with a persisted resume_at.
	RunPhasePaused RunPhase = "Paused"

	// RunPhaseCanceling: the 3.3 operator-kill transitional phase — kill was
	// issued, the controller owes the sandbox teardown, then the Run marks
	// Cancelled (FR-A6/F4).
	RunPhaseCanceling RunPhase = "Canceling"

	// RunPhaseSucceeded: terminal success.
	RunPhaseSucceeded RunPhase = "Succeeded"

	// RunPhaseFailed: terminal failure; retryPolicy may re-enter Claiming.
	RunPhaseFailed RunPhase = "Failed"

	// RunPhaseCancelled: terminal operator kill (FR-A6/F4).
	RunPhaseCancelled RunPhase = "Cancelled"
)

// LLM interaction types for tracking different kinds of LLM interactions
const (
	// InteractionPrompt represents an LLM prompt/request
	InteractionPrompt = "prompt"

	// InteractionResponse represents an LLM response
	InteractionResponse = "response"

	// InteractionToolCall represents a tool call made by the LLM
	InteractionToolCall = "tool_call"

	// InteractionToolResponse represents a response from a tool call
	InteractionToolResponse = "tool_response"
)

// LLM interaction error codes
const (
	// ErrorCodeRateLimited indicates the interaction was rate limited
	ErrorCodeRateLimited = "rate_limited"

	// ErrorCodeTimeout indicates the interaction timed out
	ErrorCodeTimeout = "timeout"

	// ErrorCodeInvalidRequest indicates the request was invalid
	ErrorCodeInvalidRequest = "invalid_request"

	// ErrorCodeModelError indicates the model returned an error
	ErrorCodeModelError = "model_error"

	// ErrorCodeUnknown indicates an unknown error occurred
	ErrorCodeUnknown = "unknown"
)

// RunSpec defines the desired state of Run (arch §5.1, §8, story 1.2 AC6).
//
// The Run is the unit of squad work and the ONLY CRD that touches
// coordination data — strictly through the opaque spec.workItemRef and
// status.artifactRefs references (ADR-001/ADR-002).
type RunSpec struct {
	// TeamRef references the Team under whose tenancy this Run executes
	// (namespace, RBAC, NetworkPolicy, quota — §12.1).
	// +kubebuilder:validation:Required
	TeamRef ObjectRef `json:"teamRef"`

	// ProjectRef references the Project supplying the repo, workspace PVC
	// and context budget for this Run.
	// +kubebuilder:validation:Required
	ProjectRef ObjectRef `json:"projectRef"`

	// WorkItemRef is an opaque id into the apiserver/coordination Postgres
	// (§4/§6); the Run references the work item, it does not own or embed it
	// (ADR-001). Work items, comments, claims, artifacts and memory records
	// are Postgres rows, NOT CRDs — embedding the work item here, or making
	// this an owned etcd object, would reintroduce the dual-write
	// split-brain ADR-001 exists to prevent (story 1.2 AC6/AC7).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	WorkItemRef string `json:"workItemRef"`

	// Inputs are free-form run parameters folded into the §8.5 context
	// envelope by the Context Assembler. Modeled as an opaque structured
	// field in story 1.2 (open question OQ1); refined in story 1.3.
	// +optional
	Inputs map[string]string `json:"inputs,omitempty"`

	// SandboxPolicy is the RuntimeClass/isolation selection input to §9.1
	// sandbox assembly (also opaque-structured per OQ1; semantics land with
	// the Run reconciler, story 1.3).
	// +optional
	SandboxPolicy SandboxPolicy `json:"sandboxPolicy,omitempty"`

	// Agents selects the Agent CRs to dispatch; empty lets the reconciler
	// default from the Team's composition (story 1.3).
	// +optional
	Agents []ObjectRef `json:"agents,omitempty"`

	// RetryPolicy bounds automatic retries with backoff for sandbox/agent
	// failures (§8, FR-A5).
	// +optional
	RetryPolicy *RetryPolicy `json:"retryPolicy,omitempty"`

	// OwnedBy is the owner principal ref (story 1.6, ISI-2522): the
	// authoritative ownership signal for resource-scoped permission checks
	// (Epic 15.3) — not a display field. Mutable: ownership may be
	// transferred after creation (e.g. "kill own Runs" contributor checks
	// resolve against it, Epic 15.3 role matrix). Defaults to the
	// created-by principal at admission (internal/webhook
	// AttributionWebhook) and is indexed for RBAC scope queries
	// (internal/index).
	// +optional
	OwnedBy PrincipalRef `json:"ownedBy,omitempty"`

	// ToolCredentials are the auxiliary, non-model credentials projected from
	// the dispatched Agent(s) (Agent.spec.toolCredentials, ISI-3565) onto the
	// Run so the resolved Run self-describes which aux tokens its agent
	// container carries — name-only by reference, ADR-045 D5, the same
	// discipline the MCP credential projection uses. pkg/capability.AssemblePod
	// reads these and injects GH_TOKEN/GITHUB_TOKEN into the agent container
	// by reference via pkg/toolcred.
	// +optional
	ToolCredentials []ToolCredential `json:"toolCredentials,omitempty"`
}

var _ OwnedByHolder = &Run{}

// GetOwnedBy returns the spec.ownedBy owner principal (story 1.6).
func (r *Run) GetOwnedBy() PrincipalRef { return r.Spec.OwnedBy }

// SetOwnedBy sets the spec.ownedBy owner principal (story 1.6).
func (r *Run) SetOwnedBy(principal PrincipalRef) { r.Spec.OwnedBy = principal }

// SandboxPolicy selects the isolation posture for a Run's sandbox
// (arch §9.1/§9.2; story 1.2 models it as an opaque structured field per
// OQ1 — the reconciler refines semantics in story 1.3).
type SandboxPolicy struct {
	// RuntimeClass selects the isolation runtime (§9.1): gVisor is the
	// default, Kata the high-assurance opt-in, runc only for
	// explicitly-trusted dev. Unset follows the operator's cluster default
	// (KSQUAD_SANDBOX_RUNTIME_CLASS; gvisor when the operator does not pin
	// one) — M1.2 (ISI-4128): an admission-level gvisor default made the
	// cluster knob unreachable on clusters without a gvisor RuntimeClass.
	// +kubebuilder:validation:Enum=gvisor;kata;runc
	// +optional
	RuntimeClass string `json:"runtimeClass,omitempty"`

	// Class routes the warm-pool regime (§9.2 hybrid): interactive Runs
	// draw from the warm pool; batch/non-interactive Runs may cold-start.
	// Unset defaults to interactive at admission (story 1.3 structural
	// defaulting).
	// +kubebuilder:validation:Enum=interactive;batch
	// +kubebuilder:default=interactive
	// +optional
	Class string `json:"class,omitempty"`
}

// RetryPolicy bounds the §8 failure/resume retry loop (FR-A5, NFR-REL1/2).
// Default values: MaxRetries=5, BackoffSeconds=60.
//
// +kubebuilder:validation:XValidation:message="retryPolicy.maxRetries must be >= 0 and <= 20 (0 means no automatic retry, 20 is safety limit)",rule="!has(self.maxRetries) || (self.maxRetries >= 0 && self.maxRetries <= 20)"
// +kubebuilder:validation:XValidation:message="retryPolicy.backoffSeconds must be >= 1 and <= 3600; set a reasonable base delay for the exponential backoff",rule="!has(self.backoffSeconds) || (self.backoffSeconds >= 1 && self.backoffSeconds <= 3600)"
type RetryPolicy struct {
	// MaxRetries bounds automatic retries; 0 means no automatic retry.
	// Default: 5 (safety limit to prevent infinite loops)
	// +kubebuilder:default=5
	// +optional
	MaxRetries *int32 `json:"maxRetries,omitempty"`

	// BackoffSeconds is the base delay for the exponential backoff with
	// jitter applied between retries (§8).
	// Default: 60 seconds
	// +kubebuilder:default=60
	// +optional
	BackoffSeconds *int32 `json:"backoffSeconds,omitempty"`
}

// RunStatus defines the observed state of Run (arch §5.1 status subresource,
// §8; story 1.2 AC6). Written only by the Run reconciler via the status
// subresource.
type RunStatus struct {
	// Phase is the §8 state machine value. Enum-validated so an unknown
	// phase fails admission (fail-closed).
	// +optional
	Phase RunPhase `json:"phase,omitempty"`

	// SandboxRef references the sandbox pod serving the Run (§9 warm pool).
	// +optional
	SandboxRef *ObjectRef `json:"sandboxRef,omitempty"`

	// ClaimedAt records when the Run's fenced claim was acquired (§6.2/§6.3,
	// §8).
	// +optional
	ClaimedAt *metav1.Time `json:"claimedAt,omitempty"`

	// Conditions represent the latest available observations of a Run's
	// state (§5.2 — e.g. Paused(auth_failure), Paused(rate_limited)).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ArtifactRefs are refs to artifact rows in the coordination Postgres
	// (§6.1) — the artifacts themselves are NOT embedded in the CRD
	// (story 1.2 AC7). Name carries the opaque artifact id.
	// +optional
	ArtifactRefs []ObjectRef `json:"artifactRefs,omitempty"`

	// ModelSegments is the 5.11 mid-Run provenance ledger: which model
	// served which portion of the Run, in order. The reconciler opens a
	// segment at dispatch and at every fallback switch, and closes it when
	// the portion ends (rate_limited switch or Run terminal). The endpoint
	// rides as the Secret NAME (never the URL with credentials echoed);
	// consumption attribution (7.6) and the 8.8 fallback indicators read
	// this. Bounded (MaxItems) so a pathological switch loop cannot bloat
	// the status subresource.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=32
	ModelSegments []ModelSegment `json:"modelSegments,omitempty"`

	// LLMInteractions is the bounded per-run LLM interaction summary
	// (ISI-4238): one entry per model call — prompt/response DIGESTS
	// (never full payloads; the raw wire log rides the §4 event stream),
	// model, token usage and duration. Digests are capped by schema so a
	// pathological run cannot bloat the status subresource past etcd's
	// object budget; the full-fidelity record stays in the event log.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=128
	LLMInteractions []LLMInteraction `json:"llmInteractions,omitempty"`

	// TotalTokenUsage summarizes the token usage across all LLM
	// interactions for this run (ISI-4238): the one-glance computational
	// cost of the run, maintained by the run-drive consumer from EventUsage
	// wire events.
	// +optional
	TotalTokenUsage *TokenUsage `json:"totalTokenUsage,omitempty"`

	// TraceID is the root OpenTelemetry trace id for this run (ISI-4238):
	// the shim opens a run span at dispatch and stamps its trace id here
	// via the terminal status, so logs, metrics and llm.call/tool.call
	// spans are correlatable end-to-end and a failing run is debuggable
	// from the CR alone.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{0,32}$`
	TraceID string `json:"traceID,omitempty"`

	// ContextSnapshot pins the resolved §8.5 context envelope inputs
	// (work-item rev, goal rev, memory doc-ids, resolved budget, model
	// window) for audit + re-entrant reuse (stories 3.6/5.9). Written by
	// the Run reconciler at the Claiming → Running transition; a resumed
	// Run re-assembles from it instead of re-querying latest.
	// +optional
	ContextSnapshot *ContextSnapshot `json:"contextSnapshot,omitempty"`

	// GrantedToolchainRBAC records the toolchain RBAC the operator
	// rendered for this Run (plan §2.2b): which catalog entries
	// contributed, and the exact unioned rule set bound to the pod's
	// ServiceAccount — the full audit of "which Run got which permissions
	// through which toolchain". Cleared when the Run goes terminal and
	// the per-Run Role is garbage-collected.
	// +optional
	GrantedToolchainRBAC *ToolchainRBACGrant `json:"grantedToolchainRBAC,omitempty"`

	// CapabilityManifest is the resolved capability envelope this Run was
	// dispatched with (plan §2.3, ADR-044 step 5): resolved toolchain
	// images, resolved MCP endpoints with their EFFECTIVE tool filters,
	// and the capabilityHash that keys warm-pool inventory. Computed
	// pre-dispatch and IMMUTABLE for the life of the Run — mid-flight
	// changes to Skills/Toolchains/MCPServers never widen a running
	// sandbox; they apply to the next Run. Unlike GrantedToolchainRBAC it
	// is NOT cleared at terminal: it is the reproducibility record of
	// what the sandbox actually ran with (ADR-017 discipline).
	// +optional
	CapabilityManifest *CapabilityManifest `json:"capabilityManifest,omitempty"`

	// ObservedGeneration is the generation most recently observed by the
	// Run reconciler (§5.2).
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// ToolchainRBACGrant is the recorded outcome of the per-Run toolchain RBAC
// rendering (plan §2.2b): the union of every resolved toolchain's rules,
// rendered as a Role (plus, behind the platform opt-in, a ClusterRole for
// cluster-scope rules) bound to the managed ksquad-agent ServiceAccount.
type ToolchainRBACGrant struct {
	// RoleRef names the per-Run Role (ksquad-run-<run-name>) in the Run's
	// namespace. Absent when no resolved toolchain declared RBAC.
	// +optional
	RoleRef *ObjectRef `json:"roleRef,omitempty"`

	// ClusterRoleRef names the per-Run ClusterRole rendered for
	// cluster-scope rules (platform opt-in only). Absent in the default
	// posture — the curated catalog is namespace-scoped.
	// +optional
	ClusterRoleRef *ObjectRef `json:"clusterRoleRef,omitempty"`

	// Toolchains is the resolved provenance set: which catalog entry
	// (name@version → image) contributed to the grant, for reproducibility
	// and the Epic C capability manifest.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=32
	Toolchains []ResolvedToolchainRef `json:"toolchains,omitempty"`

	// Rules is the exact unioned grant — what `kubectl auth can-i
	// --as=system:serviceaccount:<ns>:ksquad-agent` shows for this Run's
	// toolchain surface (plus the Team baseline, which stays empty).
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=256
	Rules []rbacv1.PolicyRule `json:"rules,omitempty"`
}

// ResolvedToolchainRef pins one toolchain a Run resolved (plan §2.2
// reproducibility): the name@version the skills demanded, the image Run
// assembly stages, and the catalog namespace the winning entry came from.
type ResolvedToolchainRef struct {
	// Name is the catalog name ("kubectl").
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Version is the pinned version ("1.31").
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Version string `json:"version"`

	// Image is the resolved OCI reference (digest pins recommended).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// SourceNamespace is the namespace of the winning catalog entry (the
	// override when one applied, else the cluster catalog).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	SourceNamespace string `json:"sourceNamespace"`
}

// CapabilityManifest is the resolved capability envelope of a Run (ADR-044
// step 5): what the sandbox was actually assembled with. It carries NO
// credential material — credentials are recorded as Secret NAMES only
// (ADR-045: never literal in status).
type CapabilityManifest struct {
	// Toolchains is the resolved, pinned toolchain set staged as init
	// containers (sorted by name for stable bytes).
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=64
	Toolchains []ResolvedToolchainRef `json:"toolchains,omitempty"`

	// MCPEndpoints is the resolved MCP server set with each server's
	// EFFECTIVE tool filter (server envelope − deny globs, computed
	// fail-closed at assembly — ADR-042/044).
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=32
	MCPEndpoints []ResolvedMCPEndpoint `json:"mcpEndpoints,omitempty"`

	// Skills are the granted skills collected during capability assembly
	// (ADR-0004 Phase 1 keystone): identity, source type, and permissions
	// copied verbatim from Skill.spec.permissions only (never from body
	// content — AC4/AC7 D8). Records NO credential material — only provenance.
	// +optional
	// +listType=atomic
	Skills []GrantedSkill `json:"skills,omitempty"`

	// CapabilityHash is sha256 of the manifest's canonical JSON. It keys
	// warm-pool inventory (ADR-044 step 7: identical capability envelopes
	// share pool stock) and gives consoles/audits a cheap equality handle.
	// +optional
	// +kubebuilder:validation:MinLength=1
	CapabilityHash string `json:"capabilityHash,omitempty"`
}

// ResolvedMCPEndpoint is one MCP server a Run resolved, recorded without
// credential material: the transport and endpoint/command, the effective
// allow/deny tool globs, the sidecar flag, and the Secret/EgressPolicy
// NAMES involved (provenance, never contents — ADR-045).
type ResolvedMCPEndpoint struct {
	// Name is the MCPServer object's name.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Transport is the server's transport (stdio | streamable-http).
	// +kubebuilder:validation:Required
	Transport MCPTransport `json:"transport"`

	// URL is the streamable-http endpoint (empty for stdio).
	// +optional
	URL string `json:"url,omitempty"`

	// Headers are the static non-secret HTTP headers sent to a
	// streamable-http endpoint (secret-bearing header names are rejected
	// at MCPServer admission — nothing sensitive is recorded here).
	// +optional
	Headers map[string]string `json:"headers,omitempty"`

	// Command is the stdio server executable (empty for streamable-http).
	// +optional
	Command string `json:"command,omitempty"`

	// Args are the stdio command arguments.
	// +optional
	// +listType=atomic
	Args []string `json:"args,omitempty"`

	// Image is the stdio server's packaged image; when set, Run assembly
	// stages it as a native sidecar container (ADR-044 step 6). Empty for
	// streamable-http, or stdio servers whose command must exist inside
	// the runtime image.
	// +optional
	Image string `json:"image,omitempty"`

	// Sidecar records whether assembly staged a dedicated sidecar
	// container for this endpoint (stdio with image).
	// +optional
	Sidecar bool `json:"sidecar,omitempty"`

	// AllowTools is the EFFECTIVE allow set after the server envelope was
	// intersected with observed tools and deny globs subtracted
	// (fail-closed on empty — ADR-044 step 4).
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=256
	AllowTools []string `json:"allowTools,omitempty"`

	// DenyTools is the deny-glob set recorded verbatim from the server
	// envelope (already subtracted from AllowTools).
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=256
	DenyTools []string `json:"denyTools,omitempty"`

	// CredentialSecretRef names the BYO Secret backing this server's
	// credential (name only — never material; ADR-045 D5).
	// +optional
	CredentialSecretRef *SecretRef `json:"credentialSecretRef,omitempty"`

	// EgressPolicyRef names the EgressPolicy covering a streamable-http
	// endpoint's egress (R1: MCP rides the existing egress story).
	// +optional
	EgressPolicyRef *ObjectRef `json:"egressPolicyRef,omitempty"`
}

// GrantedSkill records a skill's identity, source type, and permissions
// for capability assembly (ADR-0004 Phase 1 keystone). Permissions are copied
// verbatim from Skill.spec.permissions ONLY (never from body content — AC4/AC7 D8).
type GrantedSkill struct {
	// Namespace is the skill's namespace.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`

	// Name is the skill's name.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// SourceType is how the skill body is obtained (inline|git).
	// +kubebuilder:validation:Required
	SourceType SkillSourceType `json:"sourceType"`

	// Inline is the skill body when sourceType=inline (empty for git).
	// +optional
	Inline string `json:"inline,omitempty"`

	// Git is the git source when sourceType=git (nil for inline).
	// +optional
	Git *GitSkillSource `json:"git,omitempty"`

	// Permissions are the CRD-authorized capability envelope copied verbatim
	// from Skill.spec.permissions — trust boundary (D8): never widened by
	// untrusted body content.
	// +optional
	// +listType=atomic
	Permissions []string `json:"permissions,omitempty"`
}

// LLMInteraction is one per-run LLM call summary (ISI-4238): what was
// asked (request digest), what came back (response digest), from which
// model, at what token cost and latency. Payloads carry DIGESTS capped by
// schema (MaxLength applies to the base64 string form) — full prompts and
// responses never ride the CR; they stay in the §4 event stream.
// +kubebuilder:validation:XValidation:message="Type must be one of: prompt, response, tool_call, tool_response",rule="self.type in ['prompt', 'response', 'tool_call', 'tool_response']"
type LLMInteraction struct {
	// ID is unique identifier for this interaction
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	ID string `json:"id"`

	// Type of interaction: "prompt" | "response" | "tool_call" | "tool_response"
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=prompt;response;tool_call;tool_response
	Type string `json:"type"`

	// Model used for this interaction
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Model string `json:"model"`

	// Timestamp when the interaction occurred
	// +kubebuilder:validation:Required
	Timestamp metav1.Time `json:"timestamp"`

	// Request digest (prompt or tool-call arguments, truncated+hashed per
	// ISI-4238 — raw secrets-bearing text never rides the CR)
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=4096
	Request []byte `json:"request"`

	// Response digest (model reply or tool output, truncated)
	// +optional
	// +kubebuilder:validation:MaxLength=4096
	Response []byte `json:"response,omitempty"`

	// Token usage information
	// +optional
	TokenUsage *TokenUsage `json:"tokenUsage,omitempty"`

	// Duration of the interaction in milliseconds
	// +kubebuilder:validation:Minimum=0
	DurationMs int64 `json:"durationMs"`

	// Error information if the interaction failed
	// +optional
	Error *InteractionError `json:"error,omitempty"`
}

// TokenUsage tracks input, output, and total tokens for an LLM interaction
// +kubebuilder:validation:XValidation:message="TotalTokens must equal InputTokens + OutputTokens",rule="self.totalTokens == self.inputTokens + self.outputTokens"
// +kubebuilder:validation:XValidation:message="InputTokens and OutputTokens must be non-negative",rule="self.inputTokens >= 0 && self.outputTokens >= 0"
type TokenUsage struct {
	// InputTokens is the number of tokens in the prompt/request
	// +kubebuilder:validation:Minimum=0
	InputTokens int64 `json:"inputTokens"`

	// OutputTokens is the number of tokens in the response
	// +kubebuilder:validation:Minimum=0
	OutputTokens int64 `json:"outputTokens"`

	// TotalTokens is the sum of input and output tokens
	// +kubebuilder:validation:Minimum=0
	TotalTokens int64 `json:"totalTokens"`
}

// InteractionError captures error information for failed interactions.
// +kubebuilder:validation:XValidation:message="Code must be a valid error code",rule="self.code in ['rate_limited', 'timeout', 'invalid_request', 'model_error', 'unknown']"
type InteractionError struct {
	// Code is the error classification code
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=rate_limited;timeout;invalid_request;model_error;unknown
	Code string `json:"code"`

	// Message is the human-readable error message
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Message string `json:"message"`

	// Details provides additional context about the error
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Details string `json:"details,omitempty"`
}

// ModelSegment is one portion of a Run served by one model (5.11
// provenance). A segment is OPEN while its model serves (EndedAt nil) and
// CLOSED when the portion ends — Reason names why (rate_limited on a
// switch, terminal on completion).
type ModelSegment struct {
	// Model is the model name that served this portion.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Model string `json:"model"`

	// SecretName names the per-user endpoint Secret this model was served
	// from ("" = the runtime's provider default). Provenance only — never
	// Secret contents.
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// StartedAt is when the portion began.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// EndedAt is when the portion ended; nil while serving.
	// +optional
	EndedAt *metav1.Time `json:"endedAt,omitempty"`

	// Reason names why the portion ended (e.g. rate_limited on a fallback
	// switch); empty on the still-open segment or a clean terminal handoff
	// stamped by the reconciler.
	// +optional
	Reason string `json:"reason,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=run,categories=ksquad
// +kubebuilder:webhook:path=/mutate-ksquad-io-v1alpha1-run,mutating=true,failurePolicy=fail,sideEffects=None,groups=ksquad.io,resources=runs,verbs=create;update,versions=v1alpha1,name=mrun-attribution.ksquad.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-ksquad-io-v1alpha1-run,mutating=false,failurePolicy=fail,sideEffects=None,groups=ksquad.io,resources=runs,verbs=create;update,versions=v1alpha1,name=vrun-attribution.ksquad.io,admissionReviewVersions=v1

// Run is the Schema for the runs API — the unit of squad work (arch §5.1,
// §8), reconciled by the Run state machine. It is namespaced by default.
// Run.spec.workItemRef is an opaque coordination-DB pointer (ADR-001): the
// Run references the work item, it never owns or embeds it.
type Run struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RunSpec   `json:"spec,omitempty"`
	Status RunStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RunList contains a list of Run.
type RunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Run `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Run{}, &RunList{})
}
