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

package memory

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMCP_AliasPathsServeHandler (ISI-5694): the per-scope alias paths must all route to the same
// JSON-RPC handler as the canonical /mcp. The three built-in memory MCPServers now advertise distinct
// URLs (…/mcp/authoring, …/mcp/discussion, …/mcp/search) so opencode keeps three separate connections
// — one scoped token each — instead of collapsing same-URL servers into one credential. That only
// works if the memory server actually answers on each alias path; a 404 here would silently break the
// discussion_post → coordinator path the party round depends on.
func TestMCP_AliasPathsServeHandler(t *testing.T) {
	mux := mountMCP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()), nil)

	paths := []string{MCPEndpoint, MCPEndpointAuthoring, MCPEndpointDiscussion, MCPEndpointSearch}
	for _, path := range paths {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path,
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`))
		mux.ServeHTTP(rec, req)

		if rec.Code == http.StatusNotFound {
			t.Fatalf("path %q is not routed to the MCP handler (404) — opencode would fail to reach the scoped endpoint (ISI-5694)", path)
		}
		var resp jsonrpcResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("path %q: decode rpc response: %v (code=%d body=%q)", path, err, rec.Code, rec.Body.String())
		}
		if resp.Error != nil {
			t.Fatalf("path %q: initialize error: %+v", path, resp.Error)
		}
		var r struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		b, _ := json.Marshal(resp.Result)
		_ = json.Unmarshal(b, &r)
		if r.ProtocolVersion != "2025-06-18" {
			t.Fatalf("path %q: protocolVersion = %q, want echoed 2025-06-18 (handler not reached)", path, r.ProtocolVersion)
		}
	}
}
