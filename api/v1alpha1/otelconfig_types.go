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

// ExportProtocol enumerates the OTLP wire protocol used to ship a signal to
// its endpoint (arch §5.1, story 1.5).
// +kubebuilder:validation:Enum=grpc;http/protobuf;http/json
type ExportProtocol string

const (
	// ExportProtocolGRPC exports the signal over gRPC (host:port or
	// https?://host[:port]; no default port is assumed).
	ExportProtocolGRPC ExportProtocol = "grpc"
	// ExportProtocolHTTPProtobuf exports the signal over OTLP/HTTP with
	// protobuf-encoded bodies. The endpoint must be a full http(s) URL.
	ExportProtocolHTTPProtobuf ExportProtocol = "http/protobuf"
	// ExportProtocolHTTPJSON exports the signal over OTLP/HTTP with JSON
	// bodies. The endpoint must be a full http(s) URL.
	ExportProtocolHTTPJSON ExportProtocol = "http/json"
)

// SamplingType enumerates the head sampler applied to traces.
// +kubebuilder:validation:Enum=always_on;always_off;probabilistic
type SamplingType string

const (
	// SamplingTypeAlwaysOn keeps every span.
	SamplingTypeAlwaysOn SamplingType = "always_on"
	// SamplingTypeAlwaysOff drops every span at the head sampler.
	SamplingTypeAlwaysOff SamplingType = "always_off"
	// SamplingTypeProbabilistic keeps a ratio fraction of traces. Ratio is
	// required and must be in (0, 1] — a ratio of 0 is always_off.
	SamplingTypeProbabilistic SamplingType = "probabilistic"
)

// SecretKeyReference points at a single key inside a Secret. The referenced
// value is injected as the `Authorization` header value on every export
// request for the signal (arch §5.1). When Namespace is omitted the secret is
// resolved in the k8squad system namespace by the consuming component.
type SecretKeyReference struct {
	// Name of the Secret containing the auth value.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`

	// Key inside the Secret holding the auth value (e.g. "token").
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	Key string `json:"key"`

	// Namespace where the Secret lives. Defaults to the k8squad system
	// namespace when omitted.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace string `json:"namespace,omitempty"`
}

// SamplingConfig configures head sampling. It is only valid on the traces
// signal; CRD and webhook validation reject it on metrics and logs.
type SamplingConfig struct {
	// Type selects the sampler.
	Type SamplingType `json:"type"`

	// Ratio is the fraction of traces kept when Type is probabilistic.
	// Required and in (0, 1] for probabilistic; forbidden otherwise.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	Ratio *float64 `json:"ratio,omitempty"`
}

// SignalRouting describes where and how one telemetry signal is exported.
// Absent signal = not exported: the CRD ships nothing by default (opt-in,
// arch §5.1 story 1.5).
type SignalRouting struct {
	// Endpoint the signal is shipped to. For grpc this is host[:port] with an
	// optional https?:// scheme and no path; for http/protobuf and http/json
	// this is a full http(s) URL (typically .../v1/traces, .../v1/metrics,
	// .../v1/logs).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	Endpoint string `json:"endpoint"`

	// Protocol used to export the signal.
	Protocol ExportProtocol `json:"protocol"`

	// Auth references a Secret key whose value is presented as the
	// Authorization header on export requests.
	// +optional
	Auth *SecretKeyReference `json:"auth,omitempty"`

	// ResourceAttributes are merged into the OTel resource attributes of the
	// exported signal. Keys owned by the platform (service.instance.id) are
	// rejected.
	// +optional
	// +kubebuilder:validation:MaxProperties=64
	ResourceAttributes map[string]string `json:"resourceAttributes,omitempty"`

	// Sampling configures the head sampler. Traces only.
	// +optional
	Sampling *SamplingConfig `json:"sampling,omitempty"`
}

// ToolUsageConfig gates the Epic D tool-usage instrumentation pipeline
// (plan §2.4, story D2): the GenAI-semconv spans (gen_ai.tool.call,
// skill.load, mcp.call) and the ksquad_tool_calls_total /
// ksquad_skill_loads_total / ksquad_mcp_call_duration_seconds metrics. It is
// independent of the signal routings above — tool-usage telemetry rides
// whatever traces/metrics routing is configured, and disabling it only stops
// the tool/skill/MCP instrumentation, not the platform's own telemetry.
type ToolUsageConfig struct {
	// Enabled turns the tool-usage pipeline on or off platform-wide.
	// Absent defaults to true (opt-out, plan §5.4): tool-usage telemetry
	// ships unless explicitly disabled.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
}

// EnabledOrDefault resolves the effective gate value: absent = enabled.
func (t *ToolUsageConfig) EnabledOrDefault() bool {
	if t == nil || t.Enabled == nil {
		return true
	}
	return *t.Enabled
}

