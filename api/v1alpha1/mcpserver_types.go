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

// MCPTransport is the transport an MCPServer speaks (spike A.3: the v1alpha1
// enum is exactly the two standard MCP transports; SSE is deprecated at
// spec level and legacy-SSE-only servers surface as Ready=False connect
// failures without schema change).
// +kubebuilder:validation:Enum=stdio;streamable-http
type MCPTransport string

const (
	// MCPTransportStdio launches the server as a subprocess (Run-pod sidecar
	// when image is set, or an in-image command) speaking newline-delimited
	// JSON-RPC on stdin/stdout.
	MCPTransportStdio MCPTransport = "stdio"

	// MCPTransportStreamableHTTP speaks the MCP Streamable HTTP transport:
	// one endpoint, POST for messages, Mcp-Session-Id header echo (spike A.2).
	MCPTransportStreamableHTTP MCPTransport = "streamable-http"
)

// MCPServer condition types (ADR-042). Ready aggregates the rest.
const (
	// MCPServerConditionReady reports an MCPServer whose referenced
	// credentials/egress resolve AND whose tool surface has been discovered.
	MCPServerConditionReady = "Ready"

	// MCPServerConditionCredentialsValid reports whether spec.credentialSecretRef
	// resolves in the MCPServer's namespace (condition, not admission —
	// credential rotation must not require re-apply, ADR-042).
	MCPServerConditionCredentialsValid = "CredentialsValid"

	// MCPServerConditionEgressAllowed reports whether spec.egressRef resolves
	// to an EgressPolicy covering this server's endpoint.
	MCPServerConditionEgressAllowed = "EgressAllowed"

	// MCPServerConditionToolsDiscovered reports whether a discovery probe has
	// succeeded and populated status.observedTools. Runs never admit against
	// an unknown tool surface (ADR-042 fail-closed staleness).
	MCPServerConditionToolsDiscovered = "ToolsDiscovered"
)

// MCPToolFilter scopes which of a server's tools are granted. Allow globs
// (`create_*`); empty allow = all tools. Deny subtracts after allow. A skill
// may only narrow (intersect) the server's filter, never widen it (D8).
type MCPToolFilter struct {
	// Allow lists tool-name globs to grant. Empty (or absent) grants all of
	// the server's observed tools.
	// +optional
	Allow []string `json:"allow,omitempty"`

	// Deny lists tool-name globs subtracted from the effective allow set.
	// Exact-string overlap with allow is rejected at admission (CEL);
	// glob overlap is caught at Run assembly where an empty effective set
	// fails closed (ADR-042).
	// +optional
	Deny []string `json:"deny,omitempty"`
}

// MCPServerDiscovery tunes the control-plane discovery probe cadence
// (ADR-042: discovery runs in the control plane).
type MCPServerDiscovery struct {
	// IntervalMinutes is the periodic re-probe cadence in minutes. Default
	// 10; 0 disables the periodic re-probe (create/spec-change probes still
	// fire).
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1440
	// +kubebuilder:default=10
	IntervalMinutes *int32 `json:"intervalMinutes,omitempty"`
}

