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

package v1alpha1

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ISI-4431 E3: Role↔phase binding + coordinator flag. The webhook rejects
// unknown activePhases strings and invalid coordinatorMode values, and warns
// (does not reject) when coordinatorMode rides a non-coordinator role.

func roleWith(spec RoleSpec) *Role {
	if spec.PromptRef.Name == "" {
		spec.PromptRef = ObjectRef{Name: "role-x-prompt"}
	}
	return &Role{
		ObjectMeta: metav1.ObjectMeta{Name: "role-x", Namespace: "bmad-squad"},
		Spec:       spec,
	}
}

// TestRoleBackCompatPhaseAgnostic proves an existing Role (activePhases=nil,
// no coordinator fields) is admitted with no warnings — the back-compat AC.
func TestRoleBackCompatPhaseAgnostic(t *testing.T) {
	ctx := context.Background()
	r := roleWith(RoleSpec{RuntimeClassHint: "gvisor"})
	w, err := r.ValidateCreate(ctx, r)
	require.NoError(t, err)
	assert.Empty(t, w, "phase-agnostic Role (activePhases=nil) must admit cleanly")
}

func TestRoleAdmittedFixtures(t *testing.T) {
	ctx := context.Background()
	cases := map[string]RoleSpec{
		"all-six-phases": {ActivePhases: []string{
			"design", "planning", "implementation", "code_review", "testing", "documentation"}},
		"phase-bound-architect":  {ActivePhases: []string{"design", "planning"}},
		"coordinator-auto":       {Coordinator: true, CoordinatorMode: "auto"},
		"coordinator-propose":    {Coordinator: true, CoordinatorMode: "propose"},
		"coordinator-no-mode":    {Coordinator: true},
		"coordinator-with-phase": {Coordinator: true, CoordinatorMode: "auto", ActivePhases: []string{"implementation"}},
	}
	for name, spec := range cases {
		r := roleWith(spec)
		w, err := r.ValidateCreate(ctx, r)
		require.NoError(t, err, name)
		assert.Empty(t, w, name)
	}
}

func TestRoleRejectsUnknownPhase(t *testing.T) {
	ctx := context.Background()
	// "done" is a terminal lane, never a working phase — a config error.
	for _, bad := range []string{"done", "todo", "backlog", "cancelled", "in_progress", "bogus"} {
		r := roleWith(RoleSpec{ActivePhases: []string{"design", bad}})
		_, err := r.ValidateCreate(ctx, r)
		require.Error(t, err, "activePhases=%q must be rejected", bad)
		assert.Contains(t, err.Error(), "activePhases", bad)
	}
}

func TestRoleCoordinatorModeWithoutCoordinatorWarns(t *testing.T) {
	ctx := context.Background()
	r := roleWith(RoleSpec{CoordinatorMode: "auto"})
	w, err := r.ValidateCreate(ctx, r)
	require.NoError(t, err, "coordinatorMode without coordinator is normalized (inert), not rejected")
	require.Len(t, w, 1, "a warning must flag the ignored coordinatorMode")
	assert.Contains(t, w[0], "ignored")
	assert.Contains(t, w[0], "coordinator")
}

func TestRoleRejectsInvalidCoordinatorMode(t *testing.T) {
	ctx := context.Background()
	r := roleWith(RoleSpec{Coordinator: true, CoordinatorMode: "yolo"})
	_, err := r.ValidateCreate(ctx, r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "coordinatorMode")
}

// TestRoleValidateUpdateMirrorsCreate proves update runs the same checks.
func TestRoleValidateUpdateMirrorsCreate(t *testing.T) {
	ctx := context.Background()
	old := roleWith(RoleSpec{})
	bad := roleWith(RoleSpec{ActivePhases: []string{"deployment"}})
	_, err := bad.ValidateUpdate(ctx, old, bad)
	require.Error(t, err, "update must reject an unknown phase just like create")
}

// TestRoleActivePhasesEnumFailsClosed is the golden CRD assertion: the
// generated Role CRD constrains activePhases items to exactly the six middle
// working phases (defense in depth beyond the webhook; AC "verify golden CRD
// YAML diff"). backlog/todo/done/cancelled must be inadmissible.
func TestRoleActivePhasesEnumFailsClosed(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "ksquad.io_roles.yaml"))
	require.NoError(t, err, "generated Role CRD must exist (run `make manifests`)")

	var doc map[string]interface{}
	require.NoError(t, yaml.Unmarshal(raw, &doc))

	versions := doc["spec"].(map[string]interface{})["versions"].([]interface{})
	schema := versions[0].(map[string]interface{})["schema"].(map[string]interface{})["openAPIV3Schema"].(map[string]interface{})
	props := schema["properties"].(map[string]interface{})["spec"].(map[string]interface{})["properties"].(map[string]interface{})

	activePhases, ok := props["activePhases"].(map[string]interface{})
	require.True(t, ok, "spec.activePhases must be on the generated CRD")
	items, ok := activePhases["items"].(map[string]interface{})
	require.True(t, ok, "spec.activePhases must be an array with items")
	rawEnum, ok := items["enum"].([]interface{})
	require.True(t, ok, "spec.activePhases.items must carry an enum — fail-closed validation is missing")

	got := make([]string, 0, len(rawEnum))
	for _, v := range rawEnum {
		got = append(got, v.(string))
	}
	assert.ElementsMatch(t,
		[]string{"design", "planning", "implementation", "code_review", "testing", "documentation"},
		got, "activePhases enum must be exactly the six working phases")
	assert.NotContains(t, got, "done", "terminal lanes must not be admissible activePhases")
	assert.NotContains(t, got, "todo", "intake lanes must not be admissible activePhases")
}
