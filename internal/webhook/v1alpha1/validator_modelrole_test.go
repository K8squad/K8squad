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

// bareModelAgent is an Agent with no spec.model and a roleRef to "coder"
// (seeded in validWorld). Its effective model therefore rides the role/default
// tiers — the Model-Per-Role S4 admission surface.
func bareModelAgent() *ksquadv1alpha1.Agent {
	a := validAgent()
	a.Spec.Model = ""
	return a
}

// TestAgentModelFailsClosedWhenNoTierResolves is the S4 D1 acceptance: an Agent
// with no model, a Role with no model, and NO system-default ModelConfig is
// undispatchable and must be REJECTED at admission (not admitted to strand a
// Run mid-flight).
func TestAgentModelFailsClosedWhenNoTierResolves(t *testing.T) {
	ctx := context.Background()
	role := &ksquadv1alpha1.Role{ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: ns}} // no model
	v := newValidator(t, []client.Object{role})

	warns, errs := v.validateAgentModel(ctx, bareModelAgent())
	require.NotEmpty(t, errs, "no model in any tier must fail closed")
	assert.Contains(t, errs.ToAggregate().Error(), "no model resolves")
	assert.Empty(t, warns, "a rejection carries no soft-warn")
}

// TestAgentModelRoleTierAdmitsWithMissingDefaultWarn is the S4 role-tier
// acceptance: an Agent with no model but a Role that supplies one admits — and
// because no system-default ModelConfig exists, the D3 soft-guard surfaces a
// warning (never a rejection).
func TestAgentModelRoleTierAdmitsWithMissingDefaultWarn(t *testing.T) {
	ctx := context.Background()
	role := &ksquadv1alpha1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: ns},
		Spec:       ksquadv1alpha1.RoleSpec{Model: "qwen3:role-model"},
	}
	v := newValidator(t, []client.Object{role})

	warns, errs := v.validateAgentModel(ctx, bareModelAgent())
	require.Empty(t, errs, "a role-supplied model must admit")
	require.Len(t, warns, 1, "a missing system-default ModelConfig must soft-warn")
	assert.Contains(t, warns[0], "system-default ModelConfig")
}

// TestAgentModelDefaultTierAdmitsNoWarn is the S4 default-tier acceptance: an
// Agent and Role both empty resolve to the system-default ModelConfig; with the
// default present there is no soft-warn.
func TestAgentModelDefaultTierAdmitsNoWarn(t *testing.T) {
	ctx := context.Background()
	def := &ksquadv1alpha1.ModelConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "k8squad-system"},
		Spec:       ksquadv1alpha1.ModelConfigSpec{Model: "claude-default"},
	}
	agent := validAgent()
	agent.Spec.Model = ""
	agent.Spec.RoleRef = ksquadv1alpha1.ObjectRef{} // no role → default tier
	v := newValidator(t, []client.Object{def})

	warns, errs := v.validateAgentModel(ctx, agent)
	require.Empty(t, errs, "the system-default tier must admit")
	assert.Empty(t, warns, "a present default emits no soft-warn")
}

// TestAgentModelMissingDefaultWarnsEvenWhenAgentTierResolves is the S4 D3
// soft-guard: a deleted default ModelConfig must surface immediately on ANY
// Agent write — even one whose own spec.model resolves — because other Agents
// leaning on the default tier will fail closed later.
func TestAgentModelMissingDefaultWarnsEvenWhenAgentTierResolves(t *testing.T) {
	ctx := context.Background()
	v := newValidator(t, nil) // no default ModelConfig anywhere

	agent := validAgent() // spec.model = "claude-sonnet-4" → resolves at agent tier
	warns, errs := v.validateAgentModel(ctx, agent)
	require.Empty(t, errs, "an agent-tier model resolves and admits")
	require.Len(t, warns, 1)
	assert.Contains(t, warns[0], "not found")
}

// TestAgentModelGuardIsLoadBearing is the falsification self-check for the new
// guard: disabling GuardAgentModelResolves must flip the no-model case from
// reject to admit — proving the guard, not something else, owns the rejection.
func TestAgentModelGuardIsLoadBearing(t *testing.T) {
	ctx := context.Background()
	role := &ksquadv1alpha1.Role{ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: ns}}
	v := newValidator(t, []client.Object{role})
	v.DisabledGuards = map[string]bool{GuardAgentModelResolves: true}

	_, errs := v.validateAgentModel(ctx, bareModelAgent())
	assert.Empty(t, errs, "guard removed but the no-model case still rejects — another guard double-covers it")
}

// TestAgentWebhookSurfacesModelWarning pins the wiring: the admission entry
// point (ValidateCreate → ValidateAgentWithWarnings) threads the D3 soft-warn
// back to the API server on an otherwise-admitted Agent.
func TestAgentWebhookSurfacesModelWarning(t *testing.T) {
	ctx := context.Background()
	cv := &AgentCustomValidator{Validator: newValidator(t, validWorld())}

	warns, err := cv.ValidateCreate(ctx, validAgent())
	require.NoError(t, err, "a valid agent-tier Agent must admit")
	require.NotEmpty(t, warns, "the missing default ModelConfig warning must reach the API server")
}
