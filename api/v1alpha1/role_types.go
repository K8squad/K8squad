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

// RoleSpec defines the desired state of Role (arch §5.1, story 1.2 AC4).
//
// A Role is a reusable behavior profile. It is data-only and validated, not
// reconciled (arch §5.1 table: "data only; validated").
type RoleSpec struct {
	// PromptRef references the prompt / behavior definition for this role.
	// +kubebuilder:validation:Required
	PromptRef ObjectRef `json:"promptRef"`

	// DefaultSkills are granted to every agent assuming this role. They are
	// UNIONED with the Agent's own skillRefs, not replaced by them: the
	// effective skill set is Agent.spec.skillRefs ∪ Role.spec.defaultSkills,
	// deduped by ref (ADR-044 step 1).
	// +optional
	DefaultSkills []ObjectRef `json:"defaultSkills,omitempty"`

	// RuntimeClassHint suggests the sandbox isolation posture for Runs using
	// this role (arch §9.1: gVisor is the default RuntimeClass, Kata is the
	// high-assurance opt-in, runc only for explicitly-trusted dev). A hint,
	// not a sandboxPolicy — Run.spec.sandboxPolicy is the selection input.
	// +kubebuilder:validation:Enum=gvisor;kata;runc
	// +optional
	RuntimeClassHint string `json:"runtimeClassHint,omitempty"`

	// Model is the role-tier model name applied to every agent assuming this
	// role that does not set its own Agent.spec.model (Model-Per-Role,
	// ISI-4430). OPTIONAL: an empty role model falls through to the
	// system-default ModelConfig singleton. Resolution is tier-as-a-unit
	// (ISI-4430 D4): the effective (primary, fallback) pair is taken from the
	// highest tier — agent, then role, then default — whose model is non-empty.
	// +optional
	Model string `json:"model,omitempty"`

	// FallbackModel optionally names the role-tier secondary model for mid-Run
	// model switches on rate_limited signals (arch §8 tier-1 recovery, §10.3).
	// It is used only when the role tier supplies the effective model (i.e.
	// Agent.spec.model is empty and Role.spec.model is set); a lower tier's
	// fallback is never grafted onto a higher tier's primary (ISI-4430 D4).
	// Reuses the shared FallbackModel type (common_types.go) so the fallback
	// contract stays identical across Agent and Role.
	// +optional
	FallbackModel *FallbackModel `json:"fallbackModel,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=role,categories=ksquad

// Role is the Schema for the roles API — a reusable behavior profile
// (arch §5.1). It is namespaced by default.
type Role struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec RoleSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// RoleList contains a list of Role.
type RoleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Role `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Role{}, &RoleList{})
}
