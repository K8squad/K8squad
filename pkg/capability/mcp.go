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

package capability

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/toolchain"
)

// MCPError is a fail-closed Run-assembly rejection over an MCP server
// (ADR-042/044). It is actionable by construction: it names the server,
// the demanding skill, and the exact remediation.
type MCPError struct {
	Server  string
	Skill   string
	Reason  string
	Details string
}

func (e *MCPError) Error() string {
	return fmt.Sprintf("mcp server %q: %s%s", e.Server, e.Reason, e.suffix())
}

func (e *MCPError) suffix() string {
	if e.Skill != "" {
		return fmt.Sprintf(" (required via run skill %s)", e.Skill)
	}
	if e.Details != "" {
		return " (" + e.Details + ")"
	}
	return ""
}

// Endpoint is the normalized MCP IR (spike B): one MCP server a Run is
// wired to, transport-flattened and credential-free. The IR is what Run
// assembly records on Run.status, projects into the sandbox as
// K8SQUAD_MCP_CONFIG, and the per-runtime renderers
// (runtimecfg.go) consume — one IR, four renderers.
type Endpoint struct {
	// Name is the MCPServer object name (also the server key in every
	// runtime-native config).
	Name string `json:"name"`

	// Transport is "stdio" or "streamable-http".
	Transport string `json:"transport"`

	// URL is the streamable-http endpoint (empty for stdio).
	URL string `json:"url,omitempty"`

	// Command is the stdio executable (empty for streamable-http).
	Command string `json:"command,omitempty"`

	// Args are the stdio command arguments.
	Args []string `json:"args,omitempty"`

	// Image is the stdio server's packaged image; non-empty means Run
	// assembly stages a native sidecar container running it.
	Image string `json:"image,omitempty"`

	// EnvNames are the environment variable NAMES the credential ref maps
	// to — never values. Rendered runtime configs reference these names
	// so secret material only ever lives in process env (ADR-045 D5).
	EnvNames []string `json:"envNames,omitempty"`

	// Headers are the static non-secret HTTP headers (streamable-http).
	Headers map[string]string `json:"headers,omitempty"`

	// AllowTools is the effective allow set (observed tools narrowed by
	// the server envelope, deny globs subtracted).
	AllowTools []string `json:"allowTools,omitempty"`

	// DenyTools is the server envelope's deny globs, recorded verbatim.
	DenyTools []string `json:"denyTools,omitempty"`

	// CredentialSecretRef names the BYO Secret backing this server's
	// credential — a NAME for the pod seam's SecretKeyRef projection,
	// never material (ADR-045 D5). The IR is safe to project into the
	// sandbox and record on status precisely because it stops here.
	CredentialSecretRef *api.SecretRef `json:"credentialSecretRef,omitempty"`

	// EgressPolicyRef names the EgressPolicy covering a streamable-http
	// endpoint (R1). Provenance only.
	EgressPolicyRef *api.ObjectRef `json:"egressPolicyRef,omitempty"`
}

// CredentialEnvName derives the deterministic env var name a server's
// credential is mounted as: KSQUAD_MCP_<UPPER(NAME)>_TOKEN. The env VALUE
// is a SecretKeyRef projection — the control plane never reads the Secret.
func CredentialEnvName(serverName string) string {
	normalized := strings.NewReplacer("-", "_", ".", "_", "/", "_").Replace(serverName)
	return "KSQUAD_MCP_" + strings.ToUpper(normalized) + "_TOKEN"
}

// defaultCredentialKey is the Secret key read when spec.credentialSecretRef
// carries no explicit key (the catalog convention).
const defaultCredentialKey = "token"

// ResolveMCP resolves a Run's MCP demand fail-closed (ADR-044 step 4):
//
//   - every referenced MCPServer must exist (admission cache may be stale);
//   - status.observedTools must be non-empty (ADR-042 staleness: a Run
//     never admits against an unknown tool surface);
//   - the effective allow set — server.toolFilter.allow (empty = observed
//     tools) minus deny globs — must be non-empty (a glob overlap that
//     nullifies the grant fails closed instead of passing silently);
//   - the result carries NO credential material: the credential rides as
//     an env NAME (SecretKeyRef projection at the pod seam).
//
// It returns the IR endpoints AND the resolved server objects (the egress
// re-assertion in egress.go needs the source objects; one fetch serves
// both). Both slices are sorted by server name for stable manifest bytes.
func ResolveMCP(ctx context.Context, reader client.Reader, run *api.Run, reqs *Requirements) ([]Endpoint, []*api.MCPServer, error) {
	details := toolchain.DetailsFor(run)
	endpoints := make([]Endpoint, 0, len(reqs.MCPRefs))
	servers := make([]*api.MCPServer, 0, len(reqs.MCPRefs))

	for _, ref := range reqs.MCPRefs {
		ns := ref.Namespace
		if ns == "" {
			ns = run.Namespace
		}
		key := ns + "/" + ref.Name
		var server api.MCPServer
		if err := reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &server); err != nil {
			if isNotFound(err) {
				return nil, nil, &MCPError{Server: key, Skill: reqs.MCPSources[key],
					Reason: "referenced MCPServer does not exist; apply the MCPServer or drop the skill's mcpToolRefs entry", Details: details}
			}
			return nil, nil, fmt.Errorf("read mcpserver %s (fail-closed): %w", key, err)
		}

		ep, err := endpointFor(run, reqs, key, &server)
		if err != nil {
			return nil, nil, err
		}
		endpoints = append(endpoints, *ep)
		servers = append(servers, server.DeepCopy())
	}

	sort.Slice(endpoints, func(i, j int) bool { return endpoints[i].Name < endpoints[j].Name })
	sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
	return endpoints, servers, nil
}

