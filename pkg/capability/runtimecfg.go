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
	"encoding/json"
	"fmt"
	"sort"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// The per-runtime MCP config render matrix (spike B; ADR-044 step 6): ONE
// normalized IR, four small renderers — each emits the native config the
// runtime consumes, with credentials referenced as process env (never
// literal — ADR-045 D5: config files persist in workspaces/artifacts, env
// does not).
//
// Tool scoping ("the runtime's MCP config contains github-mcp scoped to
// allowed tools" — plan §5.2): claude-code's .mcp.json has no native
// allow/deny vocabulary, so the renderer carries the effective filter as
// x-ksquad-allow-tools/x-ksquad-deny-tools extension keys the shim's
// claude-code wrapper enforces; opencode and openclaw have native
// tool-enable/disable surfaces, rendered natively; hermes consumes the IR
// itself via HERMES_MCP_CONFIG passthrough.

// claudeMCPEntry is one server in a .mcp.json mcpServers map.
type claudeMCPEntry struct {
	Type    string            `json:"type"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`

	// x-ksquad extension keys carry the effective tool filter for
	// runtimes without a native vocabulary (enforced by the shim
	// wrapper; documented contract, ignored as unknown by the CLI).
	XKsquadAllowTools []string `json:"x-ksquad-allow-tools,omitempty"`
	XKsquadDenyTools  []string `json:"x-ksquad-deny-tools,omitempty"`
}

type claudeMCPDoc struct {
	MCPServers map[string]claudeMCPEntry `json:"mcpServers"`
}

// RenderClaudeCode renders the claude-code `.mcp.json` for the endpoint
// set: stdio servers as command entries (credential env referenced via
// ${VAR} expansion), streamable-http as http entries.
func RenderClaudeCode(endpoints []Endpoint) ([]byte, error) {
	doc := claudeMCPDoc{MCPServers: map[string]claudeMCPEntry{}}
	for _, ep := range endpoints {
		entry := claudeMCPEntry{
			XKsquadAllowTools: ep.AllowTools,
			XKsquadDenyTools:  ep.DenyTools,
		}
		switch ep.Transport {
		case string(api.MCPTransportStdio):
			entry.Type = "stdio"
			entry.Command = ep.Command
			entry.Args = ep.Args
			for _, name := range ep.EnvNames {
				if entry.Env == nil {
					entry.Env = map[string]string{}
				}
				entry.Env[name] = "${" + name + "}"
			}
		case string(api.MCPTransportStreamableHTTP):
			entry.Type = "http"
			entry.URL = ep.URL
			entry.Headers = ep.Headers
			for _, name := range ep.EnvNames {
				if entry.Headers == nil {
					entry.Headers = map[string]string{}
				}
				entry.Headers["Authorization"] = "Bearer ${" + name + "}"
			}
		default:
			return nil, fmt.Errorf("claude-code renderer: unknown transport %q for server %q", ep.Transport, ep.Name)
		}
		doc.MCPServers[ep.Name] = entry
	}
	return json.MarshalIndent(doc, "", "  ")
}

// opencodeMCPEntry is one server in opencode.json's "mcp" section.
type opencodeMCPEntry struct {
	Type    string            `json:"type"`
	Command string            `json:"command,omitempty"`
	URL     string            `json:"url,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`

	// opencode's native per-server tool scoping.
	Tools *opencodeToolScope `json:"tools,omitempty"`
}

type opencodeToolScope struct {
	Enable  []string `json:"enable,omitempty"`
	Disable []string `json:"disable,omitempty"`
}

type opencodeMCPDoc struct {
	MCP map[string]opencodeMCPEntry `json:"mcp"`
}

// RenderOpenCode renders the opencode `mcp` config section: local servers
// as command entries with {env:VAR} credential references, remote as
// remote entries — tools.enable carries the effective allow set natively.
func RenderOpenCode(endpoints []Endpoint) ([]byte, error) {
	doc := opencodeMCPDoc{MCP: map[string]opencodeMCPEntry{}}
	for _, ep := range endpoints {
		entry := opencodeMCPEntry{
			Tools: &opencodeToolScope{Enable: ep.AllowTools, Disable: ep.DenyTools},
		}
		switch ep.Transport {
		case string(api.MCPTransportStdio):
			entry.Type = "local"
			entry.Command = ep.Command
			for _, name := range ep.EnvNames {
				if entry.Env == nil {
					entry.Env = map[string]string{}
				}
				entry.Env[name] = "{env:" + name + "}"
			}
		case string(api.MCPTransportStreamableHTTP):
			entry.Type = "remote"
			entry.URL = ep.URL
			entry.Headers = ep.Headers
			for _, name := range ep.EnvNames {
				if entry.Headers == nil {
					entry.Headers = map[string]string{}
				}
				entry.Headers["Authorization"] = "Bearer {env:" + name + "}"
			}
		default:
			return nil, fmt.Errorf("opencode renderer: unknown transport %q for server %q", ep.Transport, ep.Name)
		}
		doc.MCP[ep.Name] = entry
	}
	return json.MarshalIndent(doc, "", "  ")
}

// openclawMCPEntry is one server in openclaw's mcp.servers section.
type openclawMCPEntry struct {
	Type    string            `json:"type"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	URL     string            `json:"url,omitempty"`
	Env     map[string]string `json:"env,omitempty"`

	// openclaw's native tool allow/deny per server.
	AllowedTools []string `json:"allowedTools,omitempty"`
	DeniedTools  []string `json:"deniedTools,omitempty"`
}

type openclawMCPDoc struct {
	MCP struct {
		Servers map[string]openclawMCPEntry `json:"servers"`
	} `json:"mcp"`
}

// RenderOpenClaw renders the openclaw `mcp.servers` section: stdio as
// command entries, streamable-http as url entries, credentials as env
// name references (openclaw env-passthrough resolves them).
func RenderOpenClaw(endpoints []Endpoint) ([]byte, error) {
	var doc openclawMCPDoc
	doc.MCP.Servers = map[string]openclawMCPEntry{}
	for _, ep := range endpoints {
		entry := openclawMCPEntry{
			AllowedTools: ep.AllowTools,
			DeniedTools:  ep.DenyTools,
		}
		switch ep.Transport {
		case string(api.MCPTransportStdio):
			entry.Type = "stdio"
			entry.Command = ep.Command
			entry.Args = ep.Args
			for _, name := range ep.EnvNames {
				if entry.Env == nil {
					entry.Env = map[string]string{}
				}
				entry.Env[name] = "${" + name + "}"
			}
		case string(api.MCPTransportStreamableHTTP):
			entry.Type = "http"
			entry.URL = ep.URL
		default:
			return nil, fmt.Errorf("openclaw renderer: unknown transport %q for server %q", ep.Transport, ep.Name)
		}
		doc.MCP.Servers[ep.Name] = entry
	}
	return json.MarshalIndent(doc, "", "  ")
}

// RenderHermes renders the hermes passthrough: hermes consumes the
// normalized IR itself via HERMES_MCP_CONFIG, so the "renderer" is the IR
// document (env-name references included; the shim resolves them).
func RenderHermes(endpoints []Endpoint) ([]byte, error) {
	return RenderMCPConfigData(endpoints)
}

// SortedAllowTools returns the endpoint's allow set sorted — a stable
// helper for tests and console surfaces.
func SortedAllowTools(ep Endpoint) []string {
	out := append([]string(nil), ep.AllowTools...)
	sort.Strings(out)
	return out
}
