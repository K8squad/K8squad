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

package toolchain

import (
	"context"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, api.AddToScheme(s))
	return s
}

func catalogToolchain(name string, rbac *api.ToolchainRBAC, versions ...api.ToolchainVersion) *api.Toolchain {
	if versions == nil {
		versions = []api.ToolchainVersion{{Version: "1.0", Image: "ghcr.io/k8squad/toolchains/" + name + ":1.0"}}
	}
	return &api.Toolchain{
		ObjectMeta: metav1.ObjectMeta{Namespace: DefaultClusterCatalogNamespace, Name: name},
		Spec:       api.ToolchainSpec{Versions: versions, RBAC: rbac},
	}
}

func readOnlyRBAC() *api.ToolchainRBAC {
	return &api.ToolchainRBAC{
		Scope: api.ToolchainRBACScopeNamespace,
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"pods", "services"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"get", "list", "watch", "patch"}},
		},
	}
}

func TestParseRef(t *testing.T) {
	name, version, err := ParseRef("kubectl@1.31")
	require.NoError(t, err)
	assert.Equal(t, "kubectl", name)
	assert.Equal(t, "1.31", version)

	for _, bad := range []string{"kubectl", "kubectl@", "@1.31", "kubectl@1.31@dev", ""} {
		_, _, err := ParseRef(bad)
		assert.Error(t, err, "ref %q must fail closed", bad)
	}
}

func TestResolveRefsCatalogHitAndMiss(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(catalogToolchain("kubectl", readOnlyRBAC())).Build()
	r := &Resolver{Reader: c, Platform: DefaultPlatformConfig()}

	res, err := r.ResolveRefs(context.Background(), "bmad-squad", []string{"kubectl@1.0"}, "")
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.Equal(t, "kubectl", res[0].Name)
	assert.Equal(t, "1.0", res[0].Version)
	assert.Equal(t, "ghcr.io/k8squad/toolchains/kubectl:1.0", res[0].Image)
	assert.Equal(t, DefaultClusterCatalogNamespace, res[0].SourceNamespace)
	require.NotNil(t, res[0].RBAC)
	assert.Equal(t, readOnlyRBAC().Rules, res[0].RBAC.Rules)

	var unknown *UnknownError
	_, err = r.ResolveRefs(context.Background(), "bmad-squad", []string{"gh@2.62"}, "")
	require.ErrorAs(t, err, &unknown)
	assert.Contains(t, unknown.Error(), "no Toolchain")

	var version *VersionError
	_, err = r.ResolveRefs(context.Background(), "bmad-squad", []string{"kubectl@9.9"}, "")
	require.ErrorAs(t, err, &version)
	assert.Contains(t, version.Error(), "version not offered")
}

func TestResolveRefsVersionConflictFailsClosed(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(catalogToolchain("kubectl", nil,
			api.ToolchainVersion{Version: "1.30", Image: "img:1.30"},
			api.ToolchainVersion{Version: "1.31", Image: "img:1.31"})).
		Build()
	r := &Resolver{Reader: c, Platform: DefaultPlatformConfig()}

	var conflict *ConflictError
	_, err := r.ResolveRefs(context.Background(), "ns", []string{"kubectl@1.30", "kubectl@1.31"}, "skills a, b")
	require.ErrorAs(t, err, &conflict)
	assert.Contains(t, err.Error(), `toolchain "kubectl" version conflict: 1.30 vs 1.31`)

	// The same pin twice is one pin, not a conflict.
	res, err := r.ResolveRefs(context.Background(), "ns", []string{"kubectl@1.30", "kubectl@1.30"}, "")
	require.NoError(t, err)
	assert.Len(t, res, 1)
}

