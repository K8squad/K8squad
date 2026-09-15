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

package modelendpoint

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/K8squad/K8squad/api/v1alpha1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sysNS is the operator namespace the ModelConfig singleton lives in; distinct
// from ns (the workload namespace) so the default-tier read path is exercised
// against the right namespace.
const sysNS = "k8squad-system"

// modelConfigDefault builds the well-known "default" singleton in sysNS.
func modelConfigDefault(mutate func(*api.ModelConfigSpec)) *api.ModelConfig {
	mc := &api.ModelConfig{
		ObjectMeta: metav1.ObjectMeta{Name: DefaultModelConfigName, Namespace: sysNS},
		Spec:       api.ModelConfigSpec{Model: "default-model"},
	}
	if mutate != nil {
		mutate(&mc.Spec)
	}
	return mc
}

// roleWith builds a Role in ns with the given model/fallback.
func roleWith(model string, fb *api.FallbackModel) *api.Role {
	return &api.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: ns},
		Spec:       api.RoleSpec{Model: model, FallbackModel: fb},
	}
}

// agentModel builds an Agent whose spec.model is set to model ("" = fall
// through) with no BYO endpoint or fallback.
func agentModel(model string) *api.Agent {
	return byoAgent(func(s *api.AgentSpec) {
		s.Model = model
		s.ModelEndpointRef = nil
		s.FallbackModel = nil
	})
}

func withSysNS(r *Resolver) *Resolver { r.SystemNamespace = sysNS; return r }

// newResolverWith builds a resolver seeded with mc when non-nil (a typed-nil
// *ModelConfig would reach the fake client as a non-nil client.Object).
func newResolverWith(t *testing.T, mc *api.ModelConfig) *Resolver {
	t.Helper()
	if mc == nil {
		return newResolver(t)
	}
	return newResolver(t, mc)
}

// TestResolveEffectiveTierSelection is the D4 tier-as-a-unit table: agent-only,
// role-only, default-only, and the override precedences. It asserts BOTH the
// effective primary model and the winning tier returned to callers.
func TestResolveEffectiveTierSelection(t *testing.T) {
	tests := []struct {
		name      string
		agent     *api.Agent
		role      *api.Role
		mc        *api.ModelConfig
		wantModel string
		wantTier  Tier
	}{
		{
			name:      "agent-only",
			agent:     agentModel("agent-model"),
			role:      nil,
			mc:        nil,
			wantModel: "agent-model",
			wantTier:  TierAgent,
		},
		{
			name:      "role-only (agent falls through)",
			agent:     agentModel(""),
			role:      roleWith("role-model", nil),
			mc:        nil,
			wantModel: "role-model",
			wantTier:  TierRole,
		},
		{
			name:      "default-only (agent+role fall through)",
			agent:     agentModel(""),
			role:      roleWith("", nil),
			mc:        modelConfigDefault(nil),
			wantModel: "default-model",
			wantTier:  TierDefault,
		},
		{
			name:      "agent over role",
			agent:     agentModel("agent-model"),
			role:      roleWith("role-model", nil),
			mc:        modelConfigDefault(nil),
			wantModel: "agent-model",
			wantTier:  TierAgent,
		},
		{
			name:      "role over default",
			agent:     agentModel(""),
			role:      roleWith("role-model", nil),
			mc:        modelConfigDefault(nil),
			wantModel: "role-model",
			wantTier:  TierRole,
		},
		{
			name:      "nil role, agent set — no ModelConfig read needed",
			agent:     agentModel("agent-model"),
			role:      nil,
			mc:        nil,
			wantModel: "agent-model",
			wantTier:  TierAgent,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := withSysNS(newResolverWith(t, tc.mc))
			primary, _, tier, _, err := r.ResolveEffective(context.Background(), tc.agent, tc.role)
			require.NoError(t, err)
			assert.Equal(t, tc.wantModel, primary.Model)
			assert.Equal(t, tc.wantTier, tier)
		})
	}
}

// TestResolveEffectiveFailClosed: no tier supplies a model — not even the
// default (absent, or present but empty is impossible via schema so we test
// absent). ResolveEffective returns ErrNoModel and a zero primary, NEVER an
// empty-model Endpoint (ISI-4430 D3).
func TestResolveEffectiveFailClosed(t *testing.T) {
	r := withSysNS(newResolver(t)) // no ModelConfig in the store
	primary, _, _, _, err := r.ResolveEffective(context.Background(), agentModel(""), roleWith("", nil))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoModel)
	assert.Empty(t, primary.Model, "fail-closed must not return an empty-model endpoint")

	// nil agent AND nil role with no default is also fail-closed.
	_, _, _, _, err = r.ResolveEffective(context.Background(), agentModel(""), nil)
	assert.ErrorIs(t, err, ErrNoModel)
}

