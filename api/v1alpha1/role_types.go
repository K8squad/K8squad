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

	// ActivePhases lists the lifecycle phases in which an agent holding this
	// role is eligible to be dispatched (phase-lifecycle, ISI-4431 E3). The
	// values are the six middle working phases only — backlog/todo/done/
	// cancelled are intake/terminal lanes never "worked" by a role, so a role
	// active in e.g. "done" is a config error caught at admission.
	// EMPTY ⇒ phase-agnostic: the role is eligible in every phase (today's
	// behavior, the back-compat default).
	// +kubebuilder:validation:items:Enum=design;planning;implementation;code_review;testing;documentation
	// +optional
	ActivePhases []string `json:"activePhases,omitempty"`

	// Coordinator marks this role as its team's lifecycle driver: the single
	// actor that advances tickets across phases and dispatches the phase-
	// appropriate role (phase-lifecycle, ISI-4431 E3/E5). At most ONE
	// coordinator role per Team — that cardinality is enforced at Team
	// admission (a Role is reusable across teams), NOT on the Role itself. A
	// coordinator MAY also carry activePhases (it can itself work a phase) but
	// need not.
	// +optional
	Coordinator bool `json:"coordinator,omitempty"`

	// CoordinatorMode selects coordinator autonomy (ISI-4431 Q4). It is
	// IGNORED unless Coordinator=true (the webhook rejects it when
	// coordinator=false).
	//   auto    (default) — advance on phase-agent success, dispatch the next
	//                        role, no human gate; backward "rework" edges allowed.
	//   propose           — raise a request_confirmation / audit-logged proposal
	//                        before each advance and wait for acceptance.
	// +kubebuilder:validation:Enum=auto;propose
	// +optional
	CoordinatorMode string `json:"coordinatorMode,omitempty"`

	// ReportsTo is the org-chart edge (ISI-7946 WS-A): the Role this one
	// reports to — its manager in the squad's reporting hierarchy. The edge
	// lives on the Role (agents resolve to theirs via spec.roleRef) and is
	// what the Team hierarchy invariants and the routing spine build on.
	// The legacy ksquad.io/reports-to label stays as a derived/compat
	// mirror: the mutating Role webhook stamps it from this field and keeps
	// a pre-existing label when the field is unset, so pkg/orgops scope
	// derivation is unchanged. Dangling-manager and Team hierarchy checks
	// live at Team admission (internal/webhook/v1alpha1, GuardTeamOrgChart);
	// here only the self-edge and cross-namespace shapes are rejected.
	// +optional
	ReportsTo *ObjectRef `json:"reportsTo,omitempty"`

	// Process is the manager's ordered stage pipeline (ISI-7946 WS-A,
	// BMAD-style): each stage names the Role that executes it — an eng
	// manager code->review->test, a product manager
	// brainstorm->prd->architecture. Order is list order; stage names are
	// unique (map-keyed). Team admission rejects stages naming Roles
	// outside the team and delegation cycles; the Role webhook rejects an
	// empty process and duplicate stage names.
	// +optional
	// +listType=map
	// +listMapKey=stage
	// +kubebuilder:validation:MinItems=1
	Process []ProcessStage `json:"process,omitempty"`
}

// LabelReportsTo is the derived/compat mirror of Spec.ReportsTo — the label
// pkg/orgops scope derivation and kubectl queries read. The mutating Role
// webhook stamps it from spec.reportsTo. It intentionally mirrors
// orgops.LabelReportsTo: api/v1alpha1 cannot import pkg/orgops (orgops
// imports this package).
const LabelReportsTo = "ksquad.io/reports-to"

// ProcessStage is one stage of a manager Role's ordered Process (ISI-7946
// WS-A): the stage name plus the Role that executes it.
type ProcessStage struct {
	// Stage is the stage's unique name within the Process (the map key).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Stage string `json:"stage"`

	// RoleRef names the Role that executes this stage. Team admission
	// requires it to resolve to a Role in the team's composition.
	// +kubebuilder:validation:Required
	RoleRef ObjectRef `json:"roleRef"`
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
