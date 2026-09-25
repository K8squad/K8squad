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

// mcpauthoring.go — the ADR-0024a S1 built-in MCPServer provisioner (ISI-4867).
//
// A dispatched agent authors sub-tickets through the memory service's MCP edge
// (internal/memory/agentauthor.go: work_item_create / work_item_update /
// work_item_assign). For a Run to be WIRED to that edge, an MCPServer CR naming
// it must exist in the namespace the Run executes in — the squad namespace this
// reconciler already owns. So the built-in server is provisioned here, as one
// more piece of the per-namespace scaffold: exactly one singleton
// `ksquad-memory-authoring` MCPServer per squad namespace, self-healing and
// GC'd with the namespace like the rest of the scaffold.
//
// It is deliberately NOT discovered by a live probe: the endpoint is our own
// in-cluster memory Service, and its authoring tool surface is COMPILED IN
// (memory.AuthoringToolNames). Probing ourselves would be pointless and could
// return the full memory tool set (memory_write, discussion_post, …) rather
// than the three authoring verbs. So the CR is marked
// discovery.mode=Manual (the mcpserver controller then skips it) and the
// operator SEEDS status.observedTools from the compiled-in manifest. The BYO
// probe path (pkg/controller/mcpserver/probe.go) is untouched.

import (
	"context"
	"fmt"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/memory"
)

const (
	// AuthoringMCPServerName is the deterministic, singleton name of the
	// built-in memory-authoring MCPServer provisioned in every squad
	// namespace. Deterministic ⇒ idempotent by construction: a reconcile can
	// never create a duplicate, and drift is corrected in place.
	AuthoringMCPServerName = "ksquad-memory-authoring"

	// memoryServiceName is the in-cluster memory Service the built-in server
	// points at (config/helm/templates/control-plane/memory.yaml).
	memoryServiceName = "ksquad-memory"

	// memoryServicePort is the memory Service's http port (memory.yaml).
	memoryServicePort = 8080
)

// authoringEndpoint is the streamable-http MCP URL of the in-cluster memory
// Service in the control-plane namespace. The Service is a control-plane
// singleton (not per-namespace), so the endpoint targets the control-plane
// namespace the reconciler was told the platform runs in.
//
// The host MUST be the full cluster-local FQDN (<svc>.<ns>.svc.cluster.local):
// sandbox pods live in squad namespaces whose DNS search path only expands
// their own namespace, so the shorter <svc>.<ns>.svc form does not resolve
// from a squad sandbox (ISI-4873: NXDOMAIN from ksquad-team-* pods; the
// authoring MCP never connected and runs terminal-failed).
func authoringEndpoint(controlPlaneNS string) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d%s", memoryServiceName, controlPlaneNS, memoryServicePort, memory.MCPEndpoint)
}

// authoringMCPServer renders the desired built-in server for a squad namespace:
// streamable-http at the memory Service /mcp, tool envelope narrowed to exactly
// the three first-party authoring verbs, and discovery pinned to Manual so the
// mcpserver controller never probes our own service.
func authoringMCPServer(ns string, teamObj *api.Team, controlPlaneNS string) *api.MCPServer {
	interval := int32(0) // Manual already disables probing; 0 makes the intent explicit.
	return &api.MCPServer{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      AuthoringMCPServerName,
			Labels:    managedLabels(teamObj),
		},
		Spec: api.MCPServerSpec{
			Transport: api.MCPTransportStreamableHTTP,
			Endpoint:  authoringEndpoint(controlPlaneNS),
			ToolFilter: &api.MCPToolFilter{
				Allow: append([]string(nil), memory.AuthoringToolNames...),
			},
			Discovery: &api.MCPServerDiscovery{
				Mode:            api.MCPDiscoveryModeManual,
				IntervalMinutes: &interval,
			},
		},
	}
}

// ensureAuthoringMCPServer upserts the singleton built-in server and seeds its
// status.observedTools. It is idempotent (deterministic name; spec/label drift
// corrected in place; no duplicate ever created) and owned by the squad
// namespace so it is GC'd with the namespace on Team teardown.
func (r *Reconciler) ensureAuthoringMCPServer(ctx context.Context, teamObj *api.Team, nsName string, nsUID types.UID) error {
	desired := authoringMCPServer(nsName, teamObj, r.controlPlaneNamespace())

	var existing api.MCPServer
	err := r.Get(ctx, types.NamespacedName{Namespace: nsName, Name: AuthoringMCPServerName}, &existing)
	switch {
	case apierrors.IsNotFound(err):
		setNamespaceOwner(desired, nsUID)
		if err := r.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create authoring MCPServer: %w", err)
		}
		// Re-read so the status seed below patches the persisted object (spec
		// create never populates status).
		if err := r.Get(ctx, types.NamespacedName{Namespace: nsName, Name: AuthoringMCPServerName}, &existing); err != nil {
			return fmt.Errorf("get authoring MCPServer after create: %w", err)
		}
	case err != nil:
		return fmt.Errorf("get authoring MCPServer: %w", err)
	default:
		// Correct drift in place — a mutated spec or stripped label self-heals.
		changed := false
		if nsUID != "" && !hasNamespaceOwner(&existing, nsUID) {
			setNamespaceOwner(&existing, nsUID)
			changed = true
		}
		if mergeLabels(&existing, desired) {
			changed = true
		}
		if !apiequality.Semantic.DeepEqual(existing.Spec, desired.Spec) {
			existing.Spec = desired.Spec
			changed = true
		}
		if changed {
			if err := r.Update(ctx, &existing); err != nil {
				return fmt.Errorf("update authoring MCPServer: %w", err)
			}
		}
	}

	return r.ensureAuthoringStatus(ctx, &existing)
}

// ensureAuthoringStatus seeds status.observedTools from the compiled-in
// first-party manifest via the status subresource. This is what makes the
// server admissible: capability.endpointFor fails closed on an empty
// observedTools, so a missing seed silently breaks the authoring lane
// downstream. Conditions (ToolsDiscovered/Ready) stay owned by the mcpserver
// controller, which reflects this seed under discovery.mode=Manual — the two
// writers touch disjoint status fields, so their MergeFrom patches never fight.
func (r *Reconciler) ensureAuthoringStatus(ctx context.Context, server *api.MCPServer) error {
	seed := memory.AuthoringToolNames
	if apiequality.Semantic.DeepEqual(server.Status.ObservedTools, seed) {
		return nil
	}
	patch := client.MergeFrom(server.DeepCopy())
	server.Status.ObservedTools = append([]string(nil), seed...)
	if err := r.Status().Patch(ctx, server, patch); err != nil {
		return fmt.Errorf("seed authoring MCPServer status.observedTools: %w", err)
	}
	return nil
}
