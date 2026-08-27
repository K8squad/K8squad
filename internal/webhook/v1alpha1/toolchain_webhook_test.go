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

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/toolchain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const catalogNS = toolchain.DefaultClusterCatalogNamespace

func catalogKubectl(t *testing.T) *ksquadv1alpha1.Toolchain {
	t.Helper()
	return &ksquadv1alpha1.Toolchain{
		ObjectMeta: metav1.ObjectMeta{Name: "kubectl", Namespace: catalogNS},
		Spec: ksquadv1alpha1.ToolchainSpec{
			Versions: []ksquadv1alpha1.ToolchainVersion{
				{Version: "1.31", Image: "ghcr.io/k8squad/toolchains/kubectl:1.31"},
			},
			RBAC: &ksquadv1alpha1.ToolchainRBAC{
				Scope: ksquadv1alpha1.ToolchainRBACScopeNamespace,
				Rules: []rbacv1.PolicyRule{
					{APIGroups: []string{""}, Resources: []string{"pods", "services"}, Verbs: []string{"get", "list", "watch"}},
				},
			},
		},
	}
}

func teamKubectlOverride(t *testing.T) *ksquadv1alpha1.Toolchain {
	t.Helper()
	tc := catalogKubectl(t)
	tc.Namespace = ns
	tc.Spec.RBAC = &ksquadv1alpha1.ToolchainRBAC{
		Scope: ksquadv1alpha1.ToolchainRBACScopeNamespace,
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}},
		},
	}
	return tc
}

// TestValidateToolchainCatalogBaseline: a well-formed catalog entry and a
// genuine narrow-only override both admit.
func TestValidateToolchainCatalogBaseline(t *testing.T) {
	ctx := context.Background()
	v := newValidator(t, []client.Object{catalogKubectl(t), teamKubectlOverride(t)})

	assert.Empty(t, v.ValidateToolchain(ctx, catalogKubectl(t)), "catalog entry with least-privilege rbac admits")
	assert.Empty(t, v.ValidateToolchain(ctx, teamKubectlOverride(t)), "subset-rules override admits")
}

// TestValidateToolchainShapeRejects: wildcards, hollow rules, missing
// apiGroups, duplicate versions and versionless images are denied with the
// field path + fix.
func TestValidateToolchainShapeRejects(t *testing.T) {
	ctx := context.Background()
	v := newValidator(t, nil)

	wildcard := catalogKubectl(t)
	wildcard.Spec.RBAC.Rules = []rbacv1.PolicyRule{{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}}}
	errs := v.ValidateToolchain(ctx, wildcard)
	require.NotEmpty(t, errs)
	assert.Contains(t, errs.ToAggregate().Error(), "wildcards are rejected")

	hollow := catalogKubectl(t)
	hollow.Spec.RBAC.Rules = []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}}}
	errs = v.ValidateToolchain(ctx, hollow)
	require.NotEmpty(t, errs)
	assert.Contains(t, errs.ToAggregate().Error(), "at least one verb")

	noGroup := catalogKubectl(t)
	noGroup.Spec.RBAC.Rules = []rbacv1.PolicyRule{{Resources: []string{"pods"}, Verbs: []string{"get"}}}
	errs = v.ValidateToolchain(ctx, noGroup)
	require.NotEmpty(t, errs)
	assert.Contains(t, errs.ToAggregate().Error(), "apiGroups must be set")

	dupVersion := catalogKubectl(t)
	dupVersion.Spec.Versions = append(dupVersion.Spec.Versions, ksquadv1alpha1.ToolchainVersion{Version: "1.31", Image: "other:1.31"})
	errs = v.ValidateToolchain(ctx, dupVersion)
	require.NotEmpty(t, errs)
	assert.Contains(t, errs.ToAggregate().Error(), "duplicate version")

	noImage := catalogKubectl(t)
	noImage.Spec.Versions = []ksquadv1alpha1.ToolchainVersion{{Version: "1.32"}}
	errs = v.ValidateToolchain(ctx, noImage)
	require.NotEmpty(t, errs)
	assert.Contains(t, errs.ToAggregate().Error(), "must pin an image")
}

