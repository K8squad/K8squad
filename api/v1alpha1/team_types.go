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

// TeamSpec defines the desired state of Team (arch §5.1, story 1.2 AC2).
//
// A Team is the squad tenancy boundary (§12.1 — "a squad is a namespace"):
// its Projects, Runs, sandbox pods, workspace PVCs and per-user Secrets live
// in the Team's namespace. This story defines only the type; the Team
// reconciler (story 1.3) ensures the namespace, RBAC, NetworkPolicy and
// quota.
type TeamSpec struct {
	// Projects lists the Project CRs this squad works on (refs).
	// +optional
	Projects []ObjectRef `json:"projects,omitempty"`

	// Agents lists the Agent CRs composing this squad (refs).
	// +optional
	Agents []ObjectRef `json:"agents,omitempty"`

	// NamespaceStrategy describes how the Team's namespace is provisioned and
	// managed (arch §5.1, §12.1). The Team reconciler (story 1.3) owns the
	// strategy semantics.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	NamespaceStrategy string `json:"namespaceStrategy"`

	// OwnedBy is the owner principal ref (story 1.6, ISI-2522): the
	// authoritative ownership signal for resource-scoped permission checks
	// (Epic 15.3) — not a display field. Mutable: ownership may be
	// transferred after creation. Defaults to the created-by principal at
	// admission (internal/webhook AttributionWebhook) and is indexed for
	// RBAC scope queries (internal/index).
	// +optional
	OwnedBy PrincipalRef `json:"ownedBy,omitempty"`

	// Grants bind capabilities to the roles in this squad's composition
	// (ADR-0024a D3, ADR-0024 O-1 role→capability). The grant store is
	// data/config: adding "work_item.author" to a role is a Team edit — no
	// rebuild to widen (ADR-0024 §3). Bind to the PM role initially (O-1).
	//
	// Run assembly (pkg/capability.ResolveGrant) reads the grant for a Run's
	// decomposing agent from its owning Team and feeds the authoring-MCP
	// gate (S2, ISI-4868) and the run-token capability claim (S3, ISI-4869)
	// from one resolution. Deny-by-default: a role with no matching grant —
	// or a Team with no Grants — is treated as ungranted.
	// +optional
	// +listType=map
	// +listMapKey=role
	Grants []CapabilityGrant `json:"grants,omitempty"`
}

// CapabilityGrant binds a set of capability slugs to a role within a Team's
// composition (ADR-0024a D3). It is pure config: granting a new capability
// (e.g. "work_item.author") is a Team edit, never a rebuild (ADR-0024 §3).
// Keyed by role — not by agent — so it composes with the Role CR model and
// grants every agent on the Team whose spec.roleRef resolves to this role.
type CapabilityGrant struct {
	// Role is the Role name (Role.metadata.name) this grant binds to. Every
	// dispatched agent whose spec.roleRef resolves to this role is granted
	// the listed capabilities at Run assembly.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Role string `json:"role"`

	// Capabilities are the capability slugs granted to the role — an open
	// string list (only "work_item.author" is defined today; kept open so
	// widening a grant never needs a CRD/schema change). Deny-by-default: a
	// role absent from a Team's Grants carries no capability.
	// +kubebuilder:validation:MinItems=1
	Capabilities []string `json:"capabilities"`
}

var _ OwnedByHolder = &Team{}

// GetOwnedBy returns the spec.ownedBy owner principal (story 1.6).
func (t *Team) GetOwnedBy() PrincipalRef { return t.Spec.OwnedBy }

// SetOwnedBy sets the spec.ownedBy owner principal (story 1.6).
func (t *Team) SetOwnedBy(p PrincipalRef) { t.Spec.OwnedBy = p }

// TeamStatus defines the observed state of Team.
type TeamStatus struct {
	// Namespace is the squad namespace this Team reconciles into (story 4.1,
	// arch §12.1: a squad is a namespace). Resolved once — deterministically
	// derived from the Team name + a short hash of the Team UID — and then
	// recorded here; later reconciles read it back rather than re-deriving,
	// so a Team rename never strands the original namespace (AC1).
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Conditions represent the latest available observations of a Team's state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the generation most recently observed by the
	// Team reconciler (§5.2) — readiness conditions are only trustworthy
	// when stamped with the generation they describe.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=team,categories=ksquad
// +kubebuilder:webhook:path=/mutate-ksquad-io-v1alpha1-team,mutating=true,failurePolicy=fail,sideEffects=None,groups=ksquad.io,resources=teams,verbs=create;update,versions=v1alpha1,name=mteam-attribution.ksquad.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-ksquad-io-v1alpha1-team,mutating=false,failurePolicy=fail,sideEffects=None,groups=ksquad.io,resources=teams,verbs=create;update,versions=v1alpha1,name=vteam-attribution.ksquad.io,admissionReviewVersions=v1

// Team is the Schema for the teams API. A Team is the squad tenancy boundary
// (arch §5.1, §12.1). It is namespaced by default (no cluster-scope marker).
type Team struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TeamSpec   `json:"spec,omitempty"`
	Status TeamStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TeamList contains a list of Team.
type TeamList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Team `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Team{}, &TeamList{})
}
