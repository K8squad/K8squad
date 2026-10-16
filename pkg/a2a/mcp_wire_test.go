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

package a2a

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/K8squad/K8squad/pkg/capability"
)

// TestTaskMCPEnvelopeWireRoundTrip pins the ISI-5017 wire seam: the operator
// marshals wire.Task (this package) as the POST /task body and the pod-side
// supervisor decodes the same type — the MCP IR + resolved credential values
// MUST survive that round trip, because a warm-pool pod can only receive the
// capability envelope on the submit payload. A dropped json tag here is the
// exact "opencode.json has no mcp block" failure ISI-5018 chased live.
func TestTaskMCPEnvelopeWireRoundTrip(t *testing.T) {
	in := Task{
		A2ATaskID:  "run-wire",
		WorkItemID: "wi-1",
		FenceToken: "7",
		MCPEndpoints: []capability.Endpoint{{
			Name:       "ksquad-memory-authoring",
			Transport:  "streamable-http",
			URL:        "http://ksquad-memory.k8squad-system.svc.cluster.local:8080/mcp",
			AllowTools: []string{"work_item_create", "work_item_update", "work_item_assign"},
			EnvNames:   []string{"KSQUAD_MCP_KSQUAD_MEMORY_AUTHORING_TOKEN"},
		}},
		MCPTokenEnv: map[string]string{
			"KSQUAD_MCP_KSQUAD_MEMORY_AUTHORING_TOKEN": "tok-wire",
		},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The wire keys are part of the contract: the supervisor decodes the
	// operator's body by tag name, so the tags must stay stable.
	for _, key := range []string{`"mcp_endpoints"`, `"mcp_token_env"`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("wire body lacks %s: %s", key, raw)
		}
	}

	var out Task
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.MCPEndpoints) != 1 || out.MCPEndpoints[0].Name != "ksquad-memory-authoring" {
		t.Fatalf("decoded MCPEndpoints = %+v, want the authoring endpoint", out.MCPEndpoints)
	}
	if len(out.MCPEndpoints[0].EnvNames) != 1 ||
		out.MCPEndpoints[0].EnvNames[0] != "KSQUAD_MCP_KSQUAD_MEMORY_AUTHORING_TOKEN" {
		t.Fatalf("decoded EnvNames = %v", out.MCPEndpoints[0].EnvNames)
	}
	if got := out.MCPTokenEnv["KSQUAD_MCP_KSQUAD_MEMORY_AUTHORING_TOKEN"]; got != "tok-wire" {
		t.Fatalf("decoded MCPTokenEnv value = %q, want tok-wire", got)
	}
}
