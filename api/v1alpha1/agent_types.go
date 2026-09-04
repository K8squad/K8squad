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

// AgentSpec defines the desired state of Agent (arch §5.1, story 1.2 AC3).
//
// An Agent is one agent instance in a squad. The arch §5.1 table does not
// list a status subresource for Agent; the Agent reconciler (story 1.3)
// validates the credential Secret + runtime and publishes the Agent Card
// (§10.1).
type AgentSpec struct {
	// RuntimeRef references the AgentRuntime CRD (arch §5.3) that defines
	// this agent's coding-agent flavor and CLI version policy. The
	// AgentRuntime type is authored in story 1.3; this ref is the seam.
	// +kubebuilder:validation:Required
	RuntimeRef ObjectRef `json:"runtimeRef"`

	// RoleRef references the Role CRD supplying the reusable behavior
	// profile (prompt ref, default skills, runtime class hint).
	// +kubebuilder:validation:Required
	RoleRef ObjectRef `json:"roleRef"`

	// SkillRefs reference the Skill CRDs granted to this agent.
	// +optional
	SkillRefs []ObjectRef `json:"skillRefs,omitempty"`

	// CredentialSecretRef is the per-user BYO credential Secret (arch §11,
	// ADR-010 BYO-lock). Admission-time resolution ("an Agent must resolve
	// its credential Secret before it is admitted") is the reconciler's
	// fail-closed job (story 1.3).
	// +kubebuilder:validation:Required
	CredentialSecretRef SecretRef `json:"credentialSecretRef"`

	// CredentialClass declares whether the referenced credential is a
	// HUMAN-SEAT interactive OAuth token bound to a person's subscription seat
	// (e.g. Claude Code OAuth, §7.2 / zero-touch 7.7) or a SERVICE-ACCOUNT
	// long-lived API key / provider token (§7.3, second-runtime story). It is
	// the vendor-neutral axis the credential injection contract (story 5.4,
	// pkg/credinject) keys on to select the runtime-native env var — e.g.
	// CLAUDE_CODE_OAUTH_TOKEN for a human seat vs ANTHROPIC_API_KEY for a
	// service account — and it drives the Epic 7 lifecycle (a human seat has an
	// OAuth refresh/concurrency lifecycle; a service account rotates by Secret
	// update). Empty defaults to service-account at injection time
	// (credinject.DefaultClass): a human OAuth seat is opt-in, never inferred.
	// +optional
	// +kubebuilder:validation:Enum=human-seat;service-account
	CredentialClass string `json:"credentialClass,omitempty"`

	// CapabilityOverrides are applied to the generated Agent Card
	// capabilities (arch §10.1) — the ticket's agentCardOverrides.
	// +optional
	CapabilityOverrides *CapabilityOverrides `json:"capabilityOverrides,omitempty"`

	// Model is the resolved model name (e.g. a Claude model id, or an
	// Ollama-served model name when a BYO endpoint is configured).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Model string `json:"model"`

	// ModelEndpointRef optionally references a per-user Secret holding a
	// BYO / Ollama / OpenAI-compatible model endpoint (arch §10.3, ADR-026,
	// `byoModelEndpoint` capability). Epic 5.7 flagged this field as
	// required at Gate 2 before 5.7 builds (CEO 2026-08-11) — the field
	// itself is mandatory on the type, while remaining optional at runtime:
	// an Agent on a paid provider with the default endpoint sets none of
	// the optional fields. It is a model-provider seam — NOT an
	// AgentRuntime.type and NOT a new image (that category error is
	// recorded in ADR-026).
	// +optional
	ModelEndpointRef *SecretRef `json:"modelEndpointRef,omitempty"`

	// ContextBudgetOverride optionally overrides the Project-level context
	// budget (arch §8.5) for this agent — e.g. a ~200K allocation for a
	// Claude-backed agent while a BYO-Ollama agent takes ~8K on the same
	// project. Resolution order: Project default → Agent override → Run
	// dynamic trim, clamped by the resolved model contextWindow; a value
	// above the window is a fail-closed validation error (reconciler,
	// story 1.3).
	// +optional
	ContextBudgetOverride *ContextBudget `json:"contextBudgetOverride,omitempty"`

	// FallbackModel optionally names a secondary model for mid-Run model
	// switches on rate_limited signals (arch §8 tier-1 recovery, §10.3,
	// ADR-030/ADR-031), optionally with its own endpoint/credential.
	// +optional
	FallbackModel *FallbackModel `json:"fallbackModel,omitempty"`

	// OwnedBy is the owner principal ref (story 1.6, ISI-2522): the
	// authoritative ownership signal for resource-scoped permission checks
	// (Epic 15.3) — not a display field. Mutable: ownership may be
	// transferred after creation. Defaults to the created-by principal at
	// admission (internal/webhook AttributionWebhook) and is indexed for
	// RBAC scope queries (internal/index).
	// +optional
	OwnedBy PrincipalRef `json:"ownedBy,omitempty"`

	// ToolCredentials are auxiliary, NON-MODEL credentials injected
	// by-reference into the Run's agent container for local CLI tools
	// (ISI-3565, decision doc isi-3564-gh-token-seam-decision.md). This is a
	// sibling seam to the model credentialSecretRef/credentialClass path
	// (pkg/credinject): a github-token entry lands GH_TOKEN + GITHUB_TOKEN so
	// a local gh/git can push a branch and open a PR on the tree reposync
	// already checked out. It is a LIST because an agent may need more than
	// one aux token later; each entry names a PURPOSE (never an arbitrary env
	// var name — the seam owns the name mapping so a Secret cannot shadow
	// PATH or a model key) and the Secret backing it. Projected name-only
	// onto the Run spec (ADR-045 D5) and injected through
	// pkg/capability.AssemblePod, the LIVE sandbox-assembly seam.
	// +optional
	ToolCredentials []ToolCredential `json:"toolCredentials,omitempty"`
}

