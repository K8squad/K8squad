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

package webhook

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Story A2 (ISI-3285): Skill.spec.mcpToolRefs must resolve to existing
// MCPServers — valid refs admit; dangling refs reject with the exact
// actionable format; explicit cross-namespace refs honor their namespace.

func mcpServerObject(ns, name string) client.Object {
	return &ksquadv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       ksquadv1alpha1.MCPServerSpec{Transport: ksquadv1alpha1.MCPTransportStreamableHTTP, Endpoint: "https://mcp.example/" + name},
	}
}

func skillWithRefs(refs ...ksquadv1alpha1.ObjectRef) *ksquadv1alpha1.Skill {
	return &ksquadv1alpha1.Skill{
		ObjectMeta: metav1.ObjectMeta{Namespace: "squad-a", Name: "pr-handler"},
		Spec: ksquadv1alpha1.SkillSpec{
			Source:      ksquadv1alpha1.SkillSource{Type: ksquadv1alpha1.SkillSourceInline, Inline: "# do the thing"},
			McpToolRefs: refs,
		},
	}
}

func TestValidateSkillMCPRefs(t *testing.T) {
	ctx := context.Background()

	t.Run("valid same-namespace ref admits", func(t *testing.T) {
		v := newValidator(t, []client.Object{mcpServerObject("squad-a", "github-mcp")})
		skill := skillWithRefs(ksquadv1alpha1.ObjectRef{Name: "github-mcp"})
		errs := v.ValidateSkill(ctx, skill)
		assert.Empty(t, errs)
	})

	t.Run("dangling ref rejects with the exact actionable message", func(t *testing.T) {
		v := newValidator(t, nil)
		skill := skillWithRefs(ksquadv1alpha1.ObjectRef{Name: "github-mcp"})
		errs := v.ValidateSkill(ctx, skill)
		require.Len(t, errs, 1)
		assert.Contains(t, errs[0].Error(),
			"skill squad-a/pr-handler references missing MCPServer squad-a/github-mcp")
		assert.Contains(t, errs[0].Error(), "spec.mcpToolRefs[0]")
	})

	t.Run("explicit cross-namespace ref honored", func(t *testing.T) {
		v := newValidator(t, []client.Object{mcpServerObject("shared-mcp", "github-mcp")})

		ok := skillWithRefs(ksquadv1alpha1.ObjectRef{Name: "github-mcp", Namespace: "shared-mcp"})
		assert.Empty(t, v.ValidateSkill(ctx, ok))

		missing := skillWithRefs(ksquadv1alpha1.ObjectRef{Name: "github-mcp", Namespace: "nowhere"})
		errs := v.ValidateSkill(ctx, missing)
		require.Len(t, errs, 1)
		assert.Contains(t, errs[0].Error(),
			"references missing MCPServer nowhere/github-mcp")
	})

	t.Run("reader failure fails closed", func(t *testing.T) {
		v := newValidator(t, nil)
		v.Reader = failingReader{}
		errs := v.ValidateSkill(ctx, skillWithRefs(ksquadv1alpha1.ObjectRef{Name: "github-mcp"}))
		require.Len(t, errs, 1)
		assert.Contains(t, errs[0].Error(), "admission read failed (fail-closed)")
	})
}

func TestSkillCustomValidatorSurface(t *testing.T) {
	ctx := context.Background()
	v := &SkillCustomValidator{Validator: newValidator(t, nil)}

	w, err := v.ValidateCreate(ctx, skillWithRefs(ksquadv1alpha1.ObjectRef{Name: "missing"}))
	assert.Nil(t, w)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Invalid value")

	w, err = v.ValidateDelete(ctx, skillWithRefs())
	require.NoError(t, err)
	assert.Nil(t, w)
}

// failingReader always errors: simulates apiserver read trouble so the
// fail-closed branch is provably not dead code.
type failingReader struct {
	fakeClientReader
}

func (failingReader) Get(_ context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return assert.AnError
}

// fakeClientReader is the zero-embed guard for the fake.Reader interface
// methods ValidateSkill does not reach.
type fakeClientReader interface {
	List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error
}
