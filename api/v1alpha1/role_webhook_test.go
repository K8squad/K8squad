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

// ISI-7946 WS-A: org-chart edge + manager process validation on the Role
// webhook, and the Default() label mirror.

func TestRoleReportsToSelfEdgeRejected(t *testing.T) {
	ctx := context.Background()
	r := roleWith(RoleSpec{ReportsTo: &ObjectRef{Name: "role-x"}})
	_, err := r.ValidateCreate(ctx, r)
	require.Error(t, err, "a role cannot report to itself")
	assert.Contains(t, err.Error(), "reportsTo")
}

func TestRoleReportsToCrossNamespaceRejected(t *testing.T) {
	ctx := context.Background()
	r := roleWith(RoleSpec{ReportsTo: &ObjectRef{Name: "other", Namespace: "other-ns"}})
	_, err := r.ValidateCreate(ctx, r)
	require.Error(t, err, "reporting edges are namespace-local")
	assert.Contains(t, err.Error(), "namespace")
}

func TestRoleReportsToValidManagerAdmits(t *testing.T) {
	ctx := context.Background()
	r := roleWith(RoleSpec{ReportsTo: &ObjectRef{Name: "architect"}})
	w, err := r.ValidateCreate(ctx, r)
	require.NoError(t, err)
	assert.Empty(t, w)
}

func TestRoleProcessValidation(t *testing.T) {
	ctx := context.Background()
	// empty process (present but zero stages) is a config error
	r := roleWith(RoleSpec{Process: []ProcessStage{}})
	_, err := r.ValidateCreate(ctx, r)
	require.Error(t, err, "an explicitly empty process must be rejected")
	assert.Contains(t, err.Error(), "process")

	// duplicate stage names make the ordered pipeline ambiguous
	r2 := roleWith(RoleSpec{Process: []ProcessStage{
		{Stage: "code", RoleRef: ObjectRef{Name: "coder"}},
		{Stage: "code", RoleRef: ObjectRef{Name: "reviewer"}},
	}})
	_, err = r2.ValidateCreate(ctx, r2)
	require.Error(t, err, "duplicate stage names must be rejected")
	assert.Contains(t, err.Error(), "process")

	// a well-formed manager process admits (eng mgr code->review->test)
	r3 := roleWith(RoleSpec{Process: []ProcessStage{
		{Stage: "code", RoleRef: ObjectRef{Name: "coder"}},
		{Stage: "review", RoleRef: ObjectRef{Name: "reviewer"}},
		{Stage: "test", RoleRef: ObjectRef{Name: "tester"}},
	}})
	w, err := r3.ValidateCreate(ctx, r3)
	require.NoError(t, err)
	assert.Empty(t, w)
}

func TestRoleLegacyReportsToLabelWarns(t *testing.T) {
	ctx := context.Background()
	r := roleWith(RoleSpec{})
	r.Labels = map[string]string{LabelReportsTo: "architect"}
	w, err := r.ValidateCreate(ctx, r)
	require.NoError(t, err, "legacy label-only chart must stay admissible (compat)")
	require.NotEmpty(t, w, "a warning must nudge migration to spec.reportsTo")
	assert.Contains(t, w[0], "reportsTo")
}

func TestRoleDefaultDerivesReportsToLabel(t *testing.T) {
	ctx := context.Background()
	// spec set → label derived
	r := roleWith(RoleSpec{ReportsTo: &ObjectRef{Name: "architect"}})
	require.NoError(t, r.Default(ctx, r))
	assert.Equal(t, "architect", r.Labels[LabelReportsTo])
	// spec unset → legacy label preserved
	r2 := roleWith(RoleSpec{})
	r2.Labels = map[string]string{LabelReportsTo: "ceo"}
	require.NoError(t, r2.Default(ctx, r2))
	assert.Equal(t, "ceo", r2.Labels[LabelReportsTo])
	// spec wins over a stale label
	r3 := roleWith(RoleSpec{ReportsTo: &ObjectRef{Name: "boss"}})
	r3.Labels = map[string]string{LabelReportsTo: "stale"}
	require.NoError(t, r3.Default(ctx, r3))
	assert.Equal(t, "boss", r3.Labels[LabelReportsTo])
}

// TestRoleOrgChartCRDShape is the golden CRD assertion for the WS-A fields
// (defense in depth like TestRoleActivePhasesEnumFailsClosed): reportsTo
// and process must be compiled into the generated Role CRD, process stages
// map-keyed, and the mutating webhook (label mirror) emitted.
func TestRoleOrgChartCRDShape(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "ksquad.io_roles.yaml"))
	require.NoError(t, err, "generated Role CRD must exist (run `make manifests`)")

	var doc map[string]interface{}
	require.NoError(t, yaml.Unmarshal(raw, &doc))

	versions := doc["spec"].(map[string]interface{})["versions"].([]interface{})
	schema := versions[0].(map[string]interface{})["schema"].(map[string]interface{})["openAPIV3Schema"].(map[string]interface{})
	props := schema["properties"].(map[string]interface{})["spec"].(map[string]interface{})["properties"].(map[string]interface{})

	_, ok := props["reportsTo"].(map[string]interface{})
	assert.True(t, ok, "spec.reportsTo must be on the generated Role CRD")
	process, ok := props["process"].(map[string]interface{})
	require.True(t, ok, "spec.process must be on the generated Role CRD")
	assert.Equal(t, "map", process["x-kubernetes-list-type"], "process stages must be map-keyed (unique stage names)")

	webhookRaw, err := os.ReadFile(filepath.Join("..", "..", "config", "webhook", "manifests.yaml"))
	require.NoError(t, err, "generated webhook manifests must exist")
	assert.Contains(t, string(webhookRaw), "mrole-orgchart", "the Role mutating webhook (label mirror) must be in the generated manifests")
}
