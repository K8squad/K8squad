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

package apiserver

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/modelendpoint"
)

// roleWithModel builds a Role in ns whose spec.model is set (empty ⇒ the role
// tier falls through, exercising the default tier). ISI-4892 read-out fixtures.
func roleWithModel(ns, name, uid, model string) *ksquadv1.Role {
	return &ksquadv1.Role{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(uid)},
		Spec:       ksquadv1.RoleSpec{PromptRef: ksquadv1.ObjectRef{Name: "p"}, Model: model},
	}
}

// modelConfigSingleton builds the well-known "default" ModelConfig in the
// operator namespace (k8squad-system) with the given spec.model — the org-default
// tier the resolver falls to when neither agent nor role supplies a model.
func modelConfigSingleton(model string) *ksquadv1.ModelConfig {
	return &ksquadv1.ModelConfig{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: modelendpoint.DefaultSystemNamespace,
			Name:      modelendpoint.DefaultModelConfigName,
		},
		Spec: ksquadv1.ModelConfigSpec{Model: model},
	}
}

// TestEffectiveModelProjectionTiers is the S3 read-out contract: the reader
// projects the SHIPPED resolver's verdict (Agent → Role → org-default) into the
// EffectiveModelView the console renders, mapping the winning Tier to the wire
// "agent"|"role"|"default" the ProvenanceChip keys off. Mirrors
// modelendpoint/resolve_effective_test.go's tier-as-a-unit table, one tier up.
func TestEffectiveModelProjectionTiers(t *testing.T) {
	// One squad, one Team (so detailNamespace resolves), three agents exercising
	// each winning tier, plus the org-default ModelConfig singleton.
	objs := []client.Object{
		teamWithMembers("squad-a", "alpha", fleetUIDA, []string{"override", "role-inherit", "default-inherit"}, nil),
		agentObj("squad-a", "override", "ag-ov", "claude-code", "coder", "agent-model"),
		agentObj("squad-a", "role-inherit", "ag-ri", "claude-code", "coder", ""),
		agentObj("squad-a", "default-inherit", "ag-di", "claude-code", "empty-role", ""),
		roleWithModel("squad-a", "coder", "r-coder", "role-model"),
		roleWithModel("squad-a", "empty-role", "r-empty", ""),
		modelConfigSingleton("org-default-model"),
	}
	r := newFleetReader(t, objs...)
	ctx := context.Background()

	tests := []struct {
		name      string
		agent     string
		wantModel string
		wantTier  string
		wantRole  string
	}{
		{"agent override wins", "override", "agent-model", "agent", "coder"},
		{"role inherited (agent blank)", "role-inherit", "role-model", "role", "coder"},
		{"org default (agent+role blank)", "default-inherit", "org-default-model", "default", "empty-role"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			view, err := r.EffectiveModel(ctx, fleetUIDA, tc.agent, "", false)
			if err != nil {
				t.Fatalf("EffectiveModel(%q): %v", tc.agent, err)
			}
			if view.Unresolved {
				t.Fatalf("EffectiveModel(%q): unexpected unresolved verdict", tc.agent)
			}
			if view.Model != tc.wantModel {
				t.Errorf("model = %q, want %q", view.Model, tc.wantModel)
			}
			if view.Tier != tc.wantTier {
				t.Errorf("tier = %q, want %q", view.Tier, tc.wantTier)
			}
			if view.RoleName != tc.wantRole {
				t.Errorf("roleName = %q, want %q", view.RoleName, tc.wantRole)
			}
		})
	}
}

// TestEffectiveModelFailClosed is the AC4 guardrail: an agent that resolves empty
// at every tier (no agent model, no role model, no org-default ModelConfig) is a
// STRUCTURED verdict — Unresolved=true — not an error the handler turns into 5xx.
// The read route must never invent a fallback model (ISI-4430 D3 fail-closed).
func TestEffectiveModelFailClosed(t *testing.T) {
	objs := []client.Object{
		teamWithMembers("squad-a", "alpha", fleetUIDA, []string{"stranded"}, nil),
		agentObj("squad-a", "stranded", "ag-st", "claude-code", "empty-role", ""),
		roleWithModel("squad-a", "empty-role", "r-empty", ""),
		// no ModelConfig singleton — nothing supplies a model at any tier.
	}
	r := newFleetReader(t, objs...)

	view, err := r.EffectiveModel(context.Background(), fleetUIDA, "stranded", "", false)
	if err != nil {
		t.Fatalf("fail-closed must be a non-error verdict, got err: %v", err)
	}
	if !view.Unresolved {
		t.Fatalf("want Unresolved=true, got %+v", view)
	}
	if view.Model != "" || view.Tier != "" || view.FallbackModel != "" {
		t.Fatalf("unresolved verdict must not carry a model/tier/fallback: %+v", view)
	}
	if view.RoleName != "empty-role" {
		t.Errorf("roleName should still label the ref, got %q", view.RoleName)
	}
}

