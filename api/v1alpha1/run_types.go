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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RunPhase is the Run status state machine (arch §8):
//
//	Pending → Claiming → Running → {Succeeded | Failed | Cancelled | Paused}
//
// with retry/backoff edges Failed → Claiming and Paused → Running/Claiming.
// The phase is CEL/OpenAPI-validated to exactly this set — an out-of-set
// phase fails admission (fail-closed, story 1.2 AC6/AC8).
// +kubebuilder:validation:Enum=Pending;Claiming;Running;Paused;Succeeded;Failed;Cancelled
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
	// explicitly-trusted dev. Unset means the platform default (gvisor).
	// +kubebuilder:validation:Enum=gvisor;kata;runc
	// +optional
	RuntimeClass string `json:"runtimeClass,omitempty"`

	// Class routes the warm-pool regime (§9.2 hybrid): interactive Runs
	// draw from the warm pool; batch/non-interactive Runs may cold-start.
	// Unset defaults to interactive (reconciler, story 1.3).
	// +kubebuilder:validation:Enum=interactive;batch
	// +optional
	Class string `json:"class,omitempty"`
}

// RetryPolicy bounds the §8 failure/resume retry loop (FR-A5, NFR-REL1/2).
type RetryPolicy struct {
	// MaxRetries bounds automatic retries; 0 means no automatic retry.
	// +optional
	MaxRetries *int32 `json:"maxRetries,omitempty"`

	// BackoffSeconds is the base delay for the exponential backoff with
	// jitter applied between retries (§8).
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

	// ObservedGeneration is the generation most recently observed by the
	// Run reconciler (§5.2).
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
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
