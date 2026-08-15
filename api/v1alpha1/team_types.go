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
}

var _ OwnedByHolder = &Team{}

// GetOwnedBy returns the spec.ownedBy owner principal (story 1.6).
func (t *Team) GetOwnedBy() PrincipalRef { return t.Spec.OwnedBy }

// SetOwnedBy sets the spec.ownedBy owner principal (story 1.6).
func (t *Team) SetOwnedBy(p PrincipalRef) { t.Spec.OwnedBy = p }

// TeamStatus defines the observed state of Team.
type TeamStatus struct {
	// Conditions represent the latest available observations of a Team's state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
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