// TestValidateToolchainClusterScopeGated: rbac.scope=cluster is denied
// without the platform opt-in and admitted with it — the explicit
// Helm-value gate (plan §2.2b).
func TestValidateToolchainClusterScopeGated(t *testing.T) {
	ctx := context.Background()
	cluster := catalogKubectl(t)
	cluster.Spec.RBAC.Scope = ksquadv1alpha1.ToolchainRBACScopeCluster

	v := newValidator(t, nil)
	errs := v.ValidateToolchain(ctx, cluster)
	require.NotEmpty(t, errs, "cluster scope without opt-in must fail closed")
	assert.Contains(t, errs.ToAggregate().Error(), "clusterScopeEnabled")

	optedIn := newValidator(t, nil)
	optedIn.Toolchains.AllowClusterScope = true
	assert.Empty(t, optedIn.ValidateToolchain(ctx, cluster), "cluster scope with the platform opt-in admits")
}

// TestValidateToolchainNarrowOnlyBoundary: a team-namespace Toolchain
// without a catalog counterpart is rejected; widening rules, minted
// versions and substituted images are rejected; cluster scope from a team
// namespace is rejected even with the platform opt-in.
func TestValidateToolchainNarrowOnlyBoundary(t *testing.T) {
	ctx := context.Background()

	loner := catalogKubectl(t)
	loner.Namespace = ns
	v := newValidator(t, nil)
	errs := v.ValidateToolchain(ctx, loner)
	require.NotEmpty(t, errs)
	assert.Contains(t, errs.ToAggregate().Error(), "may only override an existing cluster-catalog entry")

	seeded := newValidator(t, []client.Object{catalogKubectl(t)})

	widening := teamKubectlOverride(t)
	widening.Spec.RBAC.Rules = []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}},
	}
	errs = seeded.ValidateToolchain(ctx, widening)
	require.NotEmpty(t, errs)
	assert.Contains(t, errs.ToAggregate().Error(), "overrides may only narrow")

	minted := teamKubectlOverride(t)
	minted.Spec.RBAC = nil
	minted.Spec.Versions = append(minted.Spec.Versions, ksquadv1alpha1.ToolchainVersion{Version: "1.99", Image: "ghcr.io/evil/kubectl:1.99"})
	errs = seeded.ValidateToolchain(ctx, minted)
	require.NotEmpty(t, errs)
	assert.Contains(t, errs.ToAggregate().Error(), "cluster catalog does not")

	substituted := teamKubectlOverride(t)
	substituted.Spec.RBAC = nil
	substituted.Spec.Versions[0].Image = "ghcr.io/evil/kubectl:1.31"
	errs = seeded.ValidateToolchain(ctx, substituted)
	require.NotEmpty(t, errs)
	assert.Contains(t, errs.ToAggregate().Error(), "image substitution is not narrowing")

	clusterScopeOverride := teamKubectlOverride(t)
	clusterScopeOverride.Spec.RBAC.Scope = ksquadv1alpha1.ToolchainRBACScopeCluster
	optedIn := newValidator(t, []client.Object{catalogKubectl(t)})
	optedIn.Toolchains.AllowClusterScope = true
	errs = optedIn.ValidateToolchain(ctx, clusterScopeOverride)
	require.NotEmpty(t, errs, "cluster scope is never renderable from a team namespace")
	assert.Contains(t, errs.ToAggregate().Error(), "never renderable from a team namespace")
}

// TestValidateToolchainGuardFalsification: with GuardToolchainCatalog
// disabled, every invalid specimen above flips to admit — the guard is the
// only thing denying them (no dead code, no double-cover).
func TestValidateToolchainGuardFalsification(t *testing.T) {
	ctx := context.Background()
	bad := []*ksquadv1alpha1.Toolchain{
		func() *ksquadv1alpha1.Toolchain {
			tc := catalogKubectl(t)
			tc.Spec.RBAC.Rules = []rbacv1.PolicyRule{{Verbs: []string{"*"}, APIGroups: []string{"*"}, Resources: []string{"*"}}}
			return tc
		}(),
		func() *ksquadv1alpha1.Toolchain {
			tc := catalogKubectl(t)
			tc.Namespace = ns
			return tc
		}(),
	}
	v := newValidator(t, []client.Object{catalogKubectl(t)})
	v.DisabledGuards = map[string]bool{GuardToolchainCatalog: true}
	for _, tc := range bad {
		assert.Empty(t, v.ValidateToolchain(ctx, tc), "guard removed but %s/%s still rejects", tc.Namespace, tc.Name)
	}
}

