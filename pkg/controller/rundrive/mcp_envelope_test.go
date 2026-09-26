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

package rundrive

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/capability"
)

// mcpEnvelopeRun is the shared fixture for the ISI-5017 dispatch tests: a Run
// whose immutable capability manifest demands the built-in memory-authoring
// MCP server, with its per-run credential Secret bound (mirrors Run assembly's
// S2/S3 output).
func mcpEnvelopeRun(uid, secretName string) *api.Run {
	run := modelRoleRun(uid, "john")
	run.Status.CapabilityManifest = &api.CapabilityManifest{
		MCPEndpoints: []api.ResolvedMCPEndpoint{{
			Name:                capabilityName,
			Transport:           api.MCPTransportStreamableHTTP,
			URL:                 "http://ksquad-memory.k8squad-system.svc.cluster.local:8080/mcp",
			AllowTools:          []string{"work_item_create", "work_item_update", "work_item_assign"},
			CredentialSecretRef: &api.SecretRef{Name: secretName},
		}},
	}
	return run
}

// capabilityName mirrors the built-in authoring MCPServer name (constant kept
// local so the test asserts the exact wire value the live path uses).
const capabilityName = "ksquad-memory-authoring"

func mcpEnvelopeAgent() *api.Agent {
	return &api.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "john", Namespace: "team-a"},
		Spec:       api.AgentSpec{Model: "claude-sonnet-4"},
	}
}

// TestBuildTaskDeliversMCPEnvelopeAndToken is the ISI-5017 acceptance at the
// dispatch seam: a warm-pool-bound Run receives its MCP IR AND the resolved
// per-run credential VALUE on the submit payload, so the in-pod shim can render
// the authoring tools without the (post-Bind impossible) ConfigMap volume.
func TestBuildTaskDeliversMCPEnvelopeAndToken(t *testing.T) {
	const runUID = "11110000-1111-4444-8888-aaaaaaaaaaaa"
	run := mcpEnvelopeRun(runUID, "run-mpr-authoring-token")
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "run-mpr-authoring-token", Namespace: "team-a"},
		Data:       map[string][]byte{"token": []byte("tok-abc123")},
	}
	cl := fake.NewClientBuilder().WithScheme(dispatchScheme(t)).
		WithObjects(run, mcpEnvelopeAgent(), sec, dispatchTeamObj()).
		WithStatusSubresource(&api.Run{}).Build()
	d := &operatorDispatch{
		cfg:    OperatorDispatchConfig{Client: cl},
		source: fakeDispatchSource{title: "t", body: "b", fence: "1"},
	}

	tk, err := d.buildTask(context.Background(), runUID, runUID)
	if err != nil {
		t.Fatalf("buildTask: %v", err)
	}
	if len(tk.MCPEndpoints) != 1 {
		t.Fatalf("task.MCPEndpoints len = %d, want 1", len(tk.MCPEndpoints))
	}
	ep := tk.MCPEndpoints[0]
	if ep.Name != capabilityName || ep.Transport != "streamable-http" {
		t.Errorf("endpoint = {name:%q transport:%q}", ep.Name, ep.Transport)
	}
	wantEnv := capability.CredentialEnvName(capabilityName)
	if len(ep.EnvNames) != 1 || ep.EnvNames[0] != wantEnv {
		t.Errorf("endpoint EnvNames = %v, want [%s]", ep.EnvNames, wantEnv)
	}
	if got := tk.MCPTokenEnv[wantEnv]; got != "tok-abc123" {
		t.Errorf("task.MCPTokenEnv[%s] = %q, want the per-run Secret value", wantEnv, got)
	}
}

// TestBuildTaskFailsClosedWhenMCPCredentialSecretMissing is the ADR-044
// fail-closed acceptance: a manifest that demands an MCP server whose
// credential Secret is absent must ABORT the dispatch, never launch a generic
// pod with a half-wired capability envelope.
func TestBuildTaskFailsClosedWhenMCPCredentialSecretMissing(t *testing.T) {
	const runUID = "22220000-2222-4444-8888-bbbbbbbbbbbb"
	run := mcpEnvelopeRun(runUID, "absent-authoring-token")
	cl := fake.NewClientBuilder().WithScheme(dispatchScheme(t)).
		WithObjects(run, mcpEnvelopeAgent(), dispatchTeamObj()).
		WithStatusSubresource(&api.Run{}).Build()
	d := &operatorDispatch{
		cfg:    OperatorDispatchConfig{Client: cl},
		source: fakeDispatchSource{title: "t", body: "b", fence: "1"},
	}

	if _, err := d.buildTask(context.Background(), runUID, runUID); err == nil {
		t.Fatal("buildTask must fail closed when the MCP credential Secret is missing")
	}
}

// TestBuildTaskBareManifestHasNoMCPEnvelope pins the no-regression case: a Run
// with no MCP demand carries no endpoints/tokens on the envelope.
func TestBuildTaskBareManifestHasNoMCPEnvelope(t *testing.T) {
	const runUID = "33330000-3333-4444-8888-cccccccccccc"
	run := modelRoleRun(runUID, "john")
	cl := fake.NewClientBuilder().WithScheme(dispatchScheme(t)).
		WithObjects(run, mcpEnvelopeAgent(), dispatchTeamObj()).
		WithStatusSubresource(&api.Run{}).Build()
	d := &operatorDispatch{
		cfg:    OperatorDispatchConfig{Client: cl},
		source: fakeDispatchSource{title: "t", body: "b", fence: "1"},
	}

	tk, err := d.buildTask(context.Background(), runUID, runUID)
	if err != nil {
		t.Fatalf("buildTask: %v", err)
	}
	if len(tk.MCPEndpoints) != 0 || len(tk.MCPTokenEnv) != 0 {
		t.Errorf("bare manifest produced MCP envelope: endpoints=%v tokens=%v", tk.MCPEndpoints, tk.MCPTokenEnv)
	}
}
