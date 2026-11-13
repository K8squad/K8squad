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
	"strings"
	"testing"
)

// mcpAdvertisedDiscussionPost digs the discussion_post tool out of a
// toolsList() result (the exact object an MCP client is served).
func mcpAdvertisedDiscussionPost(t *testing.T) mcpTool {
	t.Helper()
	m := NewToolMCP(nil, nil, &fakeDiscussionWriter{})
	res, ok := m.toolsList().(map[string]any)
	if !ok {
		t.Fatalf("toolsList shape: %T", m.toolsList())
	}
	raw, err := json.Marshal(res["tools"])
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	var tools []mcpTool
	if err := json.Unmarshal(raw, &tools); err != nil {
		t.Fatalf("unmarshal tools: %v", err)
	}
	for _, tl := range tools {
		if tl.Name == DiscussionPostToolName {
			return tl
		}
	}
	t.Fatalf("toolsList does not advertise %s (read-only deployment?)", DiscussionPostToolName)
	return mcpTool{}
}

// TestDiscussionPostCatalogCarriesOutputContract pins the WS-7a contract in
// the MCP catalog: the advertised description leads with the mechanics and
// closes with the board-readability guidance, and the body arg itself carries
// the same shape (ISI-7367).
func TestDiscussionPostCatalogCarriesOutputContract(t *testing.T) {
	tool := mcpAdvertisedDiscussionPost(t)
	if tool.Description != DiscussionPostToolDescription {
		t.Errorf("advertised description drifted from the one source of truth")
	}
	for _, want := range []string{
		"board reads your message directly in the room",
		"explicit ask",
		"only for an exact command, path, or identifier",
		"Do not interleave commands with prose",
		"park that detail in work items or linked issues",
		"1200 characters",
	} {
		if !strings.Contains(tool.Description, want) {
			t.Errorf("description missing required phrase %q", want)
		}
	}
	if !strings.Contains(string(tool.InputSchema), DiscussionPostBodyArgDescription) {
		t.Errorf("body arg description does not carry the WS-7a guidance")
	}
}

// TestDiscussionPostHTTPManifestParity is the parity test (ISI-7367): the HTTP
// shim's GET self-description serves EXACTLY the name/description/body-guidance
// the MCP catalog advertises — the two transports cannot drift.
func TestDiscussionPostHTTPManifestParity(t *testing.T) {
	tool := mcpAdvertisedDiscussionPost(t)
	srv := newDiscussionToolServer(t, &fakeDiscussionWriter{})
	defer srv.Close()
	res, err := http.Get(srv.URL + "/mcp/tools/discussion_post")
	if err != nil {
		t.Fatalf("get manifest: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var man struct {
		Name        string            `json:"name"`
		Description string            `json:"description"`
		Args        map[string]string `json:"args"`
	}
	if err := json.NewDecoder(res.Body).Decode(&man); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if man.Name != tool.Name {
		t.Errorf("name: HTTP %q != MCP %q", man.Name, tool.Name)
	}
	if man.Description != tool.Description {
		t.Errorf("description: HTTP and MCP drifted (HTTP %d chars, MCP %d chars)", len(man.Description), len(tool.Description))
	}
	if man.Args["body"] != DiscussionPostBodyArgDescription {
		t.Errorf("body arg: HTTP %q != shared constant", man.Args["body"])
	}
}

// TestDiscussionPostGuideIsGuidanceNotWrapper guards the board-approved
// posture: the guide is prompt-side guidance, never a rigid stamped form
// (ISI-7195 rejection carries over to WS-7a) and never a post-hoc rewrite.
func TestDiscussionPostGuideIsGuidanceNotWrapper(t *testing.T) {
	for _, forbidden := range []string{
		"must use headings",
		"**What was asked**",
		"rewrite the body",
	} {
		if strings.Contains(DiscussionPostToolDescription, forbidden) {
			t.Errorf("guide reintroduced rigid-form element %q", forbidden)
		}
	}
	if !strings.Contains(DiscussionPostToolDescription, "soft target, not a limit") {
		t.Error("guide must frame the 1200-char target as soft, not a hard limit")
	}
}
