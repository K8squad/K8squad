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

// SkillSourceType discriminates how a Skill body is obtained (arch §5.3.6).
// +kubebuilder:validation:Enum=inline;git
type SkillSourceType string

const (
	// SkillSourceInline means the skill body lives in the CRD itself.
	SkillSourceInline SkillSourceType = "inline"

	// SkillSourceGit means the skill body is fetched from a git repository
	// via the pkg/scm provider seam, pinned to a commit SHA.
	SkillSourceGit SkillSourceType = "git"
)

// SkillSpec defines the desired state of Skill (arch §5.1, §5.3.4, §5.3.6,
// story 1.2 AC4).
//
// A Skill is a granted tool/capability. Data-only and validated; it drives
// the operator's pod assembly (§5.3.4) when the Run reconciler unions the
// requirements of the Run's skills.
type SkillSpec struct {
	// Source selects how the skill body is obtained: inline (body in the
	// CRD) or git (fetched via pkg/scm pinned to a commit SHA, §5.3.6).
	// The type discriminator is enum-validated so an unknown source fails
	// admission (fail-closed, story 1.2 AC8); the deeper inline⇔git field
	// consistency webhook lands with story 1.3.
	// +kubebuilder:validation:Required
	Source SkillSource `json:"source"`

	// McpToolRefs list the MCP tool endpoints granted to this skill. Part of
	// the CRD-authorized capability envelope.
	// +optional
	McpToolRefs []ObjectRef `json:"mcpToolRefs,omitempty"`

	// Permissions is the CRD-authorized capability envelope. Trust boundary
	// (D8, §5.3.6): a git-sourced body is UNTRUSTED input — it supplies
	// behavior inside this envelope and never widens it. The envelope is
	// authorized here by the operator/admin who registers the skill, never
	// self-declared by the fetched repo content.
	// +optional
	Permissions []string `json:"permissions,omitempty"`

	// Requires declares toolchain packs and service sidecars the operator
	// merges into the Run pod at assembly (§5.3.4).
	// +optional
	Requires SkillRequires `json:"requires,omitempty"`
}

// SkillSource is the discriminated inline|git source of a skill body
// (arch §5.3.6).
//
// The inline⇔git field consistency rule (story 1.3): exactly one body
// carrier matches the discriminator, fail-closed at admission via CEL.
// +kubebuilder:validation:XValidation:message="source.inline must carry the body (non-empty) and source.git must be unset when source.type is inline; set source.type=inline and provide source.inline",rule="self.type != 'inline' || (has(self.inline) && self.inline != '' && !has(self.git))"
// +kubebuilder:validation:XValidation:message="source.git must carry the body (repoRef and ref set) and source.inline must be unset when source.type is git; set source.type=git and provide source.git.repoRef/ref",rule="self.type != 'git' || (has(self.git) && self.git.repoRef != '' && self.git.ref != '' && !has(self.inline))"
type SkillSource struct {
	// Type discriminates the source: inline or git.
	// +kubebuilder:validation:Required
	Type SkillSourceType `json:"type"`

	// Inline is the literal skill body, used when type=inline.
	// +kubebuilder:validation:MaxLength=262144
	// +optional
	Inline string `json:"inline,omitempty"`

	// Git fetches the body from a git repository via the pkg/scm provider
	// seam (§5.4, ADR-018). The fetched body is untrusted input (D8) and is
	// scanned/validated before staging (§5.3.6).
	// +optional
	Git *GitSkillSource `json:"git,omitempty"`
}

// GitSkillSource locates a git-sourced skill body, pinned to an immutable
// revision (arch §5.3.6 — same reproducibility discipline as
// AgentRuntime.cliVersion, ADR-017).
type GitSkillSource struct {
	// RepoRef is the repository reference via the pkg/scm provider, e.g.
	// "github.com/acme/squad-skills". GitHub is the v1 provider; GitLab and
	// Gitea drop in behind the same seam (§5.4, ADR-018).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	RepoRef string `json:"repoRef"`

	// Ref is the immutable revision to fetch — PINNED to a commit SHA, never
	// a floating branch (§5.3.6): a Run resolves a skill to an immutable
	// revision so a repo force-push cannot silently alter in-flight
	// behavior. Moving refs are admitted only behind an explicit
	// experimental/dev posture, resolved-and-recorded per Run by the
	// reconciler (story 1.3).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Ref string `json:"ref"`

	// Path to the skill inside the repository, e.g. "skills/pg-migrate".
	// +optional
	Path string `json:"path,omitempty"`

	// CredentialSecretRef is an optional BYO read-only token Secret for
	// private repositories (§11 — never a shared KSquad token).
	// +optional
	CredentialSecretRef *SecretRef `json:"credentialSecretRef,omitempty"`
}

// SkillRequires is a skill's self-declared toolchain and sidecar needs,
// consumed by the operator's pod-assembly algorithm (arch §5.3.4): the union
// of a Run's skills' requirements becomes init containers (toolchains) and
// capability-gated service sidecars.
type SkillRequires struct {
	// Toolchains are language/CLI packs staged as init containers (§5.3.2),
	// expressed as name@version, e.g. "go@1.23", "node@22". Version
	// conflicts across a Run's skills fail closed (§5.3.4).
	// +optional
	Toolchains []string `json:"toolchains,omitempty"`

	// Sidecars are stateful service sidecars (§5.3.3), e.g. "dockerd" —
	// capability-gated against AgentRuntime.capabilities at assembly.
	// +optional
	Sidecars []string `json:"sidecars,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=skill,categories=ksquad
// +kubebuilder:webhook:path=/validate-ksquad-io-v1alpha1-skill,mutating=false,failurePolicy=fail,sideEffects=None,groups=ksquad.io,resources=skills,verbs=create;update,versions=v1alpha1,name=vskill-crossrefs.ksquad.io,admissionReviewVersions=v1

// Skill is the Schema for the skills API — a granted tool/capability
// (arch §5.1). It is namespaced by default.
type Skill struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SkillSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// SkillList contains a list of Skill.
type SkillList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Skill `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Skill{}, &SkillList{})
}
