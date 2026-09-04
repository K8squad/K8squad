//go:build e2e

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

package e2e

import (
	"context"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// itoa renders an int port as the string the CRD port fields expect.
func itoa(n int) string { return strconv.Itoa(n) }

// scenario is the full CR graph the smoke drives:
//
//	Team ─ Project (repo) ─ EgressPolicy (proxy + allowlist)
//	  └─ Agent ─ Role
//	       └─ Skill (inline) ── Requires: kubectl@<pinned>  (catalog-resolved)
//	                        └── McpToolRefs: github-mcp
//	                              └─ MCPServer (streamable-http, github-mcp)
//	                                    ├─ CredentialSecretRef: github-mcp-token
//	                                    └─ EgressRef: the team EgressPolicy
//	  └─ Run (teamRef, projectRef, workItemRef, agents: [smoke-agent])
//
// Objects are created most-dependent-last and torn down in reverse so a Run
// never dangles a reference. All names are constants so assertions and skips
// cite the exact object.
type scenario struct {
	namespace      string
	kubectlVersion string // the pinned kubectl version this Run requires.

	egress    *ksquadv1alpha1.EgressPolicy
	mcpSecret *corev1.Secret
	mcpServer *ksquadv1alpha1.MCPServer
	skill     *ksquadv1alpha1.Skill
	role      *ksquadv1alpha1.Role
	agent     *ksquadv1alpha1.Agent
	project   *ksquadv1alpha1.Project
	team      *ksquadv1alpha1.Team
	run       *ksquadv1alpha1.Run
}

const (
	teamName    = "e2e-smoke-team"
	projectName = "e2e-smoke-project"
	roleName    = "e2e-smoke-role"
	agentName   = "e2e-smoke-agent"
	skillName   = "e2e-smoke-skill"
	egressName  = "e2e-smoke-egress"
	runName     = "e2e-smoke-run"
)

// newScenario builds the CR graph (unpersisted) targeting namespace ns, pinning
// the kubectl toolchain to kubectlVersion (resolved from the installed catalog).
func newScenario(ns, kubectlVersion string) *scenario {
	meta := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: ns}
	}

	egress := &ksquadv1alpha1.EgressPolicy{
		ObjectMeta: meta(egressName),
		Spec: ksquadv1alpha1.EgressPolicySpec{
			// Route squad egress through the per-team proxy Service (§9.2, AD-7);
			// the allowlisted upstream is reachable only via this hop, mirroring
			// s4-1's "allowlisted model path reachable via proxy".
			Proxy: &ksquadv1alpha1.EgressProxy{
				Address:  "egress-proxy." + ns + ".svc.cluster.local",
				Port:     itoa(egressProxyPort),
				Protocol: "TCP",
			},
			Allow: []ksquadv1alpha1.EgressRule{{
				To:    ksquadv1alpha1.EgressDestination{CIDR: "10.0.0.0/8"},
				Ports: []ksquadv1alpha1.EgressPort{{Protocol: "TCP", Port: itoa(egressProxyPort)}},
			}},
		},
	}

	// BYO credential Secret for github-mcp (ADR-045: material never in the CRD;
	// the token here is a throwaway the lane provisions — assertions prove the
	// mount/injection wiring, never the token value).
	mcpSecret := &corev1.Secret{
		ObjectMeta: meta(mcpCredentialSecret),
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{"token": "e2e-smoke-placeholder-token"},
	}

	mcpServer := &ksquadv1alpha1.MCPServer{
		ObjectMeta: meta(mcpServerName),
		Spec: ksquadv1alpha1.MCPServerSpec{
			Transport: ksquadv1alpha1.MCPTransportStreamableHTTP,
			Endpoint:  "https://api.githubcopilot.com/mcp/",
			// Static, NON-secret headers only. The secret-bearing Authorization
			// header is injected from CredentialSecretRef — proving it is NOT
			// inlined here (admission would reject a secret header name anyway).
			Headers: map[string]string{"X-MCP-Client": "ksquad-e2e"},
			CredentialSecretRef: &ksquadv1alpha1.SecretRef{
				Name: mcpCredentialSecret,
				Key:  "token",
			},
			// Scope the tool envelope so the rendered config is provably narrow.
			ToolFilter: &ksquadv1alpha1.MCPToolFilter{
				Allow: []string{"get_*", "list_*", "search_*"},
				Deny:  []string{"delete_*"},
			},
			EgressRef: &ksquadv1alpha1.ObjectRef{Name: egressName},
		},
	}

	skill := &ksquadv1alpha1.Skill{
		ObjectMeta: meta(skillName),
		Spec: ksquadv1alpha1.SkillSpec{
			Source: ksquadv1alpha1.SkillSource{
				Type:   ksquadv1alpha1.SkillSourceInline,
				Inline: "# e2e smoke skill\nDrive a version-pinned kubectl + github-mcp Run for conformance.\n",
			},
			// The MCP tool endpoint granted to this skill (single declared server).
			McpToolRefs: []ksquadv1alpha1.ObjectRef{{Name: mcpServerName}},
			// The toolchain the Run must stage: kubectl@<version>, version-pinned.
			Requires: ksquadv1alpha1.SkillRequires{
				Toolchains: []string{toolchainName + "@" + kubectlVersion},
			},
		},
	}

	role := &ksquadv1alpha1.Role{
		ObjectMeta: meta(roleName),
		Spec:       ksquadv1alpha1.RoleSpec{DefaultSkills: []ksquadv1alpha1.ObjectRef{{Name: skillName}}},
	}

	agent := &ksquadv1alpha1.Agent{
		ObjectMeta: meta(agentName),
		Spec: ksquadv1alpha1.AgentSpec{
			RoleRef:   ksquadv1alpha1.ObjectRef{Name: roleName},
			SkillRefs: []ksquadv1alpha1.ObjectRef{{Name: skillName}},
		},
	}

	project := &ksquadv1alpha1.Project{
		ObjectMeta: meta(projectName),
		Spec: ksquadv1alpha1.ProjectSpec{
			Repo:            ksquadv1alpha1.RepoSpec{URL: "https://github.com/K8squad/K8squad.git", Ref: "main"},
			EgressPolicyRef: &ksquadv1alpha1.ObjectRef{Name: egressName},
		},
	}

	team := &ksquadv1alpha1.Team{
		ObjectMeta: meta(teamName),
		Spec: ksquadv1alpha1.TeamSpec{
			NamespaceStrategy: "adopt", // adopt the pre-created working namespace.
			Projects:          []ksquadv1alpha1.ObjectRef{{Name: projectName}},
			Agents:            []ksquadv1alpha1.ObjectRef{{Name: agentName}},
		},
	}

	run := &ksquadv1alpha1.Run{
		ObjectMeta: meta(runName),
		Spec: ksquadv1alpha1.RunSpec{
			TeamRef:     ksquadv1alpha1.ObjectRef{Name: teamName},
			ProjectRef:  ksquadv1alpha1.ObjectRef{Name: projectName},
			WorkItemRef: "e2e-smoke-workitem",
			Agents:      []ksquadv1alpha1.ObjectRef{{Name: agentName}},
			OwnedBy:     ksquadv1alpha1.PrincipalRef("e2e-smoke"),
		},
	}

	return &scenario{
		namespace:      ns,
		kubectlVersion: kubectlVersion,
		egress:         egress,
		mcpSecret:      mcpSecret,
		mcpServer:      mcpServer,
		skill:          skill,
		role:           role,
		agent:          agent,
		project:        project,
		team:           team,
		run:            run,
	}
}

// objectsCreateOrder returns the graph in dependency order (least-dependent
// first). The Run is created last, after every reference it names exists.
func (s *scenario) objectsCreateOrder() []client.Object {
	return []client.Object{
		s.egress, s.mcpSecret, s.mcpServer, s.skill, s.role, s.agent, s.project, s.team, s.run,
	}
}

// apply creates the whole graph idempotently and registers reverse-order
// cleanup via t.Cleanup. Any create error other than AlreadyExists fails the
// test (the graph is well-formed; a reject means the operator/webhooks disagree
// with the fixture — a real signal, not an environment gap).
func (s *scenario) apply(ctx context.Context, t *testing.T, cl client.Client) {
	t.Helper()
	created := s.objectsCreateOrder()
	for _, obj := range created {
		if err := cl.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("create %T %q: %v", obj, obj.GetName(), err)
		}
	}
	t.Cleanup(func() {
		delCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for i := len(created) - 1; i >= 0; i-- {
			_ = cl.Delete(delCtx, created[i])
		}
	})
}
