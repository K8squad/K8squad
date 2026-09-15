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

// ModelConfigSpec defines the system-default model tier for the Model-Per-Role
// resolution (ISI-4430). It is the floor of the tier-as-a-unit walk (D4):
// agent (Agent.spec.model) → role (Role.spec.model) → this default. It is
// data-only and validated, not reconciled — the resolver (pkg/modelendpoint)
// reads it; no ModelConfig controller exists.
type ModelConfigSpec struct {
	// Model is the system-default model name — the floor that must always
	// exist so an Agent/Role with no model still resolves (ISI-4430 D3, the
	// explicit guard against "hidden failures"). REQUIRED on this CRD: unlike
	// Agent.spec.model and Role.spec.model (both optional, they fall through),
	// the default tier has nothing below it to fall through to.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Model string `json:"model"`

	// FallbackModel optionally names the default-tier secondary model for
	// mid-Run switches on rate_limited signals (arch §8 tier-1 recovery,
	// §10.3). Used only when the effective model comes from this default tier.
	// Reuses the shared FallbackModel type (common_types.go) so the fallback
	// contract is identical across Agent, Role, and ModelConfig.
	// +optional
	FallbackModel *FallbackModel `json:"fallbackModel,omitempty"`

	// ModelEndpointRef optionally references a Secret holding the default-tier
	// BYO / Ollama / OpenAI-compatible model endpoint (endpointURL + optional
	// apiToken), used when the effective model comes from this default tier
	// (ISI-4430 D5). Unset means the provider-default endpoint. It is a
	// *SecretRef — NOT an ObjectRef — to match Agent.spec.modelEndpointRef and
	// FallbackModel.modelEndpointRef and to resolve through the same
	// modelendpoint.Resolver.ResolveRef seam, which reads a Secret. (Architect
	// call, ISI-4459: the plan text said ObjectRef, but the endpoint seam is a
	// Secret everywhere; an ObjectRef would be unresolvable by the resolver.)
	// +optional
	ModelEndpointRef *SecretRef `json:"modelEndpointRef,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=modelcfg,categories=ksquad

// ModelConfig is the Schema for the modelconfigs API — the system-default
// model tier for Model-Per-Role resolution (ISI-4430). It is a namespaced
// well-known singleton: the resolver reads the object named "default" in the
// operator namespace (k8squad-system). Namespaced keeps it inside the
// operator's existing watch/RBAC scope (O3b, board-accepted). The Helm chart
// ships one such CR at install so the default tier is never empty; singleton
// identity is enforced by the resolver + install convention, not the schema.
type ModelConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ModelConfigSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// ModelConfigList contains a list of ModelConfig.
type ModelConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ModelConfig{}, &ModelConfigList{})
}