// TestResolveEffectiveFallbackPerTier: the fallback comes from the WINNING
// tier as a unit (D4) — a lower tier's fallback is never grafted onto a higher
// tier's primary.
func TestResolveEffectiveFallbackPerTier(t *testing.T) {
	t.Run("agent tier: agent fallback wins, role/default fallbacks ignored", func(t *testing.T) {
		agent := byoAgent(func(s *api.AgentSpec) {
			s.Model = "agent-model"
			s.ModelEndpointRef = nil
			s.FallbackModel = &api.FallbackModel{Model: "agent-fb"}
		})
		role := roleWith("role-model", &api.FallbackModel{Model: "role-fb"})
		r := withSysNS(newResolverWith(t, modelConfigDefault(func(s *api.ModelConfigSpec) {
			s.FallbackModel = &api.FallbackModel{Model: "default-fb"}
		})))
		primary, fb, tier, ok, err := r.ResolveEffective(context.Background(), agent, role)
		require.NoError(t, err)
		assert.Equal(t, TierAgent, tier)
		assert.Equal(t, "agent-model", primary.Model)
		require.True(t, ok)
		assert.Equal(t, "agent-fb", fb.Model)
	})

	t.Run("role tier: role fallback wins, default fallback ignored", func(t *testing.T) {
		role := roleWith("role-model", &api.FallbackModel{Model: "role-fb"})
		r := withSysNS(newResolverWith(t, modelConfigDefault(func(s *api.ModelConfigSpec) {
			s.FallbackModel = &api.FallbackModel{Model: "default-fb"}
		})))
		primary, fb, tier, ok, err := r.ResolveEffective(context.Background(), agentModel(""), role)
		require.NoError(t, err)
		assert.Equal(t, TierRole, tier)
		assert.Equal(t, "role-model", primary.Model)
		require.True(t, ok)
		assert.Equal(t, "role-fb", fb.Model)
		assert.Empty(t, primary.BaseURL, "role tier uses provider-default endpoint (O1 deferred)")
	})

	t.Run("default tier: default fallback used", func(t *testing.T) {
		r := withSysNS(newResolverWith(t, modelConfigDefault(func(s *api.ModelConfigSpec) {
			s.FallbackModel = &api.FallbackModel{Model: "default-fb"}
		})))
		_, fb, tier, ok, err := r.ResolveEffective(context.Background(), agentModel(""), roleWith("", nil))
		require.NoError(t, err)
		assert.Equal(t, TierDefault, tier)
		require.True(t, ok)
		assert.Equal(t, "default-fb", fb.Model)
	})

	t.Run("winning tier has no fallback: ok=false even if a lower tier does", func(t *testing.T) {
		// Agent wins with a model but NO fallback; role/default have fallbacks
		// that must be ignored.
		role := roleWith("role-model", &api.FallbackModel{Model: "role-fb"})
		r := withSysNS(newResolverWith(t, modelConfigDefault(func(s *api.ModelConfigSpec) {
			s.FallbackModel = &api.FallbackModel{Model: "default-fb"}
		})))
		_, fb, tier, ok, err := r.ResolveEffective(context.Background(), agentModel("agent-model"), role)
		require.NoError(t, err)
		assert.Equal(t, TierAgent, tier)
		assert.False(t, ok, "agent tier has no fallback; lower tiers must not leak in")
		assert.Empty(t, fb.Model)
	})
}

// TestResolveEffectiveEndpointPerTier: the credential/endpoint follows the
// winning tier (D5). Agent tier resolves its BYO Secret; default tier resolves
// the ModelConfig's modelEndpointRef Secret (in the operator namespace).
func TestResolveEffectiveEndpointPerTier(t *testing.T) {
	t.Run("agent tier BYO endpoint", func(t *testing.T) {
		agent := byoAgent(func(s *api.AgentSpec) {
			s.Model = "agent-model"
			s.ModelEndpointRef = &api.SecretRef{Name: "agent-ep"}
			s.FallbackModel = nil
		})
		r := withSysNS(newResolver(t, endpointSecret("agent-ep", map[string][]byte{
			KeyEndpointURL: []byte("http://ollama.svc:11434"),
			KeyAPIToken:    []byte("tok"),
		})))
		primary, _, tier, _, err := r.ResolveEffective(context.Background(), agent, nil)
		require.NoError(t, err)
		assert.Equal(t, TierAgent, tier)
		assert.Equal(t, "http://ollama.svc:11434", primary.BaseURL)
		assert.Equal(t, "tok", primary.Token)
		assert.Equal(t, "agent-ep", primary.SecretName)
	})

	t.Run("default tier ModelConfig endpoint (operator namespace)", func(t *testing.T) {
		mc := modelConfigDefault(func(s *api.ModelConfigSpec) {
			s.ModelEndpointRef = &api.SecretRef{Name: "default-ep"}
		})
		epSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "default-ep", Namespace: sysNS},
			Data:       map[string][]byte{KeyEndpointURL: []byte("http://gw.svc:8080")},
		}
		r := withSysNS(newResolver(t, mc, epSecret))
		primary, _, tier, _, err := r.ResolveEffective(context.Background(), agentModel(""), roleWith("", nil))
		require.NoError(t, err)
		assert.Equal(t, TierDefault, tier)
		assert.Equal(t, "http://gw.svc:8080", primary.BaseURL)
		assert.Equal(t, "default-ep", primary.SecretName)
	})

	t.Run("dangling endpoint Secret fails closed (not ErrNoModel)", func(t *testing.T) {
		agent := byoAgent(func(s *api.AgentSpec) {
			s.Model = "agent-model"
			s.ModelEndpointRef = &api.SecretRef{Name: "missing-secret"}
			s.FallbackModel = nil
		})
		r := withSysNS(newResolver(t))
		_, _, _, _, err := r.ResolveEffective(context.Background(), agent, nil)
		require.Error(t, err)
		assert.False(t, errors.Is(err, ErrNoModel), "a dangling Secret is a resolution error, not the no-model sentinel")
		var unresolved *ErrUnresolved
		assert.ErrorAs(t, err, &unresolved)
	})
}

// TestDefaultModelConfigReadErrorPropagates: a non-NotFound read error from the
// ModelConfig Get propagates for retry, distinct from the fail-closed no-model
// path. Modeled with a Reader that errors on Get.
func TestResolveEffectiveDefaultNotFoundIsFailClosed(t *testing.T) {
	// Default absent + higher tiers empty → ErrNoModel (NotFound is swallowed
	// into found=false, then fail-closed).
	r := withSysNS(newResolver(t))
	_, _, _, _, err := r.ResolveEffective(context.Background(), agentModel(""), roleWith("", nil))
	assert.ErrorIs(t, err, ErrNoModel)
}
