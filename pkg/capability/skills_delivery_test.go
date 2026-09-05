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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// skillsClient builds a fake client whose scheme covers both the CRDs and
// the core ConfigMaps the per-skill projection converges.
func skillsClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, api.AddToScheme(s))
	require.NoError(t, clientgoscheme.AddToScheme(s))
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

// inlineSkill returns a granted inline skill whose body embeds a would-be
// permission declaration (D8: the body tries to widen itself; delivery
// must ignore it).
func inlineSkill(name string, permissions []string) GrantedSkill {
	return GrantedSkill{
		Namespace:   runNS,
		Name:        name,
		SourceType:  api.SkillSourceInline,
		Inline:      "---\nname: " + name + "\n---\nUse task-io.\npermissions: [\"task-io:admin\"] <!-- untrusted self-widening attempt -->\n",
		Permissions: permissions,
	}
}

func gitSkill(name string) GrantedSkill {
	return GrantedSkill{
		Namespace:  runNS,
		Name:       name,
		SourceType: api.SkillSourceGit,
		Git:        &api.GitSkillSource{RepoRef: "github.com/acme/squad-skills", Ref: "1234567890abcdef"},
	}
}

// AC1/AC2/AC3: one inline skill → one ConfigMap with BOTH keys, the body
// verbatim and the envelope from the CR only.
func TestSkillConfigMapForShapeAndD8(t *testing.T) {
	run := newRun()
	skill := inlineSkill("task-io", []string{"task-io:read", "task-io:comment"})

	cm, err := SkillConfigMapFor(run, skill)
	require.NoError(t, err)

	assert.Equal(t, "ksquad-run-r1-skill-task-io", cm.Name)
	assert.Equal(t, runNS, cm.Namespace)
	assert.Equal(t, "ksquad-operator", cm.Labels["app.kubernetes.io/managed-by"])
	assert.Equal(t, run.Name, cm.Labels["ksquad.io/run"])
	assert.Equal(t, "skill", cm.Labels["ksquad.io/component"])
	assert.Equal(t, "task-io", cm.Labels["ksquad.io/skill"])
	require.Len(t, cm.OwnerReferences, 1)
	assert.True(t, *cm.OwnerReferences[0].Controller)

	// AC1: body bytes verbatim.
	assert.Equal(t, skill.Inline, cm.Data[SkillBodyFile])

	// AC2/D8: the envelope is exactly the CR permissions — the body's
	// embedded "task-io:admin" never reaches permissions.json.
	assert.JSONEq(t, `{"version":1,"permissions":["task-io:read","task-io:comment"]}`, cm.Data[SkillPermissionsFile])
	assert.NotContains(t, cm.Data[SkillPermissionsFile], "task-io:admin")
}

// AC2 fail-closed default: empty CR permissions → an EMPTY envelope,
// never absent, never a wildcard, never null.
func TestSkillConfigMapForEmptyPermissionsIsEmptyEnvelope(t *testing.T) {
	cm, err := SkillConfigMapFor(newRun(), inlineSkill("task-io", nil))
	require.NoError(t, err)
	assert.JSONEq(t, `{"version":1,"permissions":[]}`, cm.Data[SkillPermissionsFile])
}