// --- Run admission: story B2, the fail-closed name@version gate ---

// runRequiringKubectl builds a Run whose agent's skill requires
// kubectl@<version> — the chain the B2 guard resolves — plus the agent
// and skill objects to seed the world with.
func runRequiringKubectl(version string) (*ksquadv1alpha1.Run, *ksquadv1alpha1.Agent, *ksquadv1alpha1.Skill) {
	run := validRun()
	skill := &ksquadv1alpha1.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "restart-deploy", Namespace: ns},
		Spec:       ksquadv1alpha1.SkillSpec{Requires: ksquadv1alpha1.SkillRequires{Toolchains: []string{"kubectl@" + version}}},
	}
	agent := &ksquadv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "amelia-tools", Namespace: ns},
		Spec: ksquadv1alpha1.AgentSpec{
			SkillRefs: []ksquadv1alpha1.ObjectRef{{Name: skill.Name}},
		},
	}
	run.Spec.Agents = []ksquadv1alpha1.ObjectRef{{Name: agent.Name}}
	return run, agent, skill
}

// TestValidateRunToolchainsAdmitsAndRejects: a resolvable ref admits;
// unknown name, unknown version and version conflicts fail closed with
// actionable messages; the guard is load-bearing (falsification).
func TestValidateRunToolchainsAdmitsAndRejects(t *testing.T) {
	ctx := context.Background()
	world := validWorld()
	world = append(world, catalogKubectl(t))

	// Admit: the skill's kubectl@1.31 resolves in the catalog.
	run, agent, skill := runRequiringKubectl("1.31")
	v := newValidator(t, append(world, agent, skill))
	assert.Empty(t, v.ValidateRun(ctx, run))

	// Unknown name: actionable message naming the fix.
	run, agent, skill = runRequiringKubectl("1.31")
	skill.Spec.Requires.Toolchains = []string{"gh@2.62"}
	v = newValidator(t, append(world, agent, skill))
	errs := v.ValidateRun(ctx, run)
	require.NotEmpty(t, errs)
	assert.Contains(t, errs.ToAggregate().Error(), "no Toolchain")
	assert.Contains(t, errs.ToAggregate().Error(), "tools.defaultCatalog.enabled")

	// Unknown version: names what is available.
	run, agent, skill = runRequiringKubectl("9.9")
	v = newValidator(t, append(world, agent, skill))
	errs = v.ValidateRun(ctx, run)
	require.NotEmpty(t, errs)
	assert.Contains(t, errs.ToAggregate().Error(), "version not offered")

	// Version conflict across the Run's skills: fail closed.
	run, agent, skill = runRequiringKubectl("1.31")
	conflicting := &ksquadv1alpha1.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "logs-tail", Namespace: ns},
		Spec:       ksquadv1alpha1.SkillSpec{Requires: ksquadv1alpha1.SkillRequires{Toolchains: []string{"kubectl@1.30"}}},
	}
	agent.Spec.SkillRefs = append(agent.Spec.SkillRefs, ksquadv1alpha1.ObjectRef{Name: conflicting.Name})
	twoVersions := catalogKubectl(t)
	twoVersions.Spec.Versions = append(twoVersions.Spec.Versions,
		ksquadv1alpha1.ToolchainVersion{Version: "1.30", Image: "ghcr.io/k8squad/toolchains/kubectl:1.30"})
	conflictWorld := append(validWorld(), twoVersions)
	v = newValidator(t, append(conflictWorld, agent, skill, conflicting))
	errs = v.ValidateRun(ctx, run)
	require.NotEmpty(t, errs)
	assert.Contains(t, errs.ToAggregate().Error(), "version conflict")

	// Falsification: guard off → the unknown-name case admits.
	run, agent, skill = runRequiringKubectl("1.31")
	skill.Spec.Requires.Toolchains = []string{"gh@2.62"}
	v = newValidator(t, append(world, agent, skill))
	v.DisabledGuards = map[string]bool{GuardRunToolchains: true}
	assert.Empty(t, v.ValidateRun(ctx, run), "guard removed but the case still rejects")
}