// endpointFor computes one server's IR with its effective tool filter.
func endpointFor(run *api.Run, reqs *Requirements, key string, server *api.MCPServer) (*Endpoint, error) {
	details := toolchain.DetailsFor(run)
	spec := server.Spec

	// ADR-042 staleness: no discovery has ever succeeded → the tool
	// surface is unknown → fail closed (CEL guards shape at admission;
	// this re-asserts observed reality against drift).
	if len(server.Status.ObservedTools) == 0 {
		return nil, &MCPError{Server: key, Skill: reqs.MCPSources[key],
			Reason: "status.observedTools is empty (no successful discovery probe); wait for the ToolsDiscovered condition or check the server", Details: details}
	}

	ep := &Endpoint{
		Name:                server.Name,
		Transport:           string(spec.Transport),
		URL:                 spec.Endpoint,
		Command:             spec.Command,
		Args:                append([]string(nil), spec.Args...),
		Image:               spec.Image,
		Headers:             spec.Headers,
		DenyTools:           denyGlobs(spec.ToolFilter),
		EnvNames:            []string{},
		CredentialSecretRef: spec.CredentialSecretRef,
		EgressPolicyRef:     spec.EgressRef,
	}

	switch spec.Transport {
	case api.MCPTransportStdio:
		if spec.Command == "" {
			return nil, &MCPError{Server: key, Skill: reqs.MCPSources[key],
				Reason: "transport=stdio but spec.command is empty (drift past admission); re-apply the MCPServer with a command", Details: details}
		}
	case api.MCPTransportStreamableHTTP:
		if spec.Endpoint == "" {
			return nil, &MCPError{Server: key, Skill: reqs.MCPSources[key],
				Reason: "transport=streamable-http but spec.endpoint is empty (drift past admission); re-apply the MCPServer with an endpoint", Details: details}
		}
	default:
		return nil, &MCPError{Server: key, Skill: reqs.MCPSources[key],
			Reason: fmt.Sprintf("transport %q is outside the v1alpha1 enum (drift past admission)", spec.Transport), Details: details}
	}

	if spec.CredentialSecretRef != nil && spec.CredentialSecretRef.Name != "" {
		ep.EnvNames = append(ep.EnvNames, CredentialEnvName(server.Name))
	}

	// Effective allow (ADR-044 step 4): server envelope allow (empty =
	// observed tools) − deny globs. Empty effective set fails closed —
	// a glob overlap (e.g. allow create_* vs deny create_issue applied to
	// a surface of exactly create_issue) nullifies the grant.
	allowed := allowSet(spec.ToolFilter, server.Status.ObservedTools)
	ep.AllowTools = subtractGlobs(allowed, ep.DenyTools)
	if len(ep.AllowTools) == 0 {
		return nil, &MCPError{Server: key, Skill: reqs.MCPSources[key],
			Reason: fmt.Sprintf("effective allow set is empty after the toolFilter was applied (allow: %v, deny: %v, observed: %d tools); a grant of nothing is a misconfiguration",
				allowGlobs(spec.ToolFilter), ep.DenyTools, len(server.Status.ObservedTools)), Details: details}
	}
	return ep, nil
}

func allowGlobs(f *api.MCPToolFilter) []string {
	if f == nil {
		return nil
	}
	return f.Allow
}

func denyGlobs(f *api.MCPToolFilter) []string {
	if f == nil {
		return nil
	}
	return f.Deny
}

// allowSet expands the envelope's allow globs against the observed surface.
// An empty/absent allow list grants all observed tools (ADR-042).
func allowSet(filter *api.MCPToolFilter, observed []string) []string {
	globs := allowGlobs(filter)
	if len(globs) == 0 {
		return append([]string(nil), observed...)
	}
	var out []string
	for _, tool := range observed {
		for _, glob := range globs {
			if globMatch(glob, tool) {
				out = append(out, tool)
				break
			}
		}
	}
	return out
}

// subtractGlobs removes every tool matching any deny glob. Input order is
// preserved (stable manifest bytes).
func subtractGlobs(tools, denyGlobs []string) []string {
	if len(denyGlobs) == 0 {
		return tools
	}
	out := make([]string, 0, len(tools))
	for _, tool := range tools {
		denied := false
		for _, glob := range denyGlobs {
			if globMatch(glob, tool) {
				denied = true
				break
			}
		}
		if !denied {
			out = append(out, tool)
		}
	}
	return out
}

// globMatch implements the tool-filter glob dialect (path.Match subset:
// '*' segments only — the vocabulary MCPServer.toolFilter documents).
func globMatch(glob, tool string) bool {
	if glob == "*" {
		return true
	}
	if !strings.ContainsAny(glob, "*?[") {
		return glob == tool
	}
	matched, err := path.Match(glob, tool)
	if err != nil {
		// A malformed glob can never be proven to grant — fail closed
		// by not matching (the empty-effective-set guard then rejects).
		return false
	}
	return matched
}
