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

package teamroster

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	api "github.com/K8squad/K8squad/api/v1alpha1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, api.AddToScheme(s))
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func agent(name, ns, role string) *api.Agent {
	return &api.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       api.AgentSpec{RoleRef: api.ObjectRef{Name: role}},
	}
}

func team(ns string, refs ...api.ObjectRef) *api.Team {
	return &api.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "squad", Namespace: ns},
		Spec:       api.TeamSpec{Agents: refs},
	}
}

// The happy path: every agent's NAME and its role (Agent.Spec.RoleRef.Name)
// appear, and the fact tells the coordinator the assignee must be one of the
// NAMES — the gap ISI-5245 closes.
func TestResolveListsNameAndRole(t *testing.T) {
	c := testClient(t,
		agent("winston", "sq", "architect"),
		agent("ada", "sq", "coder"),
	)
	tm := team("sq", api.ObjectRef{Name: "winston"}, api.ObjectRef{Name: "ada"})

	got, err := Resolve(context.Background(), c, tm, "sq")
	require.NoError(t, err)
	assert.Contains(t, got, "- winston — role: architect")
	assert.Contains(t, got, "- ada — role: coder")
	assert.Contains(t, got, "assignable agents")
	assert.Contains(t, got, "NAMES")
}

// A team that names no agents yields "" — nothing to assign to, unchanged.
func TestResolveEmptyTeam(t *testing.T) {
	got, err := Resolve(context.Background(), testClient(t), team("sq"), "sq")
	require.NoError(t, err)
	assert.Equal(t, "", got)

	got, err = Resolve(context.Background(), testClient(t), nil, "sq")
	require.NoError(t, err)
	assert.Equal(t, "", got)
}

// A dangling ref (agent named on the team but its CR absent) is NOT an error:
// the NAME still lists (it is the assign target) — just without a resolved role.
func TestResolveDanglingRefListsNameOnly(t *testing.T) {
	c := testClient(t, agent("winston", "sq", "architect"))
	tm := team("sq", api.ObjectRef{Name: "winston"}, api.ObjectRef{Name: "ghost"})

	got, err := Resolve(context.Background(), c, tm, "sq")
	require.NoError(t, err)
	assert.Contains(t, got, "- winston — role: architect")
	assert.Contains(t, got, "- ghost")
	assert.NotContains(t, got, "ghost — role")
}

// The ref's own namespace wins over the default when set.
func TestResolveHonorsRefNamespace(t *testing.T) {
	c := testClient(t, agent("uma", "design-ns", "ux-designer"))
	tm := team("sq", api.ObjectRef{Name: "uma", Namespace: "design-ns"})

	got, err := Resolve(context.Background(), c, tm, "sq")
	require.NoError(t, err)
	assert.Contains(t, got, "- uma — role: ux-designer")
}

// A genuine (non-NotFound) read error fails closed: the caller requeues rather
// than dispatching a coordinator a partial roster.
func TestResolveFailsClosedOnReadError(t *testing.T) {
	boom := errors.New("apiserver unavailable")
	s := runtime.NewScheme()
	require.NoError(t, api.AddToScheme(s))
	base := fake.NewClientBuilder().WithScheme(s).WithObjects(agent("winston", "sq", "architect")).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*api.Agent); ok {
				return boom
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	})
	tm := team("sq", api.ObjectRef{Name: "winston"})

	_, err := Resolve(context.Background(), c, tm, "sq")
	require.Error(t, err)
	assert.ErrorIs(t, err, boom)
}
