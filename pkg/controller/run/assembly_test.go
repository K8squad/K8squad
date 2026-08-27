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

package run

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/reconcile"
	"github.com/K8squad/K8squad/pkg/toolchain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const asmRunNS = "bmad-squad"

// seedAssemblyWorld builds the full capability chain: Run → Agent (Role
// with a default skill) → Skills requiring kubectl@1.31 + github-mcp;
// catalog Toolchain kubectl with read-only RBAC; MCPServer github-mcp
// (streamable-http, filtered, credentialed, egress-pinned) + its
// EgressPolicy + the durable step source at claiming.
func seedAssemblyWorld(t *testing.T) (client.Client, *api.Run, *fakeStepSource) {
	t.Helper()

	role := &api.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: asmRunNS},
		Spec:       api.RoleSpec{DefaultSkills: []api.ObjectRef{{Name: "restart-deploy"}}},
	}
	agent := &api.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: asmRunNS},
		Spec: api.AgentSpec{
			RoleRef:   api.ObjectRef{Name: "dev"},
			SkillRefs: []api.ObjectRef{{Name: "gh-ops"}},
		},
	}
	skillKubectl := &api.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "restart-deploy", Namespace: asmRunNS},
		Spec: api.SkillSpec{
			Requires:    api.SkillRequires{Toolchains: []string{"kubectl@1.31"}},
			McpToolRefs: []api.ObjectRef{{Name: "github-mcp"}},
		},
	}
	skillGit := &api.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "gh-ops", Namespace: asmRunNS},
		Spec: api.SkillSpec{
			Requires: api.SkillRequires{Toolchains: []string{"git@2.62"}},
		},
	}
	toolchainKubectl := &api.Toolchain{
		ObjectMeta: metav1.ObjectMeta{Name: "kubectl", Namespace: "ksquad-system"},
		Spec: api.ToolchainSpec{
			Versions: []api.ToolchainVersion{{
				Version:  "1.31",
				Image:    "ghcr.io/k8squad/toolchains/kubectl:1.31",
				Provides: []string{"kubectl"},
			}},
			RBAC: &api.ToolchainRBAC{
				Scope: api.ToolchainRBACScopeNamespace,
				Rules: []rbacv1.PolicyRule{{
					APIGroups: []string{""},
					Resources: []string{"pods"},
					Verbs:     []string{"get", "list", "watch"},
				}},
			},
		},
	}
	toolchainGit := &api.Toolchain{
		ObjectMeta: metav1.ObjectMeta{Name: "git", Namespace: "ksquad-system"},
		Spec: api.ToolchainSpec{
			Versions: []api.ToolchainVersion{{
				Version:  "2.62",
				Image:    "ghcr.io/k8squad/toolchains/git:2.62",
				Provides: []string{"git"},
			}},
		},
	}
	mcp := &api.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "github-mcp", Namespace: asmRunNS},
		Spec: api.MCPServerSpec{
			Transport:           api.MCPTransportStreamableHTTP,
			Endpoint:            "https://mcp.example/github",
			CredentialSecretRef: &api.SecretRef{Name: "github-token"},
			ToolFilter:          &api.MCPToolFilter{Allow: []string{"create_pull_request", "list_issues"}, Deny: []string{"list_*"}},
			EgressRef:           &api.ObjectRef{Name: "github-egress"},
		},
		Status: api.MCPServerStatus{ObservedTools: []string{"create_pull_request", "list_issues", "delete_repo"}},
	}
	egress := &api.EgressPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "github-egress", Namespace: asmRunNS},
	}
	run := &api.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "asm1", Namespace: asmRunNS, UID: "uid-asm1"},
		Spec: api.RunSpec{
			WorkItemRef: "wi-1",
			Agents:      []api.ObjectRef{{Name: "coder"}},
		},
	}

	s := runtime.NewScheme()
	require.NoError(t, api.AddToScheme(s))
	require.NoError(t, clientgoscheme.AddToScheme(s))
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(role, agent, skillKubectl, skillGit, toolchainKubectl, toolchainGit, mcp, egress, run).
		WithStatusSubresource(&api.Run{}).
		Build()
	steps := &fakeStepSource{step: reconcile.StepClaimingSandbox}
	return c, run, steps
}

type fakeStepSource struct {
	step reconcile.Step
}

func (f *fakeStepSource) StepForWorkItem(_ context.Context, _ string) (reconcile.Step, bool, error) {
	return f.step, true, nil
}

