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
	"context"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/memory"
)

// searchWantTools is the single-tool surface the built-in memory-search server
// must expose — the cross-ticket work-item search verb (ISI-5276).
var searchWantTools = []string{"work_item_search"}

// TestSearchMCPServerProvisioned (ISI-5276): reconciling a Team provisions exactly
// one ksquad-memory-search MCPServer in its squad namespace, streamable-http at the
// memory Service /mcp, tool envelope narrowed to work_item_search, discovery pinned
// to Manual, and status.observedTools seeded from the compiled-in manifest.
func TestSearchMCPServerProvisioned(t *testing.T) {
	team := newTeam("alpha", "uid-alpha")
	r, c := newReconciler(t, team)

	if err := reconcileTeam(t, r, "alpha"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var after api.Team
	if err := c.Get(context.Background(), types.NamespacedName{Name: "alpha", Namespace: "default"}, &after); err != nil {
		t.Fatalf("get team: %v", err)
	}
	ns := after.Status.Namespace
	if ns == "" {
		t.Fatal("team never resolved a squad namespace")
	}

	var srv api.MCPServer
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: SearchMCPServerName}, &srv); err != nil {
		t.Fatalf("get search MCPServer: %v", err)
	}

	if srv.Spec.Transport != api.MCPTransportStreamableHTTP {
		t.Errorf("transport = %q, want streamable-http", srv.Spec.Transport)
	}
	wantEndpoint := "http://ksquad-memory." + SystemNamespace + ".svc.cluster.local:8080/mcp"
	if srv.Spec.Endpoint != wantEndpoint {
		t.Errorf("endpoint = %q, want %q", srv.Spec.Endpoint, wantEndpoint)
	}
	if srv.Spec.ToolFilter == nil || !reflect.DeepEqual(srv.Spec.ToolFilter.Allow, searchWantTools) {
		t.Errorf("toolFilter.allow = %+v, want %v", srv.Spec.ToolFilter, searchWantTools)
	}
	if srv.Spec.Discovery == nil || srv.Spec.Discovery.Mode != api.MCPDiscoveryModeManual {
		t.Errorf("discovery = %+v, want mode=Manual (no self-probe)", srv.Spec.Discovery)
	}
	if srv.Spec.CredentialSecretRef != nil {
		t.Errorf("credentialSecretRef = %+v, want none", srv.Spec.CredentialSecretRef)
	}
	if !reflect.DeepEqual(srv.Status.ObservedTools, searchWantTools) {
		t.Errorf("status.observedTools = %v, want %v (seeded, not probed)", srv.Status.ObservedTools, searchWantTools)
	}
}

// TestSearchSeedMatchesManifest is the anti-drift guard: the seed the operator
// writes is literally memory.SearchToolNames — the same constant toolmcp.go
// advertises — so the two lists can never diverge.
func TestSearchSeedMatchesManifest(t *testing.T) {
	if !reflect.DeepEqual(memory.SearchToolNames, searchWantTools) {
		t.Fatalf("memory.SearchToolNames = %v, want %v — the compiled-in manifest drifted from the seed the provisioner writes", memory.SearchToolNames, searchWantTools)
	}
	team := newTeam("alpha", "uid-alpha")
	srv := searchMCPServer("squad-ns", team, SystemNamespace)
	if !reflect.DeepEqual(srv.Spec.ToolFilter.Allow, memory.SearchToolNames) {
		t.Errorf("toolFilter.allow = %v, want the manifest %v", srv.Spec.ToolFilter.Allow, memory.SearchToolNames)
	}
}
