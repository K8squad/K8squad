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

package roleprompt

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, api.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func coordinatorRole(promptRefName string) *api.Role {
	return &api.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "role-manager", Namespace: "sq"},
		Spec: api.RoleSpec{
			PromptRef:   api.ObjectRef{Name: promptRefName},
			Coordinator: true,
		},
	}
}

func icRole(promptRefName string) *api.Role {
	return &api.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "role-implementer", Namespace: "sq"},
		Spec: api.RoleSpec{
			PromptRef: api.ObjectRef{Name: promptRefName},
		},
	}
}

// A nil role injects no prompt.
func TestResolveNilRole(t *testing.T) {
	got, err := Resolve(context.Background(), testClient(t), nil, "sq")
	require.NoError(t, err)
	assert.Equal(t, "", got)
}

// A coordinator role with NO backing prompt ConfigMap falls back to the
// built-in default — behavior on by default (the primary MVP guarantee).
func TestResolveCoordinatorDefaultWhenConfigMapAbsent(t *testing.T) {
	role := coordinatorRole("role-manager-prompt")
	got, err := Resolve(context.Background(), testClient(t), role, "sq")
	require.NoError(t, err)
	assert.Equal(t, DefaultCoordinatorPrompt, got)
	assert.Contains(t, got, "work_item_create", "default prompt names the authoring tools")
	assert.Contains(t, got, "decompose", "default prompt directs decomposition")
}

// A non-coordinator role with no ConfigMap injects nothing — deny-by-default
// posture preserved for ICs.
func TestResolveNonCoordinatorNoConfigMapIsEmpty(t *testing.T) {
	role := icRole("role-implementer-prompt")
	got, err := Resolve(context.Background(), testClient(t), role, "sq")
	require.NoError(t, err)
	assert.Equal(t, "", got)
}

// A present prompt ConfigMap wins over the built-in default (operator override).
func TestResolveConfigMapOverridesDefault(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "role-manager-prompt", Namespace: "sq"},
		Data:       map[string]string{"prompt.md": "custom coordinator behavior"},
	}
	role := coordinatorRole("role-manager-prompt")
	got, err := Resolve(context.Background(), testClient(t, cm), role, "sq")
	require.NoError(t, err)
	assert.Equal(t, "custom coordinator behavior", got)
}

// An IC role WITH a prompt ConfigMap gets that prompt (the general role-prompt
// feature is not coordinator-only).
func TestResolveConfigMapForNonCoordinator(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "role-implementer-prompt", Namespace: "sq"},
		Data:       map[string]string{"prompt": "focus on clean, tested implementations"},
	}
	role := icRole("role-implementer-prompt")
	got, err := Resolve(context.Background(), testClient(t, cm), role, "sq")
	require.NoError(t, err)
	assert.Equal(t, "focus on clean, tested implementations", got)
}

// A single-key ConfigMap is accepted regardless of key name.
func TestResolveConfigMapSingleKeyFallback(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "role-implementer-prompt", Namespace: "sq"},
		Data:       map[string]string{"anything.txt": "sole-key prompt"},
	}
	role := icRole("role-implementer-prompt")
	got, err := Resolve(context.Background(), testClient(t, cm), role, "sq")
	require.NoError(t, err)
	assert.Equal(t, "sole-key prompt", got)
}

// A present-but-empty ConfigMap for a coordinator falls through to the default.
func TestResolveEmptyConfigMapCoordinatorFallsToDefault(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "role-manager-prompt", Namespace: "sq"},
		Data:       map[string]string{},
	}
	role := coordinatorRole("role-manager-prompt")
	got, err := Resolve(context.Background(), testClient(t, cm), role, "sq")
	require.NoError(t, err)
	assert.Equal(t, DefaultCoordinatorPrompt, got)
}

// The PromptRef namespace, when set, overrides the default namespace.
func TestResolveConfigMapExplicitNamespace(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-prompt", Namespace: "prompts"},
		Data:       map[string]string{"prompt.md": "shared behavior"},
	}
	role := &api.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "role-manager", Namespace: "sq"},
		Spec: api.RoleSpec{
			PromptRef:   api.ObjectRef{Name: "shared-prompt", Namespace: "prompts"},
			Coordinator: true,
		},
	}
	got, err := Resolve(context.Background(), testClient(t, cm), role, "sq")
	require.NoError(t, err)
	assert.Equal(t, "shared behavior", got)
}

// The embedded default is non-empty and references the delegation contract.
func TestDefaultCoordinatorPromptEmbedded(t *testing.T) {
	assert.NotEmpty(t, DefaultCoordinatorPrompt)
	assert.Contains(t, DefaultCoordinatorPrompt, "work_item_assign")
	assert.Contains(t, DefaultCoordinatorPrompt, "parent_id")
}