func TestResolveRefsOverrideNarrows(t *testing.T) {
	override := catalogToolchain("kubectl", &api.ToolchainRBAC{
		Scope: api.ToolchainRBACScopeNamespace,
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}},
		},
	})
	override.Namespace = "bmad-squad"

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(catalogToolchain("kubectl", readOnlyRBAC()), override).Build()
	r := &Resolver{Reader: c, Platform: DefaultPlatformConfig()}

	res, err := r.ResolveRefs(context.Background(), "bmad-squad", []string{"kubectl@1.0"}, "")
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.Equal(t, "bmad-squad", res[0].SourceNamespace, "the override wins in its namespace")
	require.NotNil(t, res[0].RBAC)
	assert.Len(t, res[0].RBAC.Rules, 1, "effective rbac is the narrowed override set")
}

func TestResolveRefsOverrideWithoutCatalogFailsClosed(t *testing.T) {
	loner := catalogToolchain("gh", nil)
	loner.Namespace = "bmad-squad"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(loner).Build()
	r := &Resolver{Reader: c, Platform: DefaultPlatformConfig()}

	var trust *TrustError
	_, err := r.ResolveRefs(context.Background(), "bmad-squad", []string{"gh@1.0"}, "")
	require.ErrorAs(t, err, &trust)
	assert.Contains(t, err.Error(), "no cluster-catalog Toolchain")
}

func TestResolveRefsOverrideWideningFailsClosed(t *testing.T) {
	// The override grants a resource the catalog never granted — a
	// webhook-bypassed widening the resolver must still refuse.
	widening := catalogToolchain("kubectl", &api.ToolchainRBAC{
		Scope: api.ToolchainRBACScopeNamespace,
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}},
		},
	})
	widening.Namespace = "bmad-squad"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(catalogToolchain("kubectl", readOnlyRBAC()), widening).Build()
	r := &Resolver{Reader: c, Platform: DefaultPlatformConfig()}

	var trust *TrustError
	_, err := r.ResolveRefs(context.Background(), "bmad-squad", []string{"kubectl@1.0"}, "")
	require.ErrorAs(t, err, &trust)
	assert.Contains(t, err.Error(), "no cluster-catalog rule covers")
}

func TestRuleCoveredBySemantics(t *testing.T) {
	base := []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"pods", "services"}, Verbs: []string{"get", "list", "watch"}},
	}
	narrow := rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}}
	assert.True(t, RuleCoveredBy(narrow, base), "subset verbs+resources is covered")

	wider := rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "delete"}}
	assert.False(t, RuleCoveredBy(wider, base), "a verb outside the base is a widening")

	newResource := rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}}
	assert.False(t, RuleCoveredBy(newResource, base), "a resource outside the base is a widening")

	unbounded := rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{}}
	assert.False(t, RuleCoveredBy(unbounded, base), "empty verbs means all verbs — not a subset")

	named := rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}, ResourceNames: []string{"special"}}
	assert.False(t, RuleCoveredBy(named, base), "empty resourceNames means all names — not a subset of a named base")
	baseNamed := []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}, ResourceNames: []string{"special", "other"}}}
	assert.True(t, RuleCoveredBy(named, baseNamed), "a named subset is covered")
}

func TestUnionRulesDedupesAndKeepsOrder(t *testing.T) {
	a := []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}}}
	b := []rbacv1.PolicyRule{
		{Verbs: []string{"get"}, Resources: []string{"pods"}, APIGroups: []string{""}}, // same rule, different list order
		{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"patch"}},
	}
	union := UnionRules(a, b, a)
	require.Len(t, union, 2, "exact duplicates dedupe; distinct rules are additive")
}

func TestPlatformConfigFromEnv(t *testing.T) {
	t.Setenv(EnvClusterCatalogNamespace, "custom-catalog")
	t.Setenv(EnvAllowClusterScope, "true")
	cfg := PlatformConfigFromEnv()
	assert.Equal(t, "custom-catalog", cfg.ClusterCatalogNamespace)
	assert.True(t, cfg.AllowClusterScope)

	t.Setenv(EnvAllowClusterScope, "yes please")
	assert.False(t, PlatformConfigFromEnv().AllowClusterScope, "stray values must not trip the opt-in")

	zero := PlatformConfig{}.WithDefaults()
	assert.Equal(t, DefaultClusterCatalogNamespace, zero.ClusterCatalogNamespace, "zero value defaults")
	assert.False(t, zero.AllowClusterScope)
}
