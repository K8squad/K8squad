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

// EgressPolicySpec defines the desired state of EgressPolicy (story 4.6,
// arch §9.2/§12.2, AD-7, D5: egress is policy, not hardcode).
//
// An EgressPolicy is the explicit allowlist layered ON TOP of the squad
// namespace's default-deny NetworkPolicy baseline (story 4.1). It never
// removes or widens the baseline — it only re-opens exactly what a Run
// needs to reach (model endpoints, registries, mirrors), either directly
// via CIDR rules or, for corporate networks, by routing through a per-squad
// egress proxy referenced by Project.spec.egressPolicyRef.
type EgressPolicySpec struct {
	// Allow is the explicit egress allowlist (§12.2). Each rule opens
	// egress to the listed destinations, optionally restricted to the
	// listed ports. Empty Allow plus no Proxy is a valid policy: it keeps
	// the squad at the bare baseline (DNS + control plane only).
	// +optional
	Allow []EgressRule `json:"allow,omitempty"`

	// Proxy routes all policy-scoped egress through a per-squad egress
	// proxy (§9.2, AD-7) — the corporate-network answer to allowlists that
	// must follow DNS (NetworkPolicy is L3/L4 and cannot match FQDNs; the
	// proxy can). When set, the materialized NetworkPolicy opens egress to
	// the proxy address/port ONLY — the proxy, not the policy, decides
	// which upstreams are reachable.
	// +optional
	Proxy *EgressProxy `json:"proxy,omitempty"`
}

// EgressRule is one allowlist entry: a destination (CIDR or in-cluster
// namespace selector) with optional port restrictions.
type EgressRule struct {
	// To is the destination peer. Exactly one of CIDR or NamespaceSelector
	// must be set.
	// +kubebuilder:validation:Required
	To EgressDestination `json:"to"`

	// Ports restricts the rule to the listed destination ports. Empty
	// opens all ports to the destination.
	// +optional
	Ports []EgressPort `json:"ports,omitempty"`
}

// EgressDestination is the destination half of an EgressRule. Exactly one
// of CIDR or NamespaceSelector must be set (validated by the webhook-free
// construction rule: the reconciler fail-closes on a rule with neither or
// both).
type EgressDestination struct {
	// CIDR opens egress to an address block, e.g. "203.0.113.0/24" (a
	// model endpoint range). Except carves holes back out (deny islands).
	// +optional
	CIDR string `json:"cidr,omitempty"`

	// Except lists CIDR subnets carved out of CIDR (NetworkPolicy ipBlock
	// semantics) — e.g. allow a /16 except the metadata-service /32.
	// +optional
	Except []string `json:"except,omitempty"`

	// NamespaceSelector opens egress to pods in namespaces matching the
	// label selector (in-cluster destinations — a mirror, a registry
	// proxy, an internal gateway).
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`
}

// EgressPort restricts a rule to one destination port/protocol.
type EgressPort struct {
	// Protocol is the IP protocol (TCP default, UDP, SCTP).
	// +kubebuilder:validation:Enum=TCP;UDP;SCTP
	// +optional
	Protocol string `json:"protocol,omitempty"`

	// Port is the destination port (number or IANA name).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Port string `json:"port"`
}

// EgressProxy routes squad egress through a per-squad proxy (§9.2, AD-7).
type EgressProxy struct {
	// Address is the proxy address the NetworkPolicy opens egress to —
	// an IP or CIDR (e.g. "10.0.0.5/32" or the proxy Service's
	// spec.clusterIP). FQDN proxies are not expressible here by design:
	// NetworkPolicy is L3/L4; resolve the proxy Service's IP at install
	// time (a Headless/ExternalName indirection would silently widen the
	// rule set).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Address string `json:"address"`

	// Port is the proxy port (number or IANA name).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Port string `json:"port"`

	// Protocol is the proxy protocol (TCP default, UDP, SCTP).
	// +kubebuilder:validation:Enum=TCP;UDP;SCTP
	// +optional
	Protocol string `json:"protocol,omitempty"`
}

// EgressPolicyStatus defines the observed state of EgressPolicy.
type EgressPolicyStatus struct {
	// Conditions represent the latest available observations.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the generation most recently reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=egp,categories=ksquad
// +kubebuilder:subresource:status

// EgressPolicy is the Schema for the egresspolicies API — the explicit
// allowlist layered over the squad default-deny baseline (story 4.6,
// arch §9.2/§12.2). Referenced by Project.spec.egressPolicyRef; the Project
// reconciler materializes it as NetworkPolicies in each squad namespace
// that works the Project.
type EgressPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EgressPolicySpec   `json:"spec,omitempty"`
	Status EgressPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EgressPolicyList contains a list of EgressPolicy.
type EgressPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EgressPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&EgressPolicy{}, &EgressPolicyList{})
}
