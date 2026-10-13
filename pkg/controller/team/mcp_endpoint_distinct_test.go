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

package team

import (
	"strings"
	"testing"

	"github.com/K8squad/K8squad/internal/memory"
)

// TestBuiltinMemoryEndpointsAreDistinct pins the core ISI-5694 invariant: the three built-in memory
// MCPServers (authoring / discussion / search) MUST each carry a distinct Endpoint URL. They all
// target the one in-cluster memory Service, but opencode's MCP client dedupes remote servers by URL —
// so if any two share a URL, opencode collapses them into a single connection presenting a single
// bearer token, and a call like discussion_post rides the wrong scope's token (observed live:
// caps=[work_item.search], losing the coordinator cap) → the party round stalls at 0 voices. The
// memory handler serves all three alias paths and still scopes every tools/call by the verified
// token's capabilities, so distinct paths change only connection identity, not authorization.
func TestBuiltinMemoryEndpointsAreDistinct(t *testing.T) {
	const cpNS = SystemNamespace
	team := newTeam("alpha", "uid-alpha")

	eps := map[string]string{
		AuthoringMCPServerName:  authoringMCPServer("squad-alpha", team, cpNS).Spec.Endpoint,
		DiscussionMCPServerName: discussionMCPServer("squad-alpha", team, cpNS).Spec.Endpoint,
		SearchMCPServerName:     searchMCPServer("squad-alpha", team, cpNS).Spec.Endpoint,
	}

	wantSuffix := map[string]string{
		AuthoringMCPServerName:  memory.MCPEndpointAuthoring,
		DiscussionMCPServerName: memory.MCPEndpointDiscussion,
		SearchMCPServerName:     memory.MCPEndpointSearch,
	}

	// Each endpoint carries its own per-scope path, and none regresses to the bare (collapsing) /mcp.
	for name, ep := range eps {
		if !strings.HasSuffix(ep, wantSuffix[name]) {
			t.Errorf("%s endpoint = %q, want suffix %q", name, ep, wantSuffix[name])
		}
		if strings.HasSuffix(ep, ".svc.cluster.local:8080"+memory.MCPEndpoint) {
			t.Errorf("%s endpoint = %q regressed to the shared %q path — opencode would collapse the servers (ISI-5694)", name, ep, memory.MCPEndpoint)
		}
	}

	// The invariant that matters to opencode: pairwise-distinct URLs.
	seen := map[string]string{}
	for name, ep := range eps {
		if other, dup := seen[ep]; dup {
			t.Fatalf("endpoints collapse: %s and %s share URL %q — opencode keys connections by URL and would present one token for both (ISI-5694)", name, other, ep)
		}
		seen[ep] = name
	}

	// The alias paths themselves must be distinct constants.
	if memory.MCPEndpointAuthoring == memory.MCPEndpointDiscussion ||
		memory.MCPEndpointAuthoring == memory.MCPEndpointSearch ||
		memory.MCPEndpointDiscussion == memory.MCPEndpointSearch {
		t.Fatalf("per-scope alias paths are not pairwise distinct: authoring=%q discussion=%q search=%q",
			memory.MCPEndpointAuthoring, memory.MCPEndpointDiscussion, memory.MCPEndpointSearch)
	}
}
