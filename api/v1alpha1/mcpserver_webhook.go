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
	"context"
	"fmt"
	"net/url"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// SetupMCPServerWebhookWithManager registers the MCPServer validating
// webhook with the manager. The webhook mirrors the CRD's CEL pairing and
// filter-overlap rules (defense in depth for clusters where CEL surfaced
// errors are undesired) and adds the checks CEL cannot express well:
// endpoint URL parseability, secret-looking header names (credentials ride
// credentialSecretRef, never the CRD — ADR-045), and reserved runtime
// server names (spike B.1/B.2: names the runtime clients own).
func SetupMCPServerWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &MCPServer{}).
		WithValidator(&MCPServer{}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-ksquad-io-v1alpha1-mcpserver,mutating=false,failurePolicy=fail,sideEffects=None,groups=ksquad.io,resources=mcpservers,verbs=create;update,versions=v1alpha1,name=vmcpserver-v1alpha1.ksquad.io,admissionReviewVersions=v1

var _ admission.Validator[*MCPServer] = &MCPServer{}

// ValidateCreate implements admission.Validator.
func (r *MCPServer) ValidateCreate(_ context.Context, srv *MCPServer) (admission.Warnings, error) {
	return validateMCPServer(srv)
}

// ValidateUpdate implements admission.Validator.
func (r *MCPServer) ValidateUpdate(_ context.Context, _, newObj *MCPServer) (admission.Warnings, error) {
	return validateMCPServer(newObj)
}

// ValidateDelete implements admission.Validator.
func (r *MCPServer) ValidateDelete(_ context.Context, _ *MCPServer) (admission.Warnings, error) {
	// Deletion is always allowed; the discovery controller stops probing.
	return nil, nil
}

// secretHeaderNames are header names that carry credential material. They
// must never appear in spec.headers (plain CRD data): credentials ride the
// BYO credentialSecretRef so secret material never persists in etcd or the
// Run workspace (ADR-045 credentials row).
var secretHeaderNames = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
	"set-cookie":          true,
	"x-api-key":           true,
}

// reservedMCPServerNames are server names owned by the runtime clients
// (spike B.1 claude-code, B.2 opencode). An MCPServer with one of these
// names would shadow or collide with a client-reserved slot when rendered
// into per-runtime MCP configs, so admission rejects them up front.
var reservedMCPServerNames = map[string]bool{
	"workspace":        true,
	"computer-use":     true,
	"claude-in-chrome": true,
	"Claude Preview":   true,
	"Claude Browser":   true,
	"__proto__":        true,
}

// validateMCPServer is the single validation entry point shared by create
// and update.
func validateMCPServer(r *MCPServer) (admission.Warnings, error) {
	spec := r.Spec

	// Reserved server names (spike B.1/B.2): rendered MCP configs hand
	// these slots to the runtime itself.
	if reservedMCPServerNames[r.Name] {
		return nil, fmt.Errorf(
			"metadata.name %q is reserved by the runtime MCP clients (spike B.1); rename the MCPServer", r.Name)
	}

	// Transport pairing (mirrors CEL, defense in depth).
	switch spec.Transport {
	case MCPTransportStreamableHTTP:
		if strings.TrimSpace(spec.Endpoint) == "" {
			return nil, fmt.Errorf("spec.endpoint is required when transport is streamable-http")
		}
		if spec.Command != "" {
			return nil, fmt.Errorf("spec.command is forbidden when transport is streamable-http; transport=stdio is the subprocess variant")
		}
		u, err := url.Parse(spec.Endpoint)
		if err != nil {
			return nil, fmt.Errorf("spec.endpoint is not a parsable URL: %w", err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("spec.endpoint must use the http or https scheme: %q", spec.Endpoint)
		}
		if u.Host == "" {
			return nil, fmt.Errorf("spec.endpoint must include a host: %q", spec.Endpoint)
		}
	case MCPTransportStdio:
		if strings.TrimSpace(spec.Command) == "" {
			return nil, fmt.Errorf("spec.command is required when transport is stdio")
		}
		if spec.Endpoint != "" {
			return nil, fmt.Errorf("spec.endpoint is forbidden when transport is stdio; transport=streamable-http is the remote variant")
		}
	default:
		return nil, fmt.Errorf("spec.transport %q is not one of stdio, streamable-http", spec.Transport)
	}

	// Secret-bearing headers are rejected: the credential must ride
	// credentialSecretRef (ADR-045 — never literal in CRD data).
	for name := range spec.Headers {
		if secretHeaderNames[strings.ToLower(name)] || strings.Contains(strings.ToLower(name), "token") {
			return nil, fmt.Errorf(
				"spec.headers[%q] looks like it carries a credential; use spec.credentialSecretRef (BYO Secret) instead — secret material must not persist in the CRD", name)
		}
	}

	// Tool filter literal overlap (mirrors CEL). Glob overlap is a Run
	// assembly fail-closed, not admission (ADR-042 CEL note).
	if tf := spec.ToolFilter; tf != nil {
		deny := map[string]bool{}
		for _, d := range tf.Deny {
			deny[d] = true
		}
		for _, a := range tf.Allow {
			if deny[a] {
				return nil, fmt.Errorf(
					"spec.toolFilter: tool %q appears in both allow and deny; remove the overlap or use a glob", a)
			}
		}
	}

	// credentialSecretRef shape: name required when the block is present
	// (existence is a status condition, not admission — ADR-042 rotation).
	if ref := spec.CredentialSecretRef; ref != nil && strings.TrimSpace(ref.Name) == "" {
		return nil, fmt.Errorf("spec.credentialSecretRef.name must not be empty when credentialSecretRef is set")
	}

	// egressRef shape (existence is the EgressAllowed condition).
	if ref := spec.EgressRef; ref != nil && strings.TrimSpace(ref.Name) == "" {
		return nil, fmt.Errorf("spec.egressRef.name must not be empty when egressRef is set")
	}

	return nil, nil
}