func TestAssemblerEnsureManifestComputesAndProjects(t *testing.T) {
	c, run, _ := seedAssemblyWorld(t)
	asm := NewAssembler(c, toolchain.PlatformConfig{})

	manifest, err := asm.EnsureManifest(context.Background(), run)
	require.NoError(t, err)

	// Toolchains: kubectl (from the Role default skill) + git (agent skill).
	names := []string{}
	for _, tc := range manifest.Toolchains {
		names = append(names, tc.Name)
	}
	assert.ElementsMatch(t, []string{"kubectl", "git"}, names)

	// MCP endpoint with the EFFECTIVE filter: allow ∩ observed − deny.
	require.Len(t, manifest.MCPEndpoints, 1)
	ep := manifest.MCPEndpoints[0]
	assert.Equal(t, "github-mcp", ep.Name)
	assert.Equal(t, []string{"create_pull_request"}, ep.AllowTools)
	require.NotNil(t, ep.CredentialSecretRef)
	assert.NotEmpty(t, manifest.CapabilityHash)

	// The IR ConfigMap exists, owner-ref'd to the Run.
	cm := &corev1.ConfigMap{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: asmRunNS, Name: "ksquad-run-asm1-mcp"}, cm))
	require.Len(t, cm.OwnerReferences, 1)
	assert.Contains(t, cm.Data["config.json"], "github-mcp")
}

func TestAssemblerManifestImmutableOnceSet(t *testing.T) {
	c, run, _ := seedAssemblyWorld(t)
	asm := NewAssembler(c, toolchain.PlatformConfig{})

	first, err := asm.EnsureManifest(context.Background(), run)
	require.NoError(t, err)

	// Mid-flight catalog change: delete the Toolchain entirely. The
	// recorded manifest stays the audit truth (ADR-044 invariant) — a
	// running sandbox never widens or rewrites its envelope.
	require.NoError(t, c.Delete(context.Background(), &api.Toolchain{
		ObjectMeta: metav1.ObjectMeta{Name: "kubectl", Namespace: "ksquad-system"}}))

	run.Status.CapabilityManifest = first
	again, err := asm.EnsureManifest(context.Background(), run)
	require.NoError(t, err)
	assert.Equal(t, first.CapabilityHash, again.CapabilityHash)
}

func TestAssemblerFailClosedOnUnresolvableEnvelope(t *testing.T) {
	c, run, _ := seedAssemblyWorld(t)
	// Break the egress pin: policy deleted.
	require.NoError(t, c.Delete(context.Background(), &api.EgressPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "github-egress", Namespace: asmRunNS}}))

	_, err := NewAssembler(c, toolchain.PlatformConfig{}).EnsureManifest(context.Background(), run)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "egressRef")
}

func TestReconcilerStampsManifestAndKeepsItAtTerminal(t *testing.T) {
	c, run, steps := seedAssemblyWorld(t)
	r := &Reconciler{
		Client:    c,
		Source:    steps,
		RBAC:      NewRBACRenderer(c, toolchain.PlatformConfig{}),
		Assembler: NewAssembler(c, toolchain.PlatformConfig{}),
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: run.Namespace, Name: run.Name}})
	require.NoError(t, err)

	var live api.Run
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: run.Namespace, Name: run.Name}, &live))
	require.NotNil(t, live.Status.CapabilityManifest)
	assert.NotEmpty(t, live.Status.CapabilityManifest.CapabilityHash)
	require.NotNil(t, live.Status.GrantedToolchainRBAC)
	require.NotNil(t, live.Status.GrantedToolchainRBAC.RoleRef)
	stamped := live.Status.CapabilityManifest

	// Go terminal: the RBAC grant is released, the manifest SURVIVES as
	// the reproducibility record, the IR ConfigMap is swept.
	steps.step = reconcile.StepSucceeded
	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: run.Namespace, Name: run.Name}})
	require.NoError(t, err)

	var done api.Run
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: run.Namespace, Name: run.Name}, &done))
	assert.Nil(t, done.Status.GrantedToolchainRBAC)
	assert.NotNil(t, done.Status.CapabilityManifest)
	assert.Equal(t, stamped.CapabilityHash, done.Status.CapabilityManifest.CapabilityHash)

	err = c.Get(context.Background(), types.NamespacedName{Namespace: asmRunNS, Name: "ksquad-run-asm1-mcp"}, &corev1.ConfigMap{})
	require.Error(t, err) // swept at terminal
}

func TestReconcilerAssemblyErrorRequeuesFailClosed(t *testing.T) {
	c, run, steps := seedAssemblyWorld(t)
	require.NoError(t, c.Delete(context.Background(), &api.EgressPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "github-egress", Namespace: asmRunNS}}))

	r := &Reconciler{
		Client:    c,
		Source:    steps,
		Assembler: NewAssembler(c, toolchain.PlatformConfig{}),
	}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: run.Namespace, Name: run.Name}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "assemble capabilities")
}
