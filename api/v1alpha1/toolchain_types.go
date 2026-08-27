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
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ToolchainRBACScope is the scope the operator renders a Toolchain's rules
// at: a per-Run namespaced Role (default), or — only for cluster-catalog
// Toolchains behind the explicit platform opt-in — a ClusterRole
// (plan §2.2b: cluster scope is never renderable from a team namespace).
// +kubebuilder:validation:Enum=namespace;cluster
type ToolchainRBACScope string

const (
	// ToolchainRBACScopeNamespace renders (and binds) the rules as a
	// per-Run Role in the squad namespace. The default and only scope a
	// team-namespace override may carry.
	ToolchainRBACScopeNamespace ToolchainRBACScope = "namespace"

	// ToolchainRBACScopeCluster renders the rules as a per-Run
	// ClusterRole. Admitted ONLY on cluster-catalog Toolchains AND only
	// when the platform opt-in (Helm tools.rbac.clusterScopeEnabled) is
	// set; rejected everywhere else, fail-closed (plan §2.2b).
	ToolchainRBACScopeCluster ToolchainRBACScope = "cluster"
)

// ToolchainVersion is one pin of the toolchain pack: the version string
// Skill.spec.requires.toolchains selects ("kubectl@1.31" → version "1.31")
// and the image Run assembly stages onto the tool volume (arch §5.3.2).
type ToolchainVersion struct {
	// Version is the version selector half of the name@version ref. Must
	// be unique within the Toolchain (CEL pairwise + admission).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Version string `json:"version"`

	// Image is the OCI image Run assembly stages as the toolchain's init
	// container. A digest pin (ghcr.io/k8squad/toolchains/kubectl@sha256:…)
	// is the recommended form: a Run records the resolved reference in its
	// status so a moving tag must never silently alter in-flight behavior
	// (same reproducibility discipline as GitSkillSource.ref, §5.3.6).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Provides lists the binaries the pack puts on PATH (e.g.
	// ["kubectl", "kustomize"]). Documentation/staging hint for Run
	// assembly and the capability manifest.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	Provides []string `json:"provides,omitempty"`
}

// ToolchainRBAC declares the Kubernetes permissions a tool needs. The
// operator — never the user — renders and binds this (plan §2.2b): Run
// assembly unions the RBAC of all resolved toolchains into one per-Run Role
// bound to the managed ksquad-agent ServiceAccount, garbage-collected with
// the Run. RBAC is honored only from cluster-catalog Toolchains; team
// overrides may narrow (subset), never widen.
type ToolchainRBAC struct {
	// Scope selects the rendering scope. Default "namespace". "cluster" is
	// admitted only on cluster-catalog Toolchains with the platform
	// opt-in set (plan §2.2b trust boundary).
	// +optional
	// +kubebuilder:validation:Enum=namespace;cluster
	// +kubebuilder:validation:Default=namespace
	Scope ToolchainRBACScope `json:"scope,omitempty"`

	// Rules are standard rbacv1.PolicyRules. Wildcards ("*") in
	// apiGroups/resources/verbs are rejected at admission — the catalog is
	// curated least-privilege (same D2 discipline as the Team baseline
	// Role). Duplicates across toolchains dedupe at Run assembly; the
	// union is additive, never intersected away.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=64
	Rules []rbacv1.PolicyRule `json:"rules,omitempty"`
}

// ToolchainSpec defines the desired state of Toolchain (plan §2.2).
//
// Data-only and validated: a Toolchain declares the install (versions →
// pinned images) and the declarative RBAC envelope a tool needs. Trust
// boundary (D8/§2.2b): the cluster catalog (ksquad-system, admin-authored)
// is the authority; a team-namespace Toolchain with the same name is an
// override that may only NARROW — subset rules, no scope widening, no new
// versions/images — enforced fail-closed at admission.
// +kubebuilder:validation:XValidation:message="toolchain versions: every version entry must set a non-empty image",rule="self.versions.all(v, has(v.image) && v.image != ”)"
// +kubebuilder:validation:XValidation:message="toolchain versions: version strings must be unique; two entries with different content cannot share a version",rule="self.versions.all(v, self.versions.all(w, w == v || w.version != v.version))"
// +kubebuilder:validation:XValidation:message="toolchain rbac: every rule must declare at least one verb",rule="!has(self.rbac) || !has(self.rbac.rules) || self.rbac.rules.all(r, has(r.verbs) && r.verbs.size() > 0)"
type ToolchainSpec struct {
	// Versions are the installable pins of this toolchain. At least one
	// is required — an abstract toolchain with no versions resolves
	// nothing and fails Run admission (fail-closed, plan §2.2).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +listType=atomic
	Versions []ToolchainVersion `json:"versions"`

	// RBAC is the declarative permission envelope the operator renders
	// per Run. Absent = the tool needs no Kubernetes API access (git, gh,
	// node, …): staging only, no rules granted.
	// +optional
	RBAC *ToolchainRBAC `json:"rbac,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=tc,categories=ksquad
// +kubebuilder:printcolumn:name="Versions",type=integer,JSONPath=`.spec.versions`
// +kubebuilder:printcolumn:name="RBAC",type=string,JSONPath=`.spec.rbac.scope`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:webhook:path=/validate-ksquad-io-v1alpha1-toolchain,mutating=false,failurePolicy=fail,sideEffects=None,groups=ksquad.io,resources=toolchains,verbs=create;update,versions=v1alpha1,name=vtoolchain-v1alpha1.ksquad.io,admissionReviewVersions=v1

// Toolchain is the Schema for the toolchains API (plan §2.2): a catalog
// entry for a toolchain pack — pinned versions/images plus the tool's
// declarative RBAC envelope — that Skill.spec.requires.toolchains
// ("kubectl@1.31") resolves against, fail-closed.
type Toolchain struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ToolchainSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// ToolchainList contains a list of Toolchain.
type ToolchainList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Toolchain `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Toolchain{}, &ToolchainList{})
}
