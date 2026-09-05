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

package rundrive

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	wire "github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/capability"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// skillsDispatchFor builds an operatorDispatch over a fake client whose
// Run manifest grants the given skills, with each inline skill's projected
// ConfigMap pre-created (the reconciler's S-B output).
func skillsDispatchFor(t *testing.T, manifest *api.CapabilityManifest, cms ...*corev1.ConfigMap) *operatorDispatch {
	t.Helper()
	run := &api.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "team-a", UID: types.UID(taskIORunUID)},
		Spec: api.RunSpec{
			TeamRef:     api.ObjectRef{Name: "team-a"},
			ProjectRef:  api.ObjectRef{Name: "proj-1"},
			WorkItemRef: "wi-1",
			Agents:      []api.ObjectRef{{Name: "coder"}},
		},
		Status: api.RunStatus{CapabilityManifest: manifest},
	}
	agent := &api.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: "team-a"},
		Spec:       api.AgentSpec{Model: "claude-sonnet-4"},
	}
	objs := []client.Object{run, agent}
	objs = append(objs, toObjects(cms)...)
	cl := fake.NewClientBuilder().WithScheme(dispatchScheme(t)).WithObjects(objs...).Build()
	return &operatorDispatch{
		cfg: OperatorDispatchConfig{
			DB:     lazyDB(t),
			Client: cl,
		},
		shimBin: "shim",
	}
}

func toObjects(cms []*corev1.ConfigMap) []client.Object {
	out := make([]client.Object, 0, len(cms))
	for _, cm := range cms {
		out = append(out, cm)
	}
	return out
}

func skillCM(t *testing.T, run *api.Run, skill capability.GrantedSkill) *corev1.ConfigMap {
	t.Helper()
	cm, err := capability.SkillConfigMapFor(run, skill)
	require.NoError(t, err)
	return cm
}

// AC5: the operator path materializes the whole per-skill tree under a
// temp KSQUAD_SKILLS_DIR — <name>/SKILL.md + <name>/permissions.json at
// 0o600 — and shimCommand sets the env; git-sourced skills are skipped.
func TestShimEnvMaterializesSkillsTree(t *testing.T) {
	run := &api.Run{ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "team-a", UID: types.UID(taskIORunUID)}}
	inline := capability.GrantedSkill{
		Namespace: "team-a", Name: "task-io",
		SourceType:  api.SkillSourceInline,
		Inline:      "# task-io\nRead and comment on your own task.\n",
		Permissions: []string{"task-io:read", "task-io:comment"},
	}
	git := capability.GrantedSkill{
		Namespace: "team-a", Name: "pinned-git",
		SourceType: api.SkillSourceGit,
		Git:        &api.GitSkillSource{RepoRef: "github.com/acme/squad-skills", Ref: "1234567890abcdef"},
	}
	manifest := &api.CapabilityManifest{Skills: []api.GrantedSkill{toAPIGranted(inline), toAPIGranted(git)}}

	d := skillsDispatchFor(t, manifest, skillCM(t, run, inline))

	cmd, err := d.shimCommand(context.Background(), wire.Task{A2ATaskID: taskIORunUID})
	require.NoError(t, err)
	em := envMap(t, cmd.Env)

	dir, ok := em[capability.SkillsDirEnvVar]
	require.True(t, ok, "KSQUAD_SKILLS_DIR expected in shim env")
	assert.NotEmpty(t, dir)

	body, err := os.ReadFile(filepath.Join(dir, "task-io", capability.SkillBodyFile))
	require.NoError(t, err)
	assert.Equal(t, inline.Inline, string(body))

	perms, err := os.ReadFile(filepath.Join(dir, "task-io", capability.SkillPermissionsFile))
	require.NoError(t, err)
	assert.JSONEq(t, `{"version":1,"permissions":["task-io:read","task-io:comment"]}`, string(perms))

	// 0o600 on both files, 0o700 on the skill dir.
	fi, err := os.Stat(filepath.Join(dir, "task-io", capability.SkillBodyFile))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	fi, err = os.Stat(filepath.Join(dir, "task-io", capability.SkillPermissionsFile))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	fi, err = os.Stat(filepath.Join(dir, "task-io"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), fi.Mode().Perm())

	// The git-sourced skill got no inline tree entry (S-D owns it).
	_, err = os.Stat(filepath.Join(dir, "pinned-git"))
	assert.True(t, os.IsNotExist(err))
}

// AC5 tail: a Run with no granted skills sets no KSQUAD_SKILLS_DIR.
func TestShimEnvNoSkillsNoDir(t *testing.T) {
	d := skillsDispatchFor(t, &api.CapabilityManifest{})
	cmd, err := d.shimCommand(context.Background(), wire.Task{A2ATaskID: taskIORunUID})
	require.NoError(t, err)
	_, present := envMap(t, cmd.Env)[capability.SkillsDirEnvVar]
	assert.False(t, present)
}

// Fail-closed: a manifest granting an inline skill whose projected
// ConfigMap is missing aborts the dispatch — never a silent partial tree.
func TestShimEnvMissingSkillConfigMapFailsClosed(t *testing.T) {
	inline := capability.GrantedSkill{
		Namespace: "team-a", Name: "task-io",
		SourceType: api.SkillSourceInline, Inline: "body",
	}
	d := skillsDispatchFor(t, &api.CapabilityManifest{Skills: []api.GrantedSkill{toAPIGranted(inline)}})
	_, err := d.shimCommand(context.Background(), wire.Task{A2ATaskID: taskIORunUID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ksquad-run-run-1-skill-task-io")
}

func toAPIGranted(s capability.GrantedSkill) api.GrantedSkill {
	return api.GrantedSkill{
		Namespace:   s.Namespace,
		Name:        s.Name,
		SourceType:  s.SourceType,
		Inline:      s.Inline,
		Git:         s.Git,
		Permissions: s.Permissions,
	}
}
