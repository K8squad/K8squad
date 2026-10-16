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

// Package a2a is k8squad's host-side re-export seam over the generic AgentMesh
// A2A wire types (github.com/K8sAgentMesh/K8sAgentMesh/a2a, lifted in roadmap
// P3 — AgentMesh chapter §1A, leak L2 of the ISI-5827 extraction spec). Every
// wire type below is a type alias, so coord/rundrive/shim keep compiling
// against the same names and the JSON bytes on the wire do not change. What
// stays k8squad-side on purpose:
//
//   - SchemaVersion — the ksquad-branded Agent Card schema tag; the module
//     ships an unbranded a2awire.DefaultSchemaVersion for hosts without
//     their own branding.
//   - MCPEndpointsFromCapability / MCPEndpointsToCapability — the
//     conversion between the host's assembly-side MCP IR
//     (pkg/capability.Endpoint, whose secret/egress refs are CRD-backed) and
//     the module's wire IR (MCPEndpoint). The module never sees a CRD or a
//     capability; the CRD shapes stay host #1 behind the seam.
//
// The moved test battery lives in the module (shellcmd, toolerrorsummary,
// wire round-trips); this package keeps the seam pins (mcp_wire_test.go).
package a2a

import (
	a2awire "github.com/K8sAgentMesh/K8sAgentMesh/a2a"
	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/capability"
)

// SchemaVersion is the Agent Card schema tag (spec §6.1), k8squad-branded. It
// is versioned independently of the pinned A2A wire revision
// (internal/protocol.A2AVersion). Hosts without their own branding stamp the
// module's DefaultSchemaVersion instead.
const SchemaVersion = "ksquad.a2a/v1"

// DefaultSchemaVersion re-exports the module's unbranded Agent Card schema
// tag, for symmetry with SchemaVersion at the seam.
const DefaultSchemaVersion = a2awire.DefaultSchemaVersion

// ToolErrorSummaryMax bounds the rune length of a tool-error summary lifted
// onto ToolPayload.Summary (ISI-5211). Re-exported from the module wire type.
const ToolErrorSummaryMax = a2awire.ToolErrorSummaryMax

// Task states (spec §3.1), re-exported from the module.
const (
	TaskSubmitted     = a2awire.TaskSubmitted
	TaskWorking       = a2awire.TaskWorking
	TaskInputRequired = a2awire.TaskInputRequired
	TaskAuthRequired  = a2awire.TaskAuthRequired
	TaskCompleted     = a2awire.TaskCompleted
	TaskFailed        = a2awire.TaskFailed
	TaskCanceled      = a2awire.TaskCanceled
)

// SSE event types (spec §4), re-exported from the module.
const (
	EventStatus       = a2awire.EventStatus
	EventMessage      = a2awire.EventMessage
	EventTool         = a2awire.EventTool
	EventSkillLoad    = a2awire.EventSkillLoad
	EventArtifactRef  = a2awire.EventArtifactRef
	EventUsage        = a2awire.EventUsage
	EventAuthRequired = a2awire.EventAuthRequired
	EventRateLimited  = a2awire.EventRateLimited
)

// Wire types, re-exported as type aliases — identical types, not copies, so
// every k8squad consumer compiles unchanged and the wire bytes do not move.
type (
	TaskState           = a2awire.TaskState
	Envelope            = a2awire.Envelope
	ModelRoute          = a2awire.ModelRoute
	Task                = a2awire.Task
	Status              = a2awire.Status
	EventType           = a2awire.EventType
	Event               = a2awire.Event
	StatusPayload       = a2awire.StatusPayload
	MessagePayload      = a2awire.MessagePayload
	ToolPayload         = a2awire.ToolPayload
	SkillLoadPayload    = a2awire.SkillLoadPayload
	ArtifactRef         = a2awire.ArtifactRef
	UsagePayload        = a2awire.UsagePayload
	AuthRequiredPayload = a2awire.AuthRequiredPayload
	RateLimitedPayload  = a2awire.RateLimitedPayload
	AgentCard           = a2awire.AgentCard
	AgentIdentity       = a2awire.AgentIdentity
	RuntimeInfo         = a2awire.RuntimeInfo
	ModelInfo           = a2awire.ModelInfo
	AuthInfo            = a2awire.AuthInfo
	Capabilities        = a2awire.Capabilities
	Shim                = a2awire.Shim
	MCPEndpoint         = a2awire.MCPEndpoint
)

// ShellCommandHead re-exports the module's shell-head extraction (ISI-4720).
func ShellCommandHead(command string) string { return a2awire.ShellCommandHead(command) }

// ToolErrorSummary re-exports the module's error-summary normalizer
// (ISI-5211).
func ToolErrorSummary(s string) string { return a2awire.ToolErrorSummary(s) }

// MCPEndpointsFromCapability converts the host's assembly-side MCP IR
// (pkg/capability.Endpoint) into the module wire IR where it enters the
// submit payload (rundrive dispatch). Field-for-field; a dropped field here
// is a wire regression caught by TestMCPEndpointSeamRoundTrip.
func MCPEndpointsFromCapability(eps []capability.Endpoint) []MCPEndpoint {
	if eps == nil {
		return nil
	}
	out := make([]MCPEndpoint, 0, len(eps))
	for _, ep := range eps {
		w := MCPEndpoint{
			Name:       ep.Name,
			Transport:  ep.Transport,
			URL:        ep.URL,
			Command:    ep.Command,
			Args:       ep.Args,
			Image:      ep.Image,
			EnvNames:   ep.EnvNames,
			Headers:    ep.Headers,
			AllowTools: ep.AllowTools,
			DenyTools:  ep.DenyTools,
		}
		if ep.CredentialSecretRef != nil {
			w.CredentialSecretRef = &a2awire.SecretRef{
				Name: ep.CredentialSecretRef.Name,
				Key:  ep.CredentialSecretRef.Key,
			}
		}
		if ep.EgressPolicyRef != nil {
			w.EgressPolicyRef = &a2awire.ObjectRef{
				Name:      ep.EgressPolicyRef.Name,
				Namespace: ep.EgressPolicyRef.Namespace,
			}
		}
		out = append(out, w)
	}
	return out
}

// MCPEndpointsToCapability converts the module wire IR back into the host's
// assembly-side IR where a decoded Task feeds the host's renderers (shim
// engine). Field-for-field; the inverse of MCPEndpointsFromCapability.
func MCPEndpointsToCapability(eps []MCPEndpoint) []capability.Endpoint {
	if eps == nil {
		return nil
	}
	out := make([]capability.Endpoint, 0, len(eps))
	for _, ep := range eps {
		h := capability.Endpoint{
			Name:       ep.Name,
			Transport:  ep.Transport,
			URL:        ep.URL,
			Command:    ep.Command,
			Args:       ep.Args,
			Image:      ep.Image,
			EnvNames:   ep.EnvNames,
			Headers:    ep.Headers,
			AllowTools: ep.AllowTools,
			DenyTools:  ep.DenyTools,
		}
		if ep.CredentialSecretRef != nil {
			h.CredentialSecretRef = &api.SecretRef{
				Name: ep.CredentialSecretRef.Name,
				Key:  ep.CredentialSecretRef.Key,
			}
		}
		if ep.EgressPolicyRef != nil {
			h.EgressPolicyRef = &api.ObjectRef{
				Name:      ep.EgressPolicyRef.Name,
				Namespace: ep.EgressPolicyRef.Namespace,
			}
		}
		out = append(out, h)
	}
	return out
}
