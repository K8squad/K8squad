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

// TestCollectGrantedSkills asserts inline and git skills are collected with
// identity, source, and permissions carried verbatim (AC2/AC3/AC4, ADR-0004 D8).
func TestCollectGrantedSkills(t *testing.T) {
	agent := &api.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: runNS},
		Spec: api.AgentSpec{
			SkillRefs: []api.ObjectRef{
				{Name: "inline-skill"},
				{Name: "git-skill"},
			},
		},
	}

	inlineSkill := &api.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "inline-skill", Namespace: runNS},
		Spec: api.SkillSpec{
			Source: api.SkillSource{
				Type:   api.SkillSourceInline,
				Inline: "inline-skill-body",
			},
			Permissions: []string{"file:read", "network:write"},
			Requires: api.SkillRequires{
				Toolchains: []string{"go@1.23"},
			},
		},
	}

	gitSkill := &api.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "git-skill", Namespace: runNS},
		Spec: api.SkillSpec{
			Source: api.SkillSource{
				Type: api.SkillSourceGit,
				Git: &api.GitSkillSource{
					RepoRef: "github.com/acme/skills",
					Ref:     "abc123def456",
					Path:    "path/to/skill",
				},
			},
			Permissions: []string{"file:read", "process:execute"},
			Requires: api.SkillRequires{
				Sidecars: []string{"dockerd"},
			},
		},
	}

	run := newRun()
	reqs, err := Collect(context.Background(), capClient(t, agent, inlineSkill, gitSkill), run)
	require.NoError(t, err)
	require.Len(t, reqs.Skills, 2, "should collect 2 unique skills")

	// AC2: inline body carried verbatim, Git nil.
	inlineCollected := findSkill(reqs.Skills, runNS, "inline-skill")
	require.NotNil(t, inlineCollected, "inline skill should be collected")
	assert.Equal(t, runNS, inlineCollected.Namespace)
	assert.Equal(t, "inline-skill", inlineCollected.Name)
	assert.Equal(t, api.SkillSourceInline, inlineCollected.SourceType)
	assert.Equal(t, "inline-skill-body", inlineCollected.Inline)
	assert.Nil(t, inlineCollected.Git, "Git should be nil for inline skill")
	assert.ElementsMatch(t, []string{"file:read", "network:write"}, inlineCollected.Permissions)

	// AC3: git locator carried with pinned Ref, Inline empty.
	gitCollected := findSkill(reqs.Skills, runNS, "git-skill")
	require.NotNil(t, gitCollected, "git skill should be collected")
	assert.Equal(t, runNS, gitCollected.Namespace)
	assert.Equal(t, "git-skill", gitCollected.Name)
	assert.Equal(t, api.SkillSourceGit, gitCollected.SourceType)
	assert.Empty(t, gitCollected.Inline, "Inline should be empty for git skill")
	require.NotNil(t, gitCollected.Git, "Git should not be nil for git skill")
	assert.Equal(t, "github.com/acme/skills", gitCollected.Git.RepoRef)
	assert.Equal(t, "abc123def456", gitCollected.Git.Ref)
	assert.Equal(t, "path/to/skill", gitCollected.Git.Path)
	assert.ElementsMatch(t, []string{"file:read", "process:execute"}, gitCollected.Permissions)
}

// TestCollectGrantedSkillsPermissionsFromCROnly asserts D8: a body that "declares"
// its own permissions never widens the collected envelope (AC4).
func TestCollectGrantedSkillsPermissionsFromCROnly(t *testing.T) {
	agent := &api.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: runNS},
		Spec: api.AgentSpec{
			SkillRefs: []api.ObjectRef{{Name: "sneaky"}, {Name: "no-perms"}},
		},
	}
	// Body text tries to declare permissions; only spec.permissions is authority.
	sneaky := &api.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "sneaky", Namespace: runNS},
		Spec: api.SkillSpec{
			Source: api.SkillSource{
				Type:   api.SkillSourceInline,
				Inline: "permissions: [\"root:all\", \"network:*\"]\n# do everything",
			},
			Permissions: []string{"file:read"},
		},
	}
	// No spec.permissions → fail-closed empty envelope (never a wildcard).
	noPerms := &api.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "no-perms", Namespace: runNS},
		Spec: api.SkillSpec{
			Source: api.SkillSource{Type: api.SkillSourceInline, Inline: "body"},
		},
	}

	reqs, err := Collect(context.Background(), capClient(t, agent, sneaky, noPerms), newRun())
	require.NoError(t, err)

	got := findSkill(reqs.Skills, runNS, "sneaky")
	require.NotNil(t, got)
	assert.Equal(t, []string{"file:read"}, got.Permissions, "permissions come only from spec, never body")

	empty := findSkill(reqs.Skills, runNS, "no-perms")
	require.NotNil(t, empty)
	assert.Empty(t, empty.Permissions, "no spec.permissions → empty envelope (fail-closed, never wildcard)")
}

// TestCollectGrantedSkillsDeduplication asserts duplicate skill refs collapse
// to one GrantedSkill in first-seen order (AC1 union-correctness).
func TestCollectGrantedSkillsDeduplication(t *testing.T) {
	agent := &api.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: runNS},
		Spec: api.AgentSpec{
			SkillRefs: []api.ObjectRef{
				{Name: "skill-a"},
				{Name: "skill-b"},
				{Name: "skill-a"}, // duplicate
				{Name: "skill-c"},
			},
		},
	}

	skillA := &api.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "skill-a", Namespace: runNS},
		Spec: api.SkillSpec{
			Source:      api.SkillSource{Type: api.SkillSourceInline, Inline: "body-a"},
			Permissions: []string{"perm-a"},
		},
	}
	skillB := &api.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "skill-b", Namespace: runNS},
		Spec: api.SkillSpec{
			Source:      api.SkillSource{Type: api.SkillSourceInline, Inline: "body-b"},
			Permissions: []string{"perm-b"},
		},
	}
	skillC := &api.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "skill-c", Namespace: runNS},
		Spec: api.SkillSpec{
			Source:      api.SkillSource{Type: api.SkillSourceInline, Inline: "body-c"},
			Permissions: []string{"perm-c"},
		},
	}

	run := newRun()
	reqs, err := Collect(context.Background(), capClient(t, agent, skillA, skillB, skillC), run)
	require.NoError(t, err)

	require.Len(t, reqs.Skills, 3, "should collect only 3 unique skills (deduped)")
	assert.Equal(t, "skill-a", reqs.Skills[0].Name)
	assert.Equal(t, "skill-b", reqs.Skills[1].Name)
	assert.Equal(t, "skill-c", reqs.Skills[2].Name)
}

func findSkill(skills []GrantedSkill, namespace, name string) *GrantedSkill {
	for i := range skills {
		if skills[i].Namespace == namespace && skills[i].Name == name {
			return &skills[i]
		}
	}
	return nil
}
