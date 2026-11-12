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

package team

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// presetTeam is a Team homed in a NON-reserved namespace. newTeam
// (naming_test.go) pins ns "default", which the preset seed skips by policy
// (a reserved-namespace Team is a control-plane misconfiguration, ADR-0033
// D1) — the onboarding lane homes Teams in e.g. k8squad-onboarding.
func presetTeam(name, uid, ns string) *api.Team {
	return &api.Team{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(uid)},
	}
}

func reconcileTeamIn(t *testing.T, r *Reconciler, ns, name string) error {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: ns},
	})
	return err
}

// TestRolePresetsMatchConfigRoles guards the embed mirror: config/roles/ is
// the documented preset source, rolepresets/ is what the operator ships.
// They must stay byte-identical or the operator silently seeds a stale preset
// set while humans apply the fresh one (ADR-0033 D1 single-source coupling).
func TestRolePresetsMatchConfigRoles(t *testing.T) {
	for _, name := range append(append([]string{}, RolePresetNames...), "role-manager-prompt") {
		file := name + ".yaml"
		embedded, err := rolePresetFS.ReadFile("rolepresets/" + file)
		if err != nil {
			t.Fatalf("embedded preset %s missing: %v", file, err)
		}
		source, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "roles", file))
		if err != nil {
			t.Fatalf("config/roles/%s unreadable: %v", file, err)
		}
		if string(embedded) != string(source) {
			t.Errorf("preset %s drifted: pkg/controller/team/rolepresets/%s must stay byte-identical to config/roles/%s (ADR-0033 D1)", file, file, file)
		}
	}
}

// TestRolePresetsSeededIntoTeamNamespace (ADR-0033 D1, ISI-7365): one
// reconcile seeds the four preset Role CRs + the manager-prompt ConfigMap
// into the Team's OWN namespace — where Agents live and roleRefs resolve —
// NOT the derived ksquad-team-* scaffold namespace, and reports
// RolePresetsReady=True. Objects carry the managed-label back-reference and
// deliberately no ownerReferences (per-namespace shared state).
func TestRolePresetsSeededIntoTeamNamespace(t *testing.T) {
	const ns = "k8squad-onboarding"
	r, c := newReconciler(t, presetTeam("starter", "uid-starter", ns))
	if err := reconcileTeamIn(t, r, ns, "starter"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	ctx := context.Background()
	for _, name := range RolePresetNames {
		var role api.Role
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &role); err != nil {
			t.Fatalf("preset Role %s: %v", name, err)
		}
		if role.Labels[LabelTeam] != "starter" || role.Labels[LabelTeamNamespace] != ns {
			t.Errorf("preset Role %s labels = %v, want the team back-reference", name, role.Labels)
		}
		if len(role.OwnerReferences) != 0 {
			t.Errorf("preset Role %s carries ownerReferences %v — presets are per-namespace shared state, an ownerRef would GC them from under sibling Teams (ADR-0033)", name, role.OwnerReferences)
		}
		if role.Spec.PromptRef.Name == "" {
			t.Errorf("preset Role %s lost spec.promptRef (embed/decode bug)", name)
		}
	}
	var prompt corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "role-manager-prompt"}, &prompt); err != nil {
		t.Fatalf("manager-prompt ConfigMap: %v", err)
	}
	if prompt.Data["prompt.md"] == "" {
		t.Errorf("manager-prompt ConfigMap lost prompt.md (embed/decode bug)")
	}

	var team api.Team
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "starter"}, &team); err != nil {
		t.Fatalf("get team: %v", err)
	}
	// Nothing seeded into the derived scaffold namespace — wrong target.
	var stray api.Role
	if err := c.Get(ctx, types.NamespacedName{Namespace: team.Status.Namespace, Name: "role-boss"}, &stray); err == nil {
		t.Errorf("preset Role found in the scaffold namespace %s — D1 seeds the Team's own namespace", team.Status.Namespace)
	}
	ready := false
	for _, cond := range team.Status.Conditions {
		if cond.Type == condRolePresetsReady {
			ready = true
			if cond.Status != metav1.ConditionTrue {
				t.Errorf("RolePresetsReady = %s, want True (%s)", cond.Status, cond.Message)
			}
		}
	}
	if !ready {
		t.Errorf("RolePresetsReady condition missing from %+v", team.Status.Conditions)
	}
}

