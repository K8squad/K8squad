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
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Story 1.2 AC8 self-check. These tests intentionally use only standard
// testing plus the deps story 1.1 already pinned (testify is in go.mod).

func boolPtr(b bool) *bool    { return &b }
func int64Ptr(i int64) *int64 { return &i }
func int32Ptr(i int32) *int32 { return &i }
func quantity(s string) resource.Quantity {
	q := resource.MustParse(s)
	return q
}

// sampleTeam/Agent/Role/Skill/Project/Run build rich specimens exercising
// every spec/status field defined in story 1.2 (arch §5.1).

func sampleTeam() *Team {
	return &Team{
		ObjectMeta: metav1.ObjectMeta{Name: "squad-alpha", Namespace: "squad-alpha"},
		Spec: TeamSpec{
			Projects:          []ObjectRef{{Name: "widget"}, {Name: "gadget", Namespace: "other"}},
			Agents:            []ObjectRef{{Name: "amelia"}, {Name: "backup"}},
			NamespaceStrategy: "dedicated",
		},
		Status: TeamStatus{
			Conditions: []metav1.Condition{{
				Type: "Ready", Status: metav1.ConditionTrue,
				Reason: "NamespaceEnsured", Message: "namespace + RBAC ensured",
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
}

func sampleAgent() *Agent {
	return &Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "amelia", Namespace: "squad-alpha"},
		Spec: AgentSpec{
			RuntimeRef:          ObjectRef{Name: "openclaw"},
			RoleRef:             ObjectRef{Name: "coder"},
			SkillRefs:           []ObjectRef{{Name: "pg-migrate"}, {Name: "k8s-deploy"}},
			CredentialSecretRef: SecretRef{Name: "amelia-claude"},
			CapabilityOverrides: &CapabilityOverrides{
				Allow: []string{"docker"},
				Deny:  []string{"packageInstall"},
			},
			Model:                 "claude-sonnet-4",
			ModelEndpointRef:      &SecretRef{Name: "amelia-ollama", Key: "baseUrl"},
			ContextBudgetOverride: &ContextBudget{WorkItem: int64Ptr(32000), MemoryRecall: int64Ptr(8000)},
			FallbackModel: &FallbackModel{
				Model:            "llama3.1",
				ModelEndpointRef: &SecretRef{Name: "amelia-ollama"},
			},
		},
	}
}

func sampleRole() *Role {
	return &Role{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: "squad-alpha"},
		Spec: RoleSpec{
			PromptRef:        ObjectRef{Name: "coder-prompt"},
			DefaultSkills:    []ObjectRef{{Name: "go-toolchain"}},
			RuntimeClassHint: "gvisor",
		},
	}
}

func sampleSkill() *Skill {
	return &Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-migrate", Namespace: "squad-alpha"},
		Spec: SkillSpec{
			Source: SkillSource{
				Type: SkillSourceGit,
				Git: &GitSkillSource{
					RepoRef:             "github.com/acme/squad-skills",
					Ref:                 "3f2a9c1",
					Path:                "skills/pg-migrate",
					CredentialSecretRef: &SecretRef{Name: "acme-skills-ro"},
				},
			},
			McpToolRefs: []ObjectRef{{Name: "postgres-mcp"}},
			Permissions: []string{"db.migrate", "db.read"},
			Requires:    SkillRequires{Toolchains: []string{"go@1.23"}, Sidecars: []string{"dockerd"}},
		},
	}
}

func sampleProject() *Project {
	return &Project{
		ObjectMeta: metav1.ObjectMeta{Name: "widget", Namespace: "squad-alpha"},
		Spec: ProjectSpec{
			Repo: RepoSpec{
				URL:  "https://github.com/acme/widget",
				Ref:  "main",
				Auth: &RepoAuth{CredentialSecretRef: SecretRef{Name: "widget-github-ro"}},
				Sync: &RepoSyncSpec{
					Provider:         "github",
					WebhookSecretRef: &SecretRef{Name: "widget-webhook-hmac"},
					Mirror:           &RepoMirrorSpec{Issues: boolPtr(true), PullRequests: boolPtr(true), CheckRuns: boolPtr(true), Artifacts: boolPtr(false)},
					ReflectOutbound:  true,
				},
			},
			WorkspacePVC:    &PVCSpec{Size: quantity("50Gi"), Class: "fast-ssd"},
			EgressPolicyRef: &ObjectRef{Name: "default-deny"},
			Goals:           []string{"ship v2 API", "keep p95 < 200ms"},
			ContextBudget:   &ContextBudget{WorkItem: int64Ptr(16000), ProjectDocs: int64Ptr(24000), MemoryRecall: int64Ptr(8000), Artifacts: int64Ptr(4000)},
		},
	}
}

