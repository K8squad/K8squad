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

// mcpsearch.go — the ISI-5276 (WS-B of ISI-5270) built-in MCPServer provisioner
// for cross-ticket work-item search.
//
// Every run — ticket OR discussion — benefits from being able to search other
// tickets before duplicating work or to cite related/blocking tickets. The FTS
// read model already exists (pkg/search over the 0012 index) and the memory MCP
// edge now wraps it as `work_item_search` (internal/memory/toolmcp.go). This file
// provisions the singleton `ksquad-memory-search` MCPServer per squad namespace,
// mirroring the authoring (mcpauthoring.go) and discussion (mcpdiscussion.go)
// provisioners but narrowed to the single read tool. UNLIKE those two, the
// capability gate injects this endpoint UNCONDITIONALLY for every run (pkg/
// capability), so the tool reaches agents in both run types (the §2 gap note).

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
	// SearchMCPServerName is the deterministic, singleton name of the built-in
	// memory-search MCPServer provisioned in every squad namespace (ISI-5276).
	// Mirrors AuthoringMCPServerName / DiscussionMCPServerName.
	SearchMCPServerName = "ksquad-memory-search"
)

// searchMCPServer renders the desired built-in search server for a squad
// namespace: streamable-http at the memory Service /mcp (the SAME in-cluster
// FQDN as the authoring/discussion servers), tool envelope narrowed to exactly
// work_item_search, discovery=Manual (the operator seeds observedTools from the
// compiled-in manifest; probing ourselves would be pointless).
func searchMCPServer(ns string, teamObj *api.Team, controlPlaneNS string) *api.MCPServer {
	interval := int32(0)
	return &api.MCPServer{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      SearchMCPServerName,
			Labels:    managedLabels(teamObj),
		},
		Spec: api.MCPServerSpec{
			Transport: api.MCPTransportStreamableHTTP,
			Endpoint:  authoringEndpoint(controlPlaneNS), // same in-cluster memory FQDN
			ToolFilter: &api.MCPToolFilter{
				Allow: append([]string(nil), memory.SearchToolNames...),
			},
			Discovery: &api.MCPServerDiscovery{
				Mode:            api.MCPDiscoveryModeManual,
				IntervalMinutes: &interval,
			},
		},
	}
}

// ensureSearchMCPServer upserts the singleton built-in search server and seeds
// its status.observedTools. Mirrors ensureDiscussionMCPServer.
func (r *Reconciler) ensureSearchMCPServer(ctx context.Context, teamObj *api.Team, nsName string, nsUID types.UID) error {
	desired := searchMCPServer(nsName, teamObj, r.controlPlaneNamespace())

	var existing api.MCPServer
	err := r.Get(ctx, types.NamespacedName{Namespace: nsName, Name: SearchMCPServerName}, &existing)
	switch {
	case apierrors.IsNotFound(err):
		setNamespaceOwner(desired, nsUID)
		if err := r.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create search MCPServer: %w", err)
		}
		if err := r.Get(ctx, types.NamespacedName{Namespace: nsName, Name: SearchMCPServerName}, &existing); err != nil {
			return fmt.Errorf("get search MCPServer after create: %w", err)
		}
	case err != nil:
		return fmt.Errorf("get search MCPServer: %w", err)
	default:
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
				return fmt.Errorf("update search MCPServer: %w", err)
			}
		}
	}

	return r.ensureSearchStatus(ctx, &existing)
}

// ensureSearchStatus seeds status.observedTools from the compiled-in search
// manifest. Mirrors ensureDiscussionStatus.
func (r *Reconciler) ensureSearchStatus(ctx context.Context, server *api.MCPServer) error {
	seed := memory.SearchToolNames
	if apiequality.Semantic.DeepEqual(server.Status.ObservedTools, seed) {
		return nil
	}
	patch := client.MergeFrom(server.DeepCopy())
	server.Status.ObservedTools = append([]string(nil), seed...)
	if err := r.Status().Patch(ctx, server, patch); err != nil {
		return fmt.Errorf("seed search MCPServer status.observedTools: %w", err)
	}
	return nil
}
