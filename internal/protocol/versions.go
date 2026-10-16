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

// Package protocol pins the external-spec protocol versions the shim seam
// speaks (story 5.3, arch §7.4, OQ12/R11). Every touchpoint with an
// external spec — A2A southbound task lifecycle, MCP tool surface — reads
// its version from HERE, never inline. A version bump is a one-line change
// in this file that flows to the adapter; it must never reach into core.
//
// The seam discipline: core code imports the constants, adapters honor
// them, and upstream A2A/MCP churn is absorbed by bumping a pin here plus
// its adapter — no core rewrite (R11).
package protocol

// Pinned external-spec versions the v1 shim set is built against. These are
// advertised on the Agent Card (story 5.2) so a peer can negotiate against
// a known, explicit protocol revision rather than an implicit "latest".
const (
	// A2AVersion pins the Agent-to-Agent protocol revision the shim serves
	// southbound task lifecycle / SSE progress / artifact channels against
	// (arch §7.1). Bump here + the A2A adapter on an upstream A2A release.
	A2AVersion = "0.2.0"

	// MCPVersion pins the Model Context Protocol revision the runtime's
	// tool surface is exercised against. Bump here + the MCP adapter on an
	// upstream MCP release.
	MCPVersion = "2025-06-18"
)

// Versions is the immutable pinned set, rendered onto the Agent Card and
// returned by conformance so a peer/vendor can assert exact wire revisions.
type Versions struct {
	A2A string `json:"a2a"`
	MCP string `json:"mcp"`
}

// Pinned returns the compiled-in protocol pins. It is a function, not an
// exported var, so callers cannot mutate the seam's source of truth.
func Pinned() Versions {
	return Versions{A2A: A2AVersion, MCP: MCPVersion}
}