func sampleRun() *Run {
	return &Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-8f2", Namespace: "squad-alpha"},
		Spec: RunSpec{
			TeamRef:       ObjectRef{Name: "squad-alpha"},
			ProjectRef:    ObjectRef{Name: "widget"},
			WorkItemRef:   "wi_01HZX9K7QM3V5R8T2Y", // opaque coordination-DB pointer (ADR-001)
			Inputs:        map[string]string{"milestone": "m3", "verbose": "true"},
			SandboxPolicy: SandboxPolicy{RuntimeClass: "gvisor", Class: "interactive"},
			Agents:        []ObjectRef{{Name: "amelia"}},
			RetryPolicy:   &RetryPolicy{MaxRetries: int32Ptr(3), BackoffSeconds: int32Ptr(30)},
		},
		Status: RunStatus{
			Phase:      RunPhaseRunning,
			SandboxRef: &ObjectRef{Name: "sandbox-7c1", Namespace: "squad-alpha"},
			ClaimedAt:  &metav1.Time{Time: metav1.Now().Time},
			Conditions: []metav1.Condition{{
				Type: "SandboxReady", Status: metav1.ConditionTrue,
				Reason: "WarmPoolClaim", Message: "claimed warm gvisor pod",
				LastTransitionTime: metav1.Now(),
			}},
			ArtifactRefs:       []ObjectRef{{Name: "art_9a1"}, {Name: "art_9a2"}},
			ObservedGeneration: 4,
		},
	}
}

// TestDeepCopyRoundTrip constructs each of the six types and round-trips it
// through the generated DeepCopy (story 1.2 AC8): regeneration drift or a
// hand-edited zz_generated.deepcopy.go fails here.
func TestDeepCopyRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		obj  interface{ DeepCopyObject() runtime.Object }
	}{
		{"Team", sampleTeam()},
		{"Agent", sampleAgent()},
		{"Role", sampleRole()},
		{"Skill", sampleSkill()},
		{"Project", sampleProject()},
		{"Run", sampleRun()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cp := tc.obj.DeepCopyObject()
			require.NotNil(t, cp)
			assert.Equal(t, tc.obj, cp, "DeepCopy of %s must round-trip equal", tc.name)
		})
	}
}

