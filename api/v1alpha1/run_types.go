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
	// explicitly-trusted dev. Unset defaults to gvisor at admission
	// (story 1.3 structural defaulting).
	// +kubebuilder:validation:Enum=gvisor;kata;runc
	// +kubebuilder:default=gvisor
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