// agentWithEndpoint builds an agent whose spec.model wins the agent tier AND
// carries a BYO modelEndpointRef — the posture that fail-closed to a 502 before
// ISI-5420 (its endpoint Secret has to be read through a corev1-capable reader,
// not the ksquad-only informer cache).
func agentWithEndpoint(ns, name, uid, model, role, secretName string) *ksquadv1.Agent {
	a := agentObj(ns, name, uid, "claude-code", role, model)
	a.Spec.ModelEndpointRef = &ksquadv1.SecretRef{Name: secretName}
	return a
}

// endpointSecret builds a BYO endpoint Secret in the 7.5 credential shape
// (endpointURL [+ apiToken]) that the resolver's ResolveRef reads.
func endpointSecret(ns, name, url string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Data:       map[string][]byte{modelendpoint.KeyEndpointURL: []byte(url)},
	}
}

// TestEffectiveModelBYOEndpoint is the ISI-5420 regression: an agent whose
// winning tier carries a modelEndpointRef resolves to 200 with its model when
// the fleet reader is handed a DIRECT corev1-capable secretReader — even though
// the primary reader (the production informer cache) carries ONLY the ksquad
// CRDs and would fail a Secret Get with "no kind is registered". The second
// sub-case pins the pre-fix behaviour: with no secretReader (resolver falls back
// to the ksquad-only reader) the Secret Get errors, the handler maps it to 502.
func TestEffectiveModelBYOEndpoint(t *testing.T) {
	ctx := context.Background()
	objs := []client.Object{
		teamWithMembers("bmad-squad", "bmad", fleetUIDA, []string{"john"}, nil),
		agentWithEndpoint("bmad-squad", "john", "ag-john", "deepseek-flash", "coder", "deepseek-endpoint"),
		roleWithModel("bmad-squad", "coder", "r-coder", ""),
	}
	// Primary reader: ksquad-only scheme, exactly like the production cache — it
	// cannot decode corev1.Secret.
	cacheReader := fake.NewClientBuilder().WithScheme(overviewScheme(t)).WithObjects(objs...).Build()

	// Direct secret reader over a corev1 scheme, holding the endpoint Secret in
	// the agent's team namespace.
	secretScheme := runtime.NewScheme()
	if err := corev1.AddToScheme(secretScheme); err != nil {
		t.Fatalf("register corev1 scheme: %v", err)
	}
	secretReader := fake.NewClientBuilder().WithScheme(secretScheme).
		WithObjects(endpointSecret("bmad-squad", "deepseek-endpoint", "https://deepseek.example/v1")).Build()

	t.Run("resolves 200 with dedicated secret reader", func(t *testing.T) {
		r := NewClientFleetListReader(cacheReader, secretReader)
		view, err := r.EffectiveModel(ctx, fleetUIDA, "john", "", false)
		if err != nil {
			t.Fatalf("EffectiveModel: unexpected error (the 502 ISI-5420 fixes): %v", err)
		}
		if view.Unresolved {
			t.Fatalf("EffectiveModel: unexpected unresolved verdict: %+v", view)
		}
		if view.Model != "deepseek-flash" || view.Tier != "agent" {
			t.Errorf("view = {Model:%q Tier:%q}, want {deepseek-flash agent}", view.Model, view.Tier)
		}
	})

	t.Run("fail-closes to an error without a corev1 reader (pre-fix 502)", func(t *testing.T) {
		r := NewClientFleetListReader(cacheReader, nil)
		_, err := r.EffectiveModel(ctx, fleetUIDA, "john", "", false)
		if err == nil {
			t.Fatal("want a non-nil error (handler → 502) when the Secret Get rides the ksquad-only reader")
		}
		if errors.Is(err, modelendpoint.ErrNoModel) {
			t.Fatalf("want a transient read error, not the fail-closed ErrNoModel: %v", err)
		}
	})
}

// TestEffectiveModelScoping mirrors AgentDetail's existence-hiding contract: an
// unknown name, an empty name, and a tenant reaching cross-squad all resolve to
// ErrTeamNotFound (the handler answers 404, never a 403 that confirms existence).
func TestEffectiveModelScoping(t *testing.T) {
	objs := []client.Object{
		teamWithMembers("squad-a", "alpha", fleetUIDA, []string{"cade"}, nil),
		agentObj("squad-a", "cade", "ag-a", "claude-code", "coder", "agent-model"),
		roleWithModel("squad-a", "coder", "r-coder", "role-model"),
	}
	r := newFleetReader(t, objs...)
	ctx := context.Background()

	if _, err := r.EffectiveModel(ctx, fleetUIDA, "ghost", "", false); !errors.Is(err, ErrTeamNotFound) {
		t.Errorf("unknown name: want ErrTeamNotFound, got %v", err)
	}
	if _, err := r.EffectiveModel(ctx, fleetUIDA, "", "", false); !errors.Is(err, ErrTeamNotFound) {
		t.Errorf("empty name: want ErrTeamNotFound, got %v", err)
	}
	if _, err := r.EffectiveModel(ctx, "no-such-uid", "cade", "", false); !errors.Is(err, ErrTeamNotFound) {
		t.Errorf("dangling tenant: want ErrTeamNotFound, got %v", err)
	}
}
