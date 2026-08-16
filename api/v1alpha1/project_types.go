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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProjectSpec defines the desired state of Project (arch §5.1, §5.4, §8.5,
// story 1.2 AC5).
//
// A Project couples an upstream source repository (mirrored, not made the
// source of truth — §5.4) with a workspace and a context budget.
type ProjectSpec struct {
	// Repo is the upstream source repository mirrored by KSquad (§5.4:
	// GitHub is the v1 provider behind the pkg/scm seam; the fenced
	// coordination record stays authoritative).
	// +kubebuilder:validation:Required
	Repo RepoSpec `json:"repo"`

	// WorkspacePVC sizes and classes the Project workspace PVC (§9.4 —
	// Runs work in their own git-worktree on this volume).
	// +optional
	WorkspacePVC *PVCSpec `json:"workspacePVC,omitempty"`

	// EgressPolicyRef references the egress policy applied to this
	// project's Runs (§12.2 — default-deny NetworkPolicy + allowlist).
	// +optional
	EgressPolicyRef *ObjectRef `json:"egressPolicyRef,omitempty"`

	// Goals are project-level goals injected into every Run's context
	// envelope (§8.5). A goal change is a new Project revision; the next Run
	// assembles against it while in-flight Runs keep their snapshot.
	// +optional
	Goals []string `json:"goals,omitempty"`

	// ContextBudget is the project-level default per-tier token allocation
	// (§8.5) — raise it once for projects with large architecture docs and
	// every agent on the project inherits it. Per-Agent overrides via
	// Agent.spec.contextBudgetOverride; per-Run dynamic trim in the shim.
	// +optional
	ContextBudget *ContextBudget `json:"contextBudget,omitempty"`

	// OwnedBy is the owner principal ref (story 1.6, ISI-2522): the
	// authoritative ownership signal for resource-scoped permission checks
	// (Epic 15.3) — not a display field. Mutable: ownership may be
	// transferred after creation. Defaults to the created-by principal at
	// admission (internal/webhook AttributionWebhook) and is indexed for
	// RBAC scope queries (internal/index).
	// +optional
	OwnedBy PrincipalRef `json:"ownedBy,omitempty"`
}

var _ OwnedByHolder = &Project{}

// GetOwnedBy returns the spec.ownedBy owner principal (story 1.6).
func (p *Project) GetOwnedBy() PrincipalRef { return p.Spec.OwnedBy }

// SetOwnedBy sets the spec.ownedBy owner principal (story 1.6).
func (p *Project) SetOwnedBy(principal PrincipalRef) { p.Spec.OwnedBy = principal }

// RepoSpec is the upstream source repository of a Project, plus its sync
// configuration (arch §5.4).
type RepoSpec struct {
	// URL of the upstream repository, e.g.
	// "https://github.com/acme/widget".
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	URL string `json:"url"`

	// Ref is the default ref to track (branch or tag); empty means the
	// provider default branch.
	// +optional
	Ref string `json:"ref,omitempty"`

	// Auth carries the per-Project BYO credential for the provider.
	// +optional
	Auth *RepoAuth `json:"auth,omitempty"`

	// Sync configures the repo-sync reconciler (§5.4). Nil disables sync.
	// +optional
	Sync *RepoSyncSpec `json:"sync,omitempty"`
}

// RepoAuth is the provider credential discipline for a Project repo
// (arch §5.4, D8, NFR-SEC8): a per-Project/per-user BYO Secret ref, scoped
// to mirror-read (+ status-write only when reflectOutbound) — never a shared
// master token, and never logged, echoed or exposed to an agent Run.
type RepoAuth struct {
	// CredentialSecretRef is the BYO provider credential Secret.
	// +kubebuilder:validation:Required
	CredentialSecretRef SecretRef `json:"credentialSecretRef"`
}

// RepoSyncSpec configures the repo-sync reconciler for a Project
// (arch §5.4, ADR-018).
type RepoSyncSpec struct {
	// Provider is the source-control provider. GitHub is v1; GitLab and
	// Gitea drop in behind the same pkg/scm interface (§5.4, ADR-018).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=github;gitlab;gitea
	Provider string `json:"provider"`

	// WebhookSecretRef references the per-Project HMAC Secret. The HMAC
	// signature is verified before any webhook payload is parsed (§5.4,
	// FR-H4, NFR-SEC8); webhooks are only a fast path on top of the
	// level-triggered reconcile.
	// +optional
	WebhookSecretRef *SecretRef `json:"webhookSecretRef,omitempty"`

	// Mirror selects what the inbound reconciler mirrors into the scm
	// schema. Nil means the default set: issues, pull requests, check runs
	// and release/build artifacts (§5.4).
	// +optional
	Mirror *RepoMirrorSpec `json:"mirror,omitempty"`

	// ReflectOutbound opts in to posting KSquad Run status/comments back to
	// the provider (§5.4): off by default, requires a status-write-scoped
	// token, every write origin-marked for echo suppression.
	// +optional
	ReflectOutbound bool `json:"reflectOutbound,omitempty"`
}

// RepoMirrorSpec selects the inbound mirror subset (arch §5.4). Nil fields
// default to mirroring that object class.
type RepoMirrorSpec struct {
	// Issues mirrors provider issues (FR-H1 issue⇄work-item mapping).
	// +optional
	Issues *bool `json:"issues,omitempty"`

	// PullRequests mirrors PRs incl. review state (FR-H2).
	// +optional
	PullRequests *bool `json:"pullRequests,omitempty"`

	// CheckRuns mirrors CI check runs (FR-H2).
	// +optional
	CheckRuns *bool `json:"checkRuns,omitempty"`

	// Artifacts mirrors release/build artifacts by URI + sha (FR-H2).
	// +optional
	Artifacts *bool `json:"artifacts,omitempty"`
}

// PVCSpec sizes and classes a workspace PVC (arch §5.1 — "workspacePVC
// (size/class)").
type PVCSpec struct {
	// Size of the PVC, e.g. "50Gi".
	// +kubebuilder:validation:Required
	Size resource.Quantity `json:"size"`

	// Class is the storageClass name; empty means the cluster default.
	// +optional
	Class string `json:"class,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=proj,categories=ksquad
// +kubebuilder:webhook:path=/mutate-ksquad-io-v1alpha1-project,mutating=true,failurePolicy=fail,sideEffects=None,groups=ksquad.io,resources=projects,verbs=create;update,versions=v1alpha1,name=mproject-attribution.ksquad.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-ksquad-io-v1alpha1-project,mutating=false,failurePolicy=fail,sideEffects=None,groups=ksquad.io,resources=projects,verbs=create;update,versions=v1alpha1,name=vproject-attribution.ksquad.io,admissionReviewVersions=v1

// Project is the Schema for the projects API — a repo + workspace
// (arch §5.1). It is namespaced by default.
type Project struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ProjectSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// ProjectList contains a list of Project.
type ProjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Project `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Project{}, &ProjectList{})
}
