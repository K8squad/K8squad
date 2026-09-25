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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/memory"
)

// authoringWantTools is the tool surface the built-in server must expose — the
// three ADR-0024a authoring verbs, in the compiled-in manifest order.
var authoringWantTools = []string{"work_item_create", "work_item_update", "work_item_assign"}

// TestAuthoringMCPServerProvisioned (ISI-4867 AC1-AC3): reconciling a Team
// provisions exactly one ksquad-memory-authoring MCPServer in its squad
// namespace, streamable-http at the memory Service /mcp, tool envelope narrowed
// to the three authoring verbs, discovery pinned to Manual, and
// status.observedTools seeded from the compiled-in manifest (no probe).
func TestAuthoringMCPServerProvisioned(t *testing.T) {
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

	// Exactly one server, deterministically named.
	var list api.MCPServerList
	if err := c.List(context.Background(), &list); err != nil {
		t.Fatalf("list mcpservers: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("want exactly 1 MCPServer, got %d: %+v", len(list.Items), list.Items)
	}
	srv := &list.Items[0]
	if srv.Name != AuthoringMCPServerName || srv.Namespace != ns {
		t.Fatalf("server at %s/%s, want %s/%s", srv.Namespace, srv.Name, ns, AuthoringMCPServerName)
	}

	if srv.Spec.Transport != api.MCPTransportStreamableHTTP {
		t.Errorf("transport = %q, want streamable-http", srv.Spec.Transport)
	}
	wantEndpoint := "http://ksquad-memory." + SystemNamespace + ".svc.cluster.local:8080/mcp"
	if srv.Spec.Endpoint != wantEndpoint {
		t.Errorf("endpoint = %q, want %q", srv.Spec.Endpoint, wantEndpoint)
	}
	if srv.Spec.ToolFilter == nil || !reflect.DeepEqual(srv.Spec.ToolFilter.Allow, authoringWantTools) {
		t.Errorf("toolFilter.allow = %+v, want %v", srv.Spec.ToolFilter, authoringWantTools)
	}
	if srv.Spec.Discovery == nil || srv.Spec.Discovery.Mode != api.MCPDiscoveryModeManual {
		t.Errorf("discovery = %+v, want mode=Manual (no self-probe)", srv.Spec.Discovery)
	}
	// Static per-project CR carries no credential (S3 owns the per-run token).
	if srv.Spec.CredentialSecretRef != nil {
		t.Errorf("credentialSecretRef = %+v, want none (S3 wires the per-run token)", srv.Spec.CredentialSecretRef)
	}

	// AC2: observedTools seeded from the compiled-in manifest via the status
	// subresource. endpointFor fails closed on empty observedTools, so this is
	// the write that makes the server admissible downstream.
	if !reflect.DeepEqual(srv.Status.ObservedTools, authoringWantTools) {
		t.Errorf("status.observedTools = %v, want %v (seeded, not probed)", srv.Status.ObservedTools, authoringWantTools)
	}
}

// TestAuthoringSeedMatchesManifest is the anti-drift guard: the seed the
// operator writes is literally memory.AuthoringToolNames — the same constants
// agentauthor.go advertises — so the two lists can never diverge.
func TestAuthoringSeedMatchesManifest(t *testing.T) {
	if !reflect.DeepEqual(memory.AuthoringToolNames, authoringWantTools) {
		t.Fatalf("memory.AuthoringToolNames = %v, want %v — the compiled-in manifest drifted from the seed the provisioner writes", memory.AuthoringToolNames, authoringWantTools)
	}
	// And the provisioned spec/seed reference exactly those constants.
	team := newTeam("alpha", "uid-alpha")
	srv := authoringMCPServer("squad-ns", team, SystemNamespace)
	if !reflect.DeepEqual(srv.Spec.ToolFilter.Allow, memory.AuthoringToolNames) {
		t.Errorf("toolFilter.allow = %v, want the manifest %v", srv.Spec.ToolFilter.Allow, memory.AuthoringToolNames)
	}
}

// TestAuthoringMCPServerIdempotentAndDriftCorrected (ISI-4867 AC4): a second
// reconcile creates no duplicate; a mutated spec and a cleared observedTools
// both self-heal on the next reconcile.
func TestAuthoringMCPServerIdempotentAndDriftCorrected(t *testing.T) {
	team := newTeam("alpha", "uid-alpha")
	r, c := newReconciler(t, team)

	if err := reconcileTeam(t, r, "alpha"); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	var after api.Team
	_ = c.Get(context.Background(), types.NamespacedName{Name: "alpha", Namespace: "default"}, &after)
	ns := after.Status.Namespace

	// Drift the spec (widen the envelope) and wipe the seeded status.
	var drifted api.MCPServer
	key := types.NamespacedName{Namespace: ns, Name: AuthoringMCPServerName}
	if err := c.Get(context.Background(), key, &drifted); err != nil {
		t.Fatalf("get server: %v", err)
	}
	drifted.Spec.ToolFilter = &api.MCPToolFilter{Allow: []string{"*"}}
	if err := c.Update(context.Background(), &drifted); err != nil {
		t.Fatalf("mutate spec: %v", err)
	}
	drifted.Status.ObservedTools = nil
	if err := c.Status().Update(context.Background(), &drifted); err != nil {
		t.Fatalf("wipe status: %v", err)
	}

	if err := reconcileTeam(t, r, "alpha"); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	// Still exactly one — no duplicate created.
	var list api.MCPServerList
	if err := c.List(context.Background(), &list); err != nil {
		t.Fatalf("list mcpservers: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("want exactly 1 MCPServer after re-reconcile, got %d", len(list.Items))
	}
	healed := &list.Items[0]
	if healed.Spec.ToolFilter == nil || !reflect.DeepEqual(healed.Spec.ToolFilter.Allow, authoringWantTools) {
		t.Errorf("spec drift not corrected: toolFilter.allow = %+v, want %v", healed.Spec.ToolFilter, authoringWantTools)
	}
	if !reflect.DeepEqual(healed.Status.ObservedTools, authoringWantTools) {
		t.Errorf("status seed not restored: observedTools = %v, want %v", healed.Status.ObservedTools, authoringWantTools)
	}
}

// TestAuthoringMCPServerNamespaceOwned (ISI-4867: GC'd with the namespace):
// when the squad namespace carries a real UID, the built-in server is stamped
// with an owner reference to it, so it is reaped with the namespace on Team
// teardown (a namespaced Team cannot own it directly).
func TestAuthoringMCPServerNamespaceOwned(t *testing.T) {
	team := newTeam("alpha", "uid-alpha")
	nsName := NamespaceNameFor(team)
	managed := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   nsName,
		UID:    types.UID("ns-uid-alpha"),
		Labels: namespaceLabels(team),
	}}
	r, c := newReconciler(t, team, managed)

	if err := reconcileTeam(t, r, "alpha"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var srv api.MCPServer
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: nsName, Name: AuthoringMCPServerName}, &srv); err != nil {
		t.Fatalf("get server: %v", err)
	}
	owned := false
	for _, ref := range srv.OwnerReferences {
		if ref.Kind == "Namespace" && ref.UID == "ns-uid-alpha" {
			owned = true
		}
	}
	if !owned {
		t.Errorf("MCPServer not owned by the squad namespace: %+v", srv.OwnerReferences)
	}
}
