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

// mcpdiscussion.go — the ADR-0024c D1 built-in MCPServer provisioner for
// discussion reply capability (ISI-5138).
//
// A source='discussion' thread-run replies through the memory service's MCP
// edge (discussion_search + discussion_post). This file provisions the singleton
// `ksquad-memory-discussion` MCPServer per squad namespace, exactly mirroring
// the authoring provisioner (mcpauthoring.go) but narrowed to the discussion
// tool surface.

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
	// DiscussionMCPServerName is the deterministic, singleton name of the
	// built-in memory-discussion MCPServer provisioned in every squad
	// namespace (ADR-0024c D1, ISI-5138). Mirrors AuthoringMCPServerName.
	DiscussionMCPServerName = "ksquad-memory-discussion"
)

// discussionMCPServer renders the desired built-in discussion server for a
// squad namespace: streamable-http at the memory Service /mcp, tool envelope
// narrowed to exactly discussion_search + discussion_post, discovery=Manual.
func discussionMCPServer(ns string, teamObj *api.Team, controlPlaneNS string) *api.MCPServer {
	interval := int32(0)
	return &api.MCPServer{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      DiscussionMCPServerName,
			Labels:    managedLabels(teamObj),
		},
		Spec: api.MCPServerSpec{
			Transport: api.MCPTransportStreamableHTTP,
			Endpoint:  authoringEndpoint(controlPlaneNS), // same in-cluster memory FQDN
			ToolFilter: &api.MCPToolFilter{
				Allow: append([]string(nil), memory.DiscussionToolNames...),
			},
			Discovery: &api.MCPServerDiscovery{
				Mode:            api.MCPDiscoveryModeManual,
				IntervalMinutes: &interval,
			},
		},
	}
}

// ensureDiscussionMCPServer upserts the singleton built-in discussion server
// and seeds its status.observedTools. Mirrors ensureAuthoringMCPServer.
func (r *Reconciler) ensureDiscussionMCPServer(ctx context.Context, teamObj *api.Team, nsName string, nsUID types.UID) error {
	desired := discussionMCPServer(nsName, teamObj, r.controlPlaneNamespace())

	var existing api.MCPServer
	err := r.Get(ctx, types.NamespacedName{Namespace: nsName, Name: DiscussionMCPServerName}, &existing)
	switch {
	case apierrors.IsNotFound(err):
		setNamespaceOwner(desired, nsUID)
		if err := r.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create discussion MCPServer: %w", err)
		}
		if err := r.Get(ctx, types.NamespacedName{Namespace: nsName, Name: DiscussionMCPServerName}, &existing); err != nil {
			return fmt.Errorf("get discussion MCPServer after create: %w", err)
		}
	case err != nil:
		return fmt.Errorf("get discussion MCPServer: %w", err)
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
				return fmt.Errorf("update discussion MCPServer: %w", err)
			}
		}
	}

	return r.ensureDiscussionStatus(ctx, &existing)
}

// ensureDiscussionStatus seeds status.observedTools from the compiled-in
// discussion manifest. Mirrors ensureAuthoringStatus.
func (r *Reconciler) ensureDiscussionStatus(ctx context.Context, server *api.MCPServer) error {
	seed := memory.DiscussionToolNames
	if apiequality.Semantic.DeepEqual(server.Status.ObservedTools, seed) {
		return nil
	}
	patch := client.MergeFrom(server.DeepCopy())
	server.Status.ObservedTools = append([]string(nil), seed...)
	if err := r.Status().Patch(ctx, server, patch); err != nil {
		return fmt.Errorf("seed discussion MCPServer status.observedTools: %w", err)
	}
	return nil
}