// MCPServerSpec defines the desired state of MCPServer (ADR-042).
//
// Data-only and validated: an MCPServer authorizes a data-plane egress point
// (transport + endpoint/command), its credentials, its tool envelope, and
// its egress policy linkage. It never carries trust decisions itself —
// skills only narrow the envelope (D8).
// +kubebuilder:validation:XValidation:message="transport pairing: streamable-http requires endpoint and must not set command/args; use spec.endpoint and transport=streamable-http",rule="(self.transport != 'streamable-http') || (has(self.endpoint) && self.endpoint != ” && !has(self.command))"
// +kubebuilder:validation:XValidation:message="transport pairing: stdio requires command and must not set endpoint; use spec.command and transport=stdio",rule="(self.transport != 'stdio') || (has(self.command) && self.command != ” && !has(self.endpoint))"
// +kubebuilder:validation:XValidation:message="toolFilter: allow and deny must not overlap on the same exact tool name; remove the overlap or use a glob",rule="!has(self.toolFilter) || !has(self.toolFilter.allow) || !has(self.toolFilter.deny) || self.toolFilter.allow.all(a, !self.toolFilter.deny.contains(a))"
type MCPServerSpec struct {
	// Transport selects the MCP transport: stdio or streamable-http
	// (spike A.3 closed enum; SSE is not a v1alpha1 transport).
	// +kubebuilder:validation:Required
	Transport MCPTransport `json:"transport"`

	// Endpoint is the streamable-http MCP endpoint URL. Required iff
	// transport=streamable-http; forbidden for stdio (CEL pairing rule).
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Command is the executable to launch for transport=stdio. Required iff
	// transport=stdio; forbidden for streamable-http (CEL pairing rule).
	// When Image is absent the command must exist inside the runtime image.
	// +optional
	Command string `json:"command,omitempty"`

	// Args are the command's arguments (stdio only).
	// +optional
	Args []string `json:"args,omitempty"`

	// Image optionally packages the stdio server. When set, Run assembly
	// stages it as a sidecar container (ADR-044 step 6); the discovery
	// controller's stdio probe Job runs the same image.
	// +optional
	Image string `json:"image,omitempty"`

	// Headers are static, NON-secret headers sent on streamable-http
	// requests. Secret-bearing header names (Authorization, Cookie, ...) are
	// rejected at admission — use credentialSecretRef so secret material
	// never lands in the CRD (ADR-045).
	// +optional
	Headers map[string]string `json:"headers,omitempty"`

	// CredentialSecretRef references the BYO Secret (same namespace) holding
	// the server's credential. Existence is enforced as the CredentialsValid
	// status condition, not admission, so credential rotation never requires
	// re-apply (ADR-042).
	// +optional
	CredentialSecretRef *SecretRef `json:"credentialSecretRef,omitempty"`

	// ToolFilter scopes the granted tool envelope (allow globs minus deny
	// globs). Skills may only narrow it (D8).
	// +optional
	ToolFilter *MCPToolFilter `json:"toolFilter,omitempty"`

	// EgressRef optionally names the EgressPolicy covering this server's
	// endpoint (R1: MCP rides the existing egress story, no new trust).
	// +optional
	EgressRef *ObjectRef `json:"egressRef,omitempty"`

	// Discovery tunes the control-plane probe cadence.
	// +optional
	Discovery *MCPServerDiscovery `json:"discovery,omitempty"`
}

// MCPServerStatus is the observed state of MCPServer, written only by the
// discovery controller (status subresource).
type MCPServerStatus struct {
	// ObservedTools is the tool list from the last successful discovery
	// probe (initialize → tools/list). Runs fail closed while it is empty
	// and ToolsDiscovered=False (ADR-042 staleness).
	// +optional
	ObservedTools []string `json:"observedTools,omitempty"`

	// LastProbedAt is the time of the last completed probe attempt
	// (successful or not).
	// +optional
	LastProbedAt *metav1.Time `json:"lastProbedAt,omitempty"`

	// ObservedGeneration is the spec.generation the last probe ran against.
	// A generation bump (spec change) triggers one fresh probe even when
	// discovery.intervalMinutes=0 disables the periodic cadence.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions carry Ready, CredentialsValid, EgressAllowed,
	// ToolsDiscovered (ADR-042).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=mcp;mcps,categories=ksquad
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Transport",type=string,JSONPath=`.spec.transport`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Tools",type=integer,JSONPath=`.status.observedTools`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// MCPServer is the Schema for the mcpservers API (ADR-042): a declared MCP
// server — transport, endpoint/command, credentials, tool envelope, egress
// linkage — that Skill.spec.mcpToolRefs resolve against.
type MCPServer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MCPServerSpec   `json:"spec,omitempty"`
	Status MCPServerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MCPServerList contains a list of MCPServer.
type MCPServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MCPServer `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MCPServer{}, &MCPServerList{})
}
