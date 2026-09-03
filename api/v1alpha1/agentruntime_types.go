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

// Conformant runtime types (FR-D3). The set is deliberately NOT a closed
// +kubebuilder:validation:Enum: an out-of-set type is admitted only behind
// spec.experimental=true, enforced by the spec-level CEL rule below. A new
// shim vendor therefore registers its runtime with zero CRD schema change.
const (
	// RuntimeTypeOpenClaw is the OpenClaw coding-agent flavor.
	RuntimeTypeOpenClaw = "openclaw"

	// RuntimeTypeClaudeCode is the Claude Code coding-agent flavor.
	RuntimeTypeClaudeCode = "claude-code"

	// RuntimeTypeOpenCode is the OpenCode coding-agent flavor.
	RuntimeTypeOpenCode = "opencode"

	// RuntimeTypeHermes is the Hermes coding-agent flavor.
	RuntimeTypeHermes = "hermes"

	// RuntimeTypeCodex is the ChatGPT Codex coding-agent flavor (ISI-3647).
	RuntimeTypeCodex = "codex"
)

// AgentRuntimeSpec defines the desired state of AgentRuntime (arch §5.3,
// story 1.3 Task 0 — minimal authoring so Agent.spec.runtimeRef has a
// target; the richer runtime policy surface lands with the sandbox stories).
//
// +kubebuilder:validation:XValidation:message="spec.type must be one of the conformant runtimes [openclaw claude-code opencode hermes codex], or set spec.experimental=true to admit a vendor-shim runtime (FR-D3)",rule="self.type in ['openclaw','claude-code','opencode','hermes','codex'] || self.experimental"
type AgentRuntimeSpec struct {
	// Type is the coding-agent flavor this runtime serves (arch §5.3):
	// openclaw, claude-code, opencode, hermes or codex out of the box. The field is
	// intentionally a free string rather than a closed enum: shim vendors
	// register additional flavors behind spec.experimental=true with zero
	// CRD schema change (FR-D3). A non-conformant type without the
	// experimental flag fails admission with the FR-D3 message.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Type string `json:"type"`

	// Experimental opts this runtime into the non-conformant posture: it
	// admits types outside the conformant set (FR-D3 shim-vendor seam) and
	// marks the runtime as such on the generated Agent Card. Absence means
	// conformant.
	// +optional
	// +kubebuilder:default=false
	Experimental bool `json:"experimental,omitempty"`

	// CLIVersion pins the coding-agent CLI this runtime serves, as an
	// immutable revision (tag or commit SHA) — the reproducibility
	// discipline of ADR-017. Admission does not resolve the pin (that is
	// the Agent reconciler's fail-closed job); it only rejects an empty
	// value when the field is set.
	// +optional
	// +kubebuilder:validation:MinLength=1
	CLIVersion string `json:"cliVersion,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=ar,categories=ksquad

// AgentRuntime is the Schema for the agentruntimes API — the coding-agent
// flavor + CLI version policy an Agent points at via spec.runtimeRef
// (arch §5.3). It is namespaced, data-only and validated: same-object rules
// (FR-D3 open-ended type discipline) run as CEL in the CRD schema;
// existence of the referenced object from Agent is checked by the Agent
// validating admission webhook (story 1.3).
type AgentRuntime struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec AgentRuntimeSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// AgentRuntimeList contains a list of AgentRuntime.
type AgentRuntimeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentRuntime `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentRuntime{}, &AgentRuntimeList{})
}