// AC3/AC4: three inline skills → three DISTINCT ConfigMaps, git skipped,
// a removed skill's map GC'd, and a no-skills Run sweeps the set without
// touching the MCP map.
func TestEnsureSkillConfigMapsPerSkillConvergeAndGC(t *testing.T) {
	run := newRun()
	c := skillsClient(t)

	skills := []GrantedSkill{
		inlineSkill("task-io", []string{"task-io:read"}),
		inlineSkill("git-ops", nil),
		inlineSkill("k8s-debug", []string{"k8s:read"}),
		gitSkill("pinned-git"), // S-D owns git bodies — never projected here
	}
	require.NoError(t, EnsureSkillConfigMaps(context.Background(), c, run, skills))

	var live corev1.ConfigMapList
	require.NoError(t, c.List(context.Background(), &live, client.InNamespace(runNS)))
	assert.Len(t, live.Items, 3)
	for _, name := range []string{"ksquad-run-r1-skill-task-io", "ksquad-run-r1-skill-git-ops", "ksquad-run-r1-skill-k8s-debug"} {
		var cm corev1.ConfigMap
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: runNS, Name: name}, &cm), name)
		require.Len(t, cm.OwnerReferences, 1)
		require.Contains(t, cm.Data, SkillBodyFile)
		require.Contains(t, cm.Data, SkillPermissionsFile)
	}

	// Drift repair: corrupt one map's data, reconverge, expect repair.
	var corrupted corev1.ConfigMap
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: runNS, Name: "ksquad-run-r1-skill-task-io"}, &corrupted))
	corrupted.Data[SkillBodyFile] = "tampered"
	require.NoError(t, c.Update(context.Background(), &corrupted))
	require.NoError(t, EnsureSkillConfigMaps(context.Background(), c, run, skills))
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: runNS, Name: "ksquad-run-r1-skill-task-io"}, &corrupted))
	assert.Equal(t, skills[0].Inline, corrupted.Data[SkillBodyFile])

	// GC of a dropped skill: shrink the set, the stale map disappears.
	shrunk := []GrantedSkill{skills[0]}
	require.NoError(t, EnsureSkillConfigMaps(context.Background(), c, run, shrunk))
	require.NoError(t, c.List(context.Background(), &live, client.InNamespace(runNS)))
	assert.Len(t, live.Items, 1)
	assert.Equal(t, "ksquad-run-r1-skill-task-io", live.Items[0].Name)

	// No skills at all → everything swept (bare posture), and the MCP IR
	// map for the same Run is NOT the projection's to delete.
	mcp, err := MCPConfigMapFor(run, []Endpoint{httpEndpoint()})
	require.NoError(t, err)
	require.NoError(t, c.Create(context.Background(), mcp))
	require.NoError(t, EnsureSkillConfigMaps(context.Background(), c, run, nil))
	require.NoError(t, c.List(context.Background(), &live, client.InNamespace(runNS)))
	require.Len(t, live.Items, 1)
	assert.Equal(t, "ksquad-run-r1-mcp", live.Items[0].Name)
}

// AC6 backstop: a per-skill projection over the 1 MiB ConfigMap ceiling
// fails closed naming the skill and suggesting git — never truncation.
func TestSkillConfigMapForOverLimitFailsClosed(t *testing.T) {
	skill := inlineSkill("huge", []string{"x:read"})
	skill.Inline = strings.Repeat("a", (1<<20)+1)
	_, err := SkillConfigMapFor(newRun(), skill)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "huge")
	assert.Contains(t, err.Error(), "git")
}

// AC1/AC3 pod path: per-skill volumes mounted at distinct
// ${SkillsMountPath}/<name>/ subdirs under one root, env names the root;
// git skills get no volume; no skills → bare posture unchanged.
func TestAssemblePodSkillsVolumesEnvAndBare(t *testing.T) {
	run := newRun()
	skills := []GrantedSkill{
		inlineSkill("task-io", []string{"task-io:read"}),
		inlineSkill("k8s-debug", nil),
		gitSkill("pinned-git"),
	}
	asm, err := AssemblePod(run, nil, nil, skills)
	require.NoError(t, err)

	assert.Len(t, asm.Volumes, 2)
	assert.Len(t, asm.AgentMounts, 2)
	mountPaths := map[string]bool{}
	for _, m := range asm.AgentMounts {
		assert.True(t, m.ReadOnly)
		mountPaths[m.MountPath] = true
	}
	assert.Equal(t, map[string]bool{
		"/ksquad/skills/task-io":   true,
		"/ksquad/skills/k8s-debug": true,
	}, mountPaths)
	assert.Equal(t, SkillsMountPath, envValue(t, asm.AgentEnv, SkillsDirEnvVar))

	// Volume names are DNS_LABEL-safe even for dot-bearing skill names.
	volName := SkillsVolumeName("acme.corp.some-very-long-skill-name-exceeding-the-63-character-dns-label-limit-for-sure")
	assert.LessOrEqual(t, len(volName), 63)

	// Bare posture: no skills → no env, no volumes (AC4 tail).
	bare, err := AssemblePod(newRun(), nil, nil, nil)
	require.NoError(t, err)
	assert.Empty(t, bare.Volumes)
	assert.Empty(t, bare.AgentMounts)
	for _, e := range bare.AgentEnv {
		assert.NotEqual(t, SkillsDirEnvVar, e.Name)
	}
}
