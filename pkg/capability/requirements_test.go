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

package capability

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func capScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, api.AddToScheme(s))
	return s
}

func capClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(capScheme(t)).WithObjects(objs...).Build()
}

const runNS = "bmad-squad"

func newRun() *api.Run {
	return &api.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: runNS, UID: "run-uid-1"},
		Spec: api.RunSpec{
			Agents: []api.ObjectRef{{Name: "coder"}},
		},
	}
}

func TestCollectUnionsAgentSkillsAndRoleDefaults(t *testing.T) {
	agent := &api.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: runNS},
		Spec: api.AgentSpec{
			RoleRef:   api.ObjectRef{Name: "dev"},
			SkillRefs: []api.ObjectRef{{Name: "kubectl-restart"}},
		},
	}
	role := &api.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: runNS},
		Spec: api.RoleSpec{
			DefaultSkills: []api.ObjectRef{{Name: "git-ops"}},
		},
	}
	skillA := &api.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "kubectl-restart", Namespace: runNS},
		Spec: api.SkillSpec{
			Requires: api.SkillRequires{Toolchains: []string{"kubectl@1.31"}},
			McpToolRefs: []api.ObjectRef{
				{Name: "github-mcp"},
			},
		},
	}
	skillB := &api.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "git-ops", Namespace: runNS},
		Spec: api.SkillSpec{
			Requires: api.SkillRequires{
				Toolchains: []string{"git@2.62", "kubectl@1.31"}, // duplicate ref dedupes
				Sidecars:   []string{"dockerd"},
			},
		},
	}
	run := newRun()

	reqs, err := Collect(context.Background(), capClient(t, agent, role, skillA, skillB), run)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"kubectl@1.31", "git@2.62"}, reqs.ToolchainRefs)
	require.Len(t, reqs.MCPRefs, 1)
	assert.Equal(t, "github-mcp", reqs.MCPRefs[0].Name)
	assert.Equal(t, runNS+"/kubectl-restart", reqs.MCPSources[runNS+"/github-mcp"])
	assert.Equal(t, []string{"dockerd"}, reqs.Sidecars)
}

func TestCollectToleratesMissingObjects(t *testing.T) {
	run := newRun() // agent does not exist
	reqs, err := Collect(context.Background(), capClient(t), run)
	require.NoError(t, err)
	assert.Empty(t, reqs.ToolchainRefs)
	assert.Empty(t, reqs.MCPRefs)
}

func TestCollectCrossNamespaceSkillRef(t *testing.T) {
	agent := &api.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: runNS},
		Spec: api.AgentSpec{
			SkillRefs: []api.ObjectRef{{Namespace: "shared-skills", Name: "s"}},
		},
	}
	skill := &api.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "shared-skills"},
		Spec: api.SkillSpec{
			McpToolRefs: []api.ObjectRef{{Namespace: "shared-servers", Name: "dt-mcp"}},
		},
	}
	reqs, err := Collect(context.Background(), capClient(t, agent, skill), newRun())
	require.NoError(t, err)
	require.Len(t, reqs.MCPRefs, 1)
	assert.Equal(t, "shared-servers", reqs.MCPRefs[0].Namespace)
	assert.Equal(t, "shared-skills/s", reqs.MCPSources["shared-servers/dt-mcp"])
}
