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
}

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