// ToolCredential declares one auxiliary, non-model credential to inject into
// a Run's agent container for a local CLI tool (ISI-3565). Purpose is a
// closed enum the injection seam (pkg/toolcred) maps to a fixed set of
// runtime env var names — users pick the tool, never the env name — and
// SecretRef names the BYO Secret carrying the token by reference.
type ToolCredential struct {
	// Purpose selects WHICH local tool the credential is for, and therefore
	// which env var name(s) the token is injected under. It is a closed enum
	// the seam owns (pkg/toolcred.KnownPurposes); an unknown purpose fails
	// closed at admission (webhook) and at injection (AssemblePod). Empty is
	// invalid — there is no safe default tool to guess.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=github-token
	Purpose string `json:"purpose"`

	// SecretRef is the BYO Secret carrying the token. Injected by reference
	// (SecretKeySelector) — the control plane never reads the bytes. Key
	// defaults to "token" when empty.
	// +kubebuilder:validation:Required
	SecretRef SecretRef `json:"secretRef"`
}

var _ OwnedByHolder = &Agent{}

// GetOwnedBy returns the spec.ownedBy owner principal (story 1.6).
func (a *Agent) GetOwnedBy() PrincipalRef { return a.Spec.OwnedBy }

// SetOwnedBy sets the spec.ownedBy owner principal (story 1.6).
func (a *Agent) SetOwnedBy(principal PrincipalRef) { a.Spec.OwnedBy = principal }

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=ag,categories=ksquad
// +kubebuilder:webhook:path=/mutate-ksquad-io-v1alpha1-agent,mutating=true,failurePolicy=fail,sideEffects=None,groups=ksquad.io,resources=agents,verbs=create;update,versions=v1alpha1,name=magent-attribution.ksquad.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-ksquad-io-v1alpha1-agent,mutating=false,failurePolicy=fail,sideEffects=None,groups=ksquad.io,resources=agents,verbs=create;update,versions=v1alpha1,name=vagent-attribution.ksquad.io,admissionReviewVersions=v1

// Agent is the Schema for the agents API — one agent instance in a squad
// (arch §5.1). It is namespaced by default.
type Agent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec AgentSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// AgentList contains a list of Agent.
type AgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Agent `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Agent{}, &AgentList{})
}
