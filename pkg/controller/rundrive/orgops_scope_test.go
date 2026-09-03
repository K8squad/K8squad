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
	"sort"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/orgops"
)

// roleWithReportsTo builds a Role CR carrying the reports-to hierarchy label.
func roleWithReportsTo(name, ns, reportsTo string) *api.Role {
	labels := map[string]string{}
	if reportsTo != "" {
		labels[orgops.LabelReportsTo] = reportsTo
	}
	return &api.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels}}
}

func agentWithRole(name, ns, roleName string) *api.Agent {
	return &api.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       api.AgentSpec{RoleRef: api.ObjectRef{Name: roleName}},
	}
}

// deriveRunScopes derives the token scope from the run's Agent's Role graph, not
// from the Agent — a manager Role gets org:write, the CEO gets project:write too,
// an IC gets nothing.
func TestDeriveRunScopes(t *testing.T) {
	const ns = "bmad-squad"
	seed := []client.Object{
		roleWithReportsTo("ceo", ns, ""),
		roleWithReportsTo("product-manager", ns, "ceo"),
		roleWithReportsTo("coder", ns, "product-manager"),
		agentWithRole("ceo-agent", ns, "ceo"),
		agentWithRole("pm-agent", ns, "product-manager"),
		agentWithRole("coder-agent", ns, "coder"),
	}
	cl := fake.NewClientBuilder().WithScheme(dispatchScheme(t)).WithObjects(seed...).Build()
	d := &operatorDispatch{cfg: OperatorDispatchConfig{Client: cl}}

	cases := []struct {
		agent string
		want  []string
	}{
		{"ceo-agent", []string{orgops.ScopeOrgWrite, orgops.ScopeProjectWrite}},
		{"pm-agent", []string{orgops.ScopeOrgWrite}},
		{"coder-agent", nil},
	}
	for _, c := range cases {
		run := &api.Run{
			ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: ns},
			Spec:       api.RunSpec{Agents: []api.ObjectRef{{Name: c.agent}}},
		}
		got := d.deriveRunScopes(context.Background(), run)
		if !sameScopes(got, c.want) {
			t.Errorf("deriveRunScopes(%s) = %v, want %v", c.agent, got, c.want)
		}
	}
}

// A run with no dispatch agent, or an unreadable Agent/Role, derives no scope
// (fail-closed to least privilege).
func TestDeriveRunScopesFailClosed(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(dispatchScheme(t)).Build()
	d := &operatorDispatch{cfg: OperatorDispatchConfig{Client: cl}}

	if got := d.deriveRunScopes(context.Background(), &api.Run{Spec: api.RunSpec{}}); got != nil {
		t.Fatalf("no-agent run: got %v, want nil", got)
	}
	run := &api.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns"},
		Spec:       api.RunSpec{Agents: []api.ObjectRef{{Name: "ghost"}}},
	}
	if got := d.deriveRunScopes(context.Background(), run); got != nil {
		t.Fatalf("missing agent: got %v, want nil", got)
	}
}

func sameScopes(a, b []string) bool {
	a = append([]string(nil), a...)
	b = append([]string(nil), b...)
	sort.Strings(a)
	sort.Strings(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