// OTelConfigSpec defines the desired state of OTelConfig. At least one signal
// must be configured; a spec with no signals is rejected as meaningless.
type OTelConfigSpec struct {
	// Traces routing. Optional — omit to not export traces.
	// +optional
	// +kubebuilder:validation:XValidation:rule=`!has(self) || self.endpoint.matches('^[^[:space:]]+$')`,message="endpoint must not contain whitespace"
	// +kubebuilder:validation:XValidation:rule=`!has(self) || self.protocol != 'grpc' || self.endpoint.matches('^(https?://)?[^/[:space:]]+(:[[:digit:]]+)?$')`,message="grpc endpoint must be host[:port] with an optional http(s) scheme and no path"
	// +kubebuilder:validation:XValidation:rule=`!has(self) || !self.protocol.startsWith('http/') || self.endpoint.matches('^https?://[^[:space:]]+$')`,message="http/protobuf and http/json endpoints must be full http(s) URLs"
	// +kubebuilder:validation:XValidation:rule=`!has(self) || !has(self.sampling) || self.sampling.type != 'probabilistic' || (has(self.sampling.ratio) && self.sampling.ratio > 0)`,message="sampling.ratio is required and must be > 0 when sampling.type is probabilistic"
	// +kubebuilder:validation:XValidation:rule=`!has(self) || !has(self.sampling) || self.sampling.type == 'probabilistic' || !has(self.sampling.ratio)`,message="sampling.ratio must only be set when sampling.type is probabilistic"
	Traces *SignalRouting `json:"traces,omitempty"`

	// Metrics routing. Optional — omit to not export metrics.
	// +optional
	// +kubebuilder:validation:XValidation:rule=`!has(self) || self.endpoint.matches('^[^[:space:]]+$')`,message="endpoint must not contain whitespace"
	// +kubebuilder:validation:XValidation:rule=`!has(self) || self.protocol != 'grpc' || self.endpoint.matches('^(https?://)?[^/[:space:]]+(:[[:digit:]]+)?$')`,message="grpc endpoint must be host[:port] with an optional http(s) scheme and no path"
	// +kubebuilder:validation:XValidation:rule=`!has(self) || !self.protocol.startsWith('http/') || self.endpoint.matches('^https?://[^[:space:]]+$')`,message="http/protobuf and http/json endpoints must be full http(s) URLs"
	// +kubebuilder:validation:XValidation:rule=`!has(self) || !has(self.sampling)`,message="sampling is only valid on traces"
	Metrics *SignalRouting `json:"metrics,omitempty"`

	// Logs routing. Optional — omit to not export logs.
	// +optional
	// +kubebuilder:validation:XValidation:rule=`!has(self) || self.endpoint.matches('^[^[:space:]]+$')`,message="endpoint must not contain whitespace"
	// +kubebuilder:validation:XValidation:rule=`!has(self) || self.protocol != 'grpc' || self.endpoint.matches('^(https?://)?[^/[:space:]]+(:[[:digit:]]+)?$')`,message="grpc endpoint must be host[:port] with an optional http(s) scheme and no path"
	// +kubebuilder:validation:XValidation:rule=`!has(self) || !self.protocol.startsWith('http/') || self.endpoint.matches('^https?://[^[:space:]]+$')`,message="http/protobuf and http/json endpoints must be full http(s) URLs"
	// +kubebuilder:validation:XValidation:rule=`!has(self) || !has(self.sampling)`,message="sampling is only valid on traces"
	Logs *SignalRouting `json:"logs,omitempty"`

	// ToolUsage gates the Epic D tool-usage instrumentation pipeline
	// (gen_ai.tool.call / skill.load / mcp.call spans and the ksquad_* tool
	// metrics). Absent = enabled (opt-out, plan §5.4).
	// +optional
	ToolUsage *ToolUsageConfig `json:"toolUsage,omitempty"`
}

// OTelConfigStatus defines the observed state of OTelConfig.
type OTelConfigStatus struct {
	// ObservedGeneration is the metadata.generation most recently reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,path=otelconfigs,shortName=otelcfg
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Since",type="date",JSONPath=`.status.conditions[?(@.type=="Ready")].lastTransitionTime`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=`.metadata.creationTimestamp`

// OTelConfig is the platform-scoped, declarative telemetry routing contract
// (arch §5.1, story 1.5). It is cluster-scoped because telemetry routing is a
// platform concern, not a Team concern. Default is opt-in: no signal blocks
// means nothing is exported anywhere.
//
// Validation is layered: CEL rules embedded in the CRD (structural,
// enforced by the API server on 1.25+) plus a validating webhook that mirrors
// the CEL checks and adds checks CEL cannot express (see
// otelconfig_webhook.go).
type OTelConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:XValidation:rule=`has(self.traces) || has(self.metrics) || has(self.logs)`,message="at least one signal (traces, metrics, logs) must be configured — OTelConfig exports nothing by default"
	Spec   OTelConfigSpec   `json:"spec,omitempty"`
	Status OTelConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// OTelConfigList contains a list of OTelConfig.
type OTelConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OTelConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OTelConfig{}, &OTelConfigList{})
}
