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
	"reflect"
	"strings"
	"testing"

	a2awire "github.com/K8sAgentMesh/K8sAgentMesh/a2a"
	apiv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/capability"
)

// TestMCPEndpointSeamRoundTrip pins the P3.2 host seam (ISI-6035, leak L2):
// the operator resolves the MCP IR host-side (capability.Endpoint, whose
// credential/egress refs are CRD-backed), converts it into the module's wire
// IR at dispatch, marshals wire.Task as the POST /task body, and the pod-side
// supervisor decodes the same type — then the shim converts back to feed the
// host renderers. The IR + resolved credential env names MUST survive the
// full loop unchanged, because a warm-pool pod can only receive the capability
// envelope on the submit payload (ISI-5017). A dropped field in either
// conversion, or a dropped json tag, is the exact "opencode.json has no mcp
// block" failure ISI-5018 chased live.
func TestMCPEndpointSeamRoundTrip(t *testing.T) {
	in := capability.Endpoint{
		Name:       "ksquad-memory-authoring",
		Transport:  "streamable-http",
		URL:        "http://ksquad-memory.k8squad-system.svc.cluster.local:8080/mcp",
		AllowTools: []string{"work_item_create", "work_item_update", "work_item_assign"},
		EnvNames:   []string{"KSQUAD_MCP_KSQUAD_MEMORY_AUTHORING_TOKEN"},
		CredentialSecretRef: &apiv1alpha1.SecretRef{
			Name: "run-mcp-authoring-token",
		},
	}
	task := Task{
		A2ATaskID:    "run-wire",
		WorkItemID:   "wi-1",
		FenceToken:   "7",
		MCPEndpoints: MCPEndpointsFromCapability([]capability.Endpoint{in}),
		MCPTokenEnv: map[string]string{
			"KSQUAD_MCP_KSQUAD_MEMORY_AUTHORING_TOKEN": "tok-wire",
		},
	}
	raw, err := json.Marshal(task)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The wire keys are part of the contract: the supervisor decodes the
	// operator's body by tag name, so the tags must stay stable.
	for _, key := range []string{`"mcp_endpoints"`, `"mcp_token_env"`, `"credentialSecretRef"`} {
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
	if got := out.MCPTokenEnv["KSQUAD_MCP_KSQUAD_MEMORY_AUTHORING_TOKEN"]; got != "tok-wire" {
		t.Fatalf("decoded MCPTokenEnv value = %q, want tok-wire", got)
	}

	back := MCPEndpointsToCapability(out.MCPEndpoints)
	if !reflect.DeepEqual(back, []capability.Endpoint{in}) {
		t.Fatalf("seam round trip changed the IR:\n got %+v\nwant %+v", back, in)
	}
}

// TestAliasWireIdentity pins the re-export discipline: every k8squad a2a type
// alias must be the module wire type itself (reflect identity, not a copy),
// so a value crossing the seam never needs a second conversion and the wire
// bytes cannot drift between the two packages.
func TestAliasWireIdentity(t *testing.T) {
	for _, tc := range []struct {
		name       string
		host, wire reflect.Type
	}{
		{"Task", reflect.TypeOf(Task{}), reflect.TypeOf(a2awire.Task{})},
		{"AgentCard", reflect.TypeOf(AgentCard{}), reflect.TypeOf(a2awire.AgentCard{})},
		{"Event", reflect.TypeOf(Event{}), reflect.TypeOf(a2awire.Event{})},
		{"Status", reflect.TypeOf(Status{}), reflect.TypeOf(a2awire.Status{})},
		{"MCPEndpoint", reflect.TypeOf(MCPEndpoint{}), reflect.TypeOf(a2awire.MCPEndpoint{})},
		{"Shim", reflect.TypeOf((*Shim)(nil)).Elem(), reflect.TypeOf((*a2awire.Shim)(nil)).Elem()},
	} {
		if tc.host != tc.wire {
			t.Errorf("%s alias drifted from the module wire type: %v != %v", tc.name, tc.host, tc.wire)
		}
	}
	if SchemaVersion != "ksquad.a2a/v1" {
		t.Errorf("host schema branding = %q, want ksquad.a2a/v1", SchemaVersion)
	}
	if DefaultSchemaVersion != a2awire.DefaultSchemaVersion {
		t.Errorf("DefaultSchemaVersion re-export drifted from the module")
	}
}