// TestRolePresetsSelfHeal (ADR-0033 re-assert choice): a hand-corrupted preset
// payload is re-asserted and a deleted preset object re-created on the next
// reconcile — the corrupted-preset self-heal SSA was chosen for.
func TestRolePresetsSelfHeal(t *testing.T) {
	const ns = "k8squad-onboarding"
	r, c := newReconciler(t, presetTeam("starter", "uid-starter", ns))
	if err := reconcileTeamIn(t, r, ns, "starter"); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	ctx := context.Background()
	var boss api.Role
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "role-boss"}, &boss); err != nil {
		t.Fatalf("get role-boss: %v", err)
	}
	if len(boss.Spec.DefaultSkills) == 0 {
		t.Fatalf("role-boss seeded without defaultSkills — embed/decode bug")
	}
	boss.Spec.DefaultSkills = nil
	if err := c.Update(ctx, &boss); err != nil {
		t.Fatalf("corrupt role-boss: %v", err)
	}
	var prompt corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "role-manager-prompt"}, &prompt); err != nil {
		t.Fatalf("get manager-prompt: %v", err)
	}
	if err := c.Delete(ctx, &prompt); err != nil {
		t.Fatalf("delete manager-prompt: %v", err)
	}

	if err := reconcileTeamIn(t, r, ns, "starter"); err != nil {
		t.Fatalf("heal reconcile: %v", err)
	}
	var healed api.Role
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "role-boss"}, &healed); err != nil {
		t.Fatalf("get healed role-boss: %v", err)
	}
	if len(healed.Spec.DefaultSkills) == 0 {
		t.Errorf("corrupted role-boss payload not re-asserted (SSA re-assert, ADR-0033)")
	}
	var healedPrompt corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "role-manager-prompt"}, &healedPrompt); err != nil {
		t.Fatalf("deleted manager-prompt ConfigMap not re-created: %v", err)
	}
}

// TestRolePresetsSkippedInReservedNamespace: a Team homed in a reserved
// control-plane namespace (newTeam pins "default") gets NO presets — the
// control plane is not a squad home, and seeding it there is only noise.
func TestRolePresetsSkippedInReservedNamespace(t *testing.T) {
	r, c := newReconciler(t, newTeam("ctrl", "uid-ctrl"))
	if err := reconcileTeam(t, r, "ctrl"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var role api.Role
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "role-boss"}, &role); err == nil {
		t.Errorf("preset seeded into reserved namespace default — must be skipped by policy")
	}
	var team api.Team
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "ctrl"}, &team); err != nil {
		t.Fatalf("get team: %v", err)
	}
	for _, cond := range team.Status.Conditions {
		if cond.Type == condRolePresetsReady && cond.Reason != "RolePresetsSkipped" {
			t.Errorf("RolePresetsReady reason = %s, want RolePresetsSkipped", cond.Reason)
		}
	}
}

// TestRolePresetObjectsStampLabels exercises the pure manifest→object path:
// namespace targeting, label merge (preset label preserved, managed labels
// added), and the preset-set completeness (4 Roles + 1 ConfigMap).
func TestRolePresetObjectsStampLabels(t *testing.T) {
	objs, err := rolePresetObjects(presetTeam("starter", "uid-starter", "k8squad-onboarding"))
	if err != nil {
		t.Fatalf("rolePresetObjects: %v", err)
	}
	if len(objs) != len(RolePresetNames)+len(RolePresetConfigMapNames) {
		t.Fatalf("got %d preset objects, want %d", len(objs), len(RolePresetNames)+len(RolePresetConfigMapNames))
	}
	roles, cms := 0, 0
	for _, obj := range objs {
		switch o := obj.(type) {
		case *api.Role:
			roles++
			if o.Namespace != "k8squad-onboarding" {
				t.Errorf("Role %s namespace = %s", o.Name, o.Namespace)
			}
			if o.Labels["ksquad.io/role-preset"] == "" {
				t.Errorf("Role %s lost its ksquad.io/role-preset label", o.Name)
			}
			if o.Labels[LabelTeam] != "starter" {
				t.Errorf("Role %s missing managed label %s", o.Name, LabelTeam)
			}
		case *corev1.ConfigMap:
			cms++
			if o.Namespace != "k8squad-onboarding" {
				t.Errorf("ConfigMap %s namespace = %s", o.Name, o.Namespace)
			}
		}
	}
	if roles != len(RolePresetNames) || cms != len(RolePresetConfigMapNames) {
		t.Fatalf("got %d Roles + %d ConfigMaps, want %d + %d", roles, cms, len(RolePresetNames), len(RolePresetConfigMapNames))
	}
	_ = client.Object(nil) // keep the client import honest for future asserts
}