// TestRunWorkItemRefIsOpaqueString pins ADR-001 (story 1.2 AC6/AC7) at the
// Go-type level: spec.workItemRef must be a plain string — an opaque
// coordination-DB pointer — and no Run field may embed a work item.
func TestRunWorkItemRefIsOpaqueString(t *testing.T) {
	specField, ok := reflect.TypeOf(RunSpec{}).FieldByName("WorkItemRef")
	require.True(t, ok, "RunSpec.WorkItemRef must exist")
	assert.Equal(t, reflect.String, specField.Type.Kind(),
		"RunSpec.WorkItemRef must be a plain string (opaque id, ADR-001), got %s", specField.Type)

	// AC7: no field on Run may embed a work-item/comment/claim/memory struct.
	// Walk Spec and Status one level deep: every embedded struct type must be
	// one of the known Run sub-specs, never a work item.
	allowed := map[reflect.Type]bool{
		reflect.TypeOf(ObjectRef{}):        true,
		reflect.TypeOf(SandboxPolicy{}):    true,
		reflect.TypeOf(RetryPolicy{}):      true,
		reflect.TypeOf(metav1.Condition{}): true,
		reflect.TypeOf(metav1.Time{}):      true,
	}
	walk := func(structType reflect.Type) {
		for i := 0; i < structType.NumField(); i++ {
			ft := structType.Field(i).Type
			for ft.Kind() == reflect.Pointer || ft.Kind() == reflect.Slice || ft.Kind() == reflect.Map {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct && !allowed[ft] {
				t.Errorf("Run embeds unexpected struct %s in %s.%s — work items and other Postgres rows must never be embedded (ADR-001/AC7)",
					ft, structType, structType.Field(i).Name)
			}
		}
	}
	walk(reflect.TypeOf(RunSpec{}))
	walk(reflect.TypeOf(RunStatus{}))

	// ArtifactRefs are refs (ObjectRef), never embedded artifacts (AC7).
	arField, ok := reflect.TypeOf(RunStatus{}).FieldByName("ArtifactRefs")
	require.True(t, ok)
	assert.Equal(t, reflect.TypeOf([]ObjectRef{}), arField.Type,
		"RunStatus.ArtifactRefs must be []ObjectRef — refs to artifact rows, not embedded artifacts (§6.1, AC7)")
}

// crdEnumFor loads a generated CRD manifest and returns the OpenAPI enum
// carried by the given property path (e.g. ["status", "phase"]).
func crdEnumFor(t *testing.T, crdFile string, path ...string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", crdFile))
	require.NoError(t, err, "generated CRD %s must exist (run `make manifests`)", crdFile)

	var doc map[string]interface{}
	require.NoError(t, yaml.Unmarshal(raw, &doc))

	versions, ok := doc["spec"].(map[string]interface{})["versions"].([]interface{})
	require.True(t, ok, "CRD %s has no versions", crdFile)
	schema := versions[0].(map[string]interface{})["schema"].(map[string]interface{})["openAPIV3Schema"].(map[string]interface{})
	node := schema["properties"].(map[string]interface{})
	for _, seg := range path[:len(path)-1] {
		node = node[seg].(map[string]interface{})["properties"].(map[string]interface{})
	}
	leaf, ok := node[path[len(path)-1]].(map[string]interface{})
	require.True(t, ok, "property %v not found in %s", path, crdFile)
	rawEnum, ok := leaf["enum"].([]interface{})
	require.True(t, ok, "property %v in %s carries no enum — fail-closed validation is missing", path, crdFile)
	out := make([]string, 0, len(rawEnum))
	for _, v := range rawEnum {
		out = append(out, v.(string))
	}
	return out
}

// TestRunPhaseEnumFailsClosed asserts the generated Run CRD validates
// status.phase against exactly the §8 enum: an out-of-set phase is rejected
// at admission (story 1.2 AC8b).
func TestRunPhaseEnumFailsClosed(t *testing.T) {
	got := crdEnumFor(t, "ksquad.io_runs.yaml", "status", "phase")
	want := []string{
		string(RunPhasePending), string(RunPhaseClaiming), string(RunPhaseRunning),
		string(RunPhasePaused), string(RunPhaseSucceeded), string(RunPhaseFailed),
		string(RunPhaseCancelled),
	}
	assert.ElementsMatch(t, want, got, "CRD phase enum must be exactly the §8 set")
	assert.NotContains(t, got, "Bogus", "an out-of-set phase must not be admissible")
}

// TestSkillSourceEnumFailsClosed asserts the generated Skill CRD validates
// source.type against inline|git: a Skill with an unknown source type is
// rejected at admission (story 1.2 AC8).
func TestSkillSourceEnumFailsClosed(t *testing.T) {
	got := crdEnumFor(t, "ksquad.io_skills.yaml", "spec", "source", "type")
	assert.ElementsMatch(t, []string{"inline", "git"}, got,
		"CRD source.type enum must be exactly inline|git (§5.3.6)")
	assert.NotContains(t, got, "http", "unknown source types must not be admissible")
}

// TestAgentModelEndpointRefRoundTrips proves the Epic 5.7 Gate-2 field is
// really on the type and survives JSON + deepcopy (story 1.2 AC3/AC8c).
func TestAgentModelEndpointRefRoundTrips(t *testing.T) {
	a := sampleAgent()
	require.NotNil(t, a.Spec.ModelEndpointRef, "Agent.spec.modelEndpointRef must be settable (Gate-2 field, §10.3/ADR-026)")

	raw, err := json.Marshal(a)
	require.NoError(t, err)
	var back Agent
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Equal(t, *a.Spec.ModelEndpointRef, *back.Spec.ModelEndpointRef,
		"modelEndpointRef must marshal/unmarshal cleanly")
	assert.Equal(t, a.Spec.FallbackModel, back.Spec.FallbackModel)

	cp := a.DeepCopy()
	assert.Equal(t, a.Spec.ModelEndpointRef, cp.Spec.ModelEndpointRef,
		"modelEndpointRef must survive DeepCopy")

	// Optional per AC3: an Agent on a paid provider omits all three optional fields.
	bare := &Agent{Spec: AgentSpec{
		RuntimeRef:          ObjectRef{Name: "openclaw"},
		RoleRef:             ObjectRef{Name: "coder"},
		CredentialSecretRef: SecretRef{Name: "amelia-claude"},
		Model:               "claude-sonnet-4",
	}}
	rawBare, err := json.Marshal(bare)
	require.NoError(t, err)
	assert.NotContains(t, string(rawBare), "modelEndpointRef", "modelEndpointRef must be omitempty (optional at runtime, AC3)")
}

// TestSchemeRegistersAllSixKinds proves all six types register into the
// namespaced ksquad.io/v1alpha1 group (story 1.2 AC1).
func TestSchemeRegistersAllSixKinds(t *testing.T) {
	s := runtime.NewScheme()
	require.NoError(t, AddToScheme(s))
	for _, kind := range []string{"Team", "Agent", "Role", "Skill", "Project", "Run"} {
		assert.True(t, s.Recognizes(GroupVersion.WithKind(kind)),
			"scheme must recognize ksquad.io/v1alpha1 kind %s", kind)
	}
}
