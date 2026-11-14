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
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/controller/team"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// danglingRoleAgent is the ISI-7403 specimen: an Agent hand-authored outside
// the compose fast-path + seeded scaffold whose spec.roleRef names a Role that
// exists nowhere.
func danglingRoleAgent() *ksquadv1alpha1.Agent {
	a := validAgent()
	a.Spec.RoleRef.Name = "ghost-role"
	return a
}

// roleWarns filters the admission warnings down to the dangling-role one
// (the missing-default-ModelConfig soft-warn may ride along in validWorld).
func roleWarns(warns []string) []string {
	var out []string
	for _, w := range warns {
		if strings.Contains(w, "ghost-role") || strings.Contains(w, "roleRef") {
			out = append(out, w)
		}
	}
	return out
}

// TestAgentDanglingRoleRefWarnsNotRejects is the ADR-0033 D3 policy swap
// (ISI-7403): a dangling spec.roleRef must ADMIT with a WARNING that names the
// ref, its namespace and the fix — rejecting it broke apply-order workflows
// where the Role is applied moments after the Agent in the same bundle.
func TestAgentDanglingRoleRefWarnsNotRejects(t *testing.T) {
	ctx := context.Background()
	v := newValidator(t, validWorld())

	warns, errs := v.ValidateAgentWithWarnings(ctx, danglingRoleAgent())
	require.Empty(t, errs, "a dangling roleRef must not reject (apply-order workflows)")
	roleSpecific := roleWarns(warns)
	require.Len(t, roleSpecific, 1, "exactly one dangling-role warning expected")
	assert.Contains(t, roleSpecific[0], "ghost-role")
	assert.Contains(t, roleSpecific[0], ns, "the warning must name the resolved namespace")
	assert.Contains(t, roleSpecific[0], "dispatch", "the warning must say what breaks later")
}

// TestAgentPresentRoleRefAdmitsSilently: a roleRef that resolves emits no
// warning from the role guard at all.
func TestAgentPresentRoleRefAdmitsSilently(t *testing.T) {
	ctx := context.Background()
	v := newValidator(t, validWorld()) // seeds Role "coder"

	warns, errs := v.ValidateAgentWithWarnings(ctx, validAgent())
	require.Empty(t, errs, "the baseline specimen must admit")
	assert.Empty(t, roleWarns(warns), "a resolving roleRef must not warn")
}

// TestAgentPresetRoleRefNeverWarns pins the preset exemption: the operator
// seeds team.RolePresetNames on team-namespace provisioning (ADR-0033 D1,
// PR #1020), so a fresh team namespace can race the seeder legitimately —
// preset refs admit silently even with the Role CR not yet visible.
func TestAgentPresetRoleRefNeverWarns(t *testing.T) {
	ctx := context.Background()
	v := newValidator(t, validWorld()) // no preset Role CRs seeded
	for _, name := range team.RolePresetNames {
		a := validAgent()
		a.Spec.RoleRef.Name = name
		warns, errs := v.ValidateAgentWithWarnings(ctx, a)
		require.Empty(t, errs, "preset %s must admit", name)
		for _, w := range warns {
			assert.NotContains(t, w, name, "preset %s must never dangle-warn", name)
		}
	}
}

// TestAgentRoleRefReadFailsClosed keeps the fail-closed half of GuardAgentRole:
// absence WARNS, but a transient Role read error still DENIES — an API outage
// must never be mistaken for (or hidden behind) the soft-warn policy.
func TestAgentRoleRefReadFailsClosed(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, ksquadv1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	boom := errors.New("apiserver timeout")
	// Fail ONLY Role reads; every other lookup resolves normally.
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(validWorld()...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isRole := obj.(*ksquadv1alpha1.Role); isRole {
					return boom
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
	v := &CrossRefValidator{Reader: c}

	agg := v.ValidateAgent(context.Background(), danglingRoleAgent()).ToAggregate()
	require.Error(t, agg, "a Role read outage must deny")
	assert.Contains(t, agg.Error(), "fail-closed", "the read failure must surface as fail-closed")
}

// TestAgentRoleWarnGuardIsLoadBearing is the falsification self-check for the
// warn half of GuardAgentRole (ISI-7403): disabling exactly this guard must
// remove BOTH the dangling-role warning and its fail-closed read rejection —
// proving the guard, not something else, owns those surfaces.
func TestAgentRoleWarnGuardIsLoadBearing(t *testing.T) {
	ctx := context.Background()
	v := newValidator(t, validWorld())
	v.DisabledGuards = map[string]bool{GuardAgentRole: true}

	assert.Empty(t, v.warnIfRoleRefDangling(ctx, danglingRoleAgent()),
		"guard removed but the dangling role still warns — another path double-covers it")
	assert.Empty(t, v.ValidateAgent(ctx, danglingRoleAgent()),
		"guard removed but a dangling role still rejects")
}

// TestAgentWebhookSurfacesRoleRefWarning pins the wiring: the admission entry
// point (ValidateCreate → ValidateAgentWithWarnings) threads the dangling-role
// warning back to the API server on an otherwise-admitted Agent.
func TestAgentWebhookSurfacesRoleRefWarning(t *testing.T) {
	ctx := context.Background()
	cv := &AgentCustomValidator{Validator: newValidator(t, validWorld())}

	warns, err := cv.ValidateCreate(ctx, danglingRoleAgent())
	require.NoError(t, err, "a dangling roleRef must admit")
	require.NotEmpty(t, roleWarns(warns), "the dangling-role warning must reach the API server")
}

// TestRolePresetNamesRemainTheOperatorContract guards the exemption itself:
// the webhook treats team.RolePresetNames as operator-seeded, so the list must
// stay the non-empty preset set the controller actually seeds (single source
// of truth — drift would silently narrow the never-warn exemption).
func TestRolePresetNamesRemainTheOperatorContract(t *testing.T) {
	require.NotEmpty(t, team.RolePresetNames)
	for _, name := range team.RolePresetNames {
		require.NotEmpty(t, name)
		assert.True(t, strings.HasPrefix(name, "role-"),
			"preset %q must keep the seeded role- prefix", name)
	}
}
