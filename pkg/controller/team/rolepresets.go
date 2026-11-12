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
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// rolePresetFS embeds the preset manifests the reconciler seeds into every
// Team's home namespace (ADR-0033 D1, ISI-7365 closing ISI-7353): the four
// preset role.ksquad.io CRs (role-boss, role-implementer, role-manager,
// role-architect) and the role-manager-prompt ConfigMap their promptRefs
// resolve against. The files under rolepresets/ are a byte-identical mirror
// of config/roles/ — the documented source — because go:embed cannot reach
// above the package directory; TestRolePresetsMatchConfigRoles fails the
// build if the two copies drift. The operator is thereby the single
// authority for the preset set, and `kubectl apply -k config/roles/ -n
// <team-namespace>` stops being a manual human step the getting-started docs
// never mentioned.
//
//go:embed rolepresets/*.yaml
var rolePresetFS embed.FS

// RolePresetFieldManager is the server-side-apply field manager the preset
// seed applies under. ADR-0033 idempotency decision: RE-ASSERT (SSA) — every
// reconcile re-applies the manifests, so a corrupted or hand-mangled preset
// self-heals on the next pass, and a prior manual kubectl apply of the same
// fields is force-taken-over (client.ForceOwnership) rather than quarrelled
// with. Operators who want tuned behavior must bring their own Role CR, not
// edit the seeded presets: the seed wins.
const RolePresetFieldManager = "ksquad-operator/team-rolepresets"

// RolePresetNames are the seeded preset Role CR names. The onboarding
// milestone (internal/apiserver onboardingPresetRoles) checks the trio the
// starter squad materializes; role-architect rides along as the fourth
// preset config/roles ships (ADR-0033 D1).
var RolePresetNames = []string{"role-architect", "role-boss", "role-implementer", "role-manager"}

// RolePresetConfigMapNames are the seeded preset ConfigMaps (the manager's
// orchestration prompt — role-manager.yaml's spec.promptRef target, the
// editable override of pkg/roleprompt's built-in default).
var RolePresetConfigMapNames = []string{"role-manager-prompt"}

// rolePresetObjects parses the embedded manifests into typed objects stamped
// for teamObj. Namespace: the Team's OWN namespace — where Agents live and
// roleRefs resolve (ObjectRefs are namespace-local; the repro-verified manual
// fix applied config/roles there too), NOT the derived ksquad-team-*
// execution namespace provision() scaffolds. Labels: managedLabels merged
// over the manifest's own preset labels — the ksquad.io/team +
// ksquad.io/team-namespace back-reference the self-healing Watches key on.
// OwnerReferences: deliberately NONE. A namespace can host several Teams and
// the presets are per-namespace SHARED state; an ownerRef to any one Team
// would garbage-collect the presets from under the other Teams on its delete
// (ADR-0033 constraint — same discipline as the scaffold's label
// back-reference, one level shallower).
func rolePresetObjects(teamObj *api.Team) ([]client.Object, error) {
	entries, err := fs.ReadDir(rolePresetFS, "rolepresets")
	if err != nil {
		return nil, fmt.Errorf("read embedded role presets: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".yaml") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	objs := make([]client.Object, 0, len(names))
	for _, name := range names {
		raw, err := rolePresetFS.ReadFile("rolepresets/" + name)
		if err != nil {
			return nil, fmt.Errorf("read embedded role preset %s: %w", name, err)
		}
		obj, err := decodeRolePreset(raw)
		if err != nil {
			return nil, fmt.Errorf("decode embedded role preset %s: %w", name, err)
		}
		stampRolePreset(obj, teamObj)
		objs = append(objs, obj)
	}
	return objs, nil
}

// decodeRolePreset unmarshals one embedded manifest into its typed object.
// The manifests are single-document YAML (a leading comment block, then the
// object under a --- separator), so sigs.k8s.io/yaml handles them directly.
func decodeRolePreset(raw []byte) (client.Object, error) {
	var probe struct {
		Kind string `json:"kind"`
	}
	if err := yaml.Unmarshal(raw, &probe); err != nil {
		return nil, err
	}
	switch probe.Kind {
	case "Role":
		var role api.Role
		if err := yaml.Unmarshal(raw, &role); err != nil {
			return nil, err
		}
		return &role, nil
	case "ConfigMap":
		var cm corev1.ConfigMap
		if err := yaml.Unmarshal(raw, &cm); err != nil {
			return nil, err
		}
		return &cm, nil
	default:
		return nil, fmt.Errorf("unsupported preset kind %q (want Role or ConfigMap)", probe.Kind)
	}
}

// stampRolePreset targets obj at the Team's home namespace and merges the
// managed labels over the manifest's own preset labels (ksquad.io/role-preset
// is kept — it identifies the preset family for humans and kubectl).
func stampRolePreset(obj client.Object, teamObj *api.Team) {
	obj.SetNamespace(teamObj.Namespace)
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	for k, v := range managedLabels(teamObj) {
		labels[k] = v
	}
	obj.SetLabels(labels)
}

// ensureRolePresets seeds the preset Role CRs and the manager-prompt ConfigMap
// into the Team's own namespace with server-side apply (re-assert — see
// RolePresetFieldManager). Called from Reconcile once the squad-namespace
// scaffold is up; the onboarding milestone (ADR-0033 D2) then observes the
// CRs and turns honest. A Team homed in a reserved control-plane namespace
// (ksquad-system, kube-*, default) is a misconfiguration: nothing is seeded
// there and seeded=false reports the skip, rather than adding preset noise
// to the control plane.
func (r *Reconciler) ensureRolePresets(ctx context.Context, teamObj *api.Team) (seeded bool, err error) {
	if IsReservedNamespace(teamObj.Namespace) {
		return false, nil
	}
	objs, err := rolePresetObjects(teamObj)
	if err != nil {
		return false, err
	}
	for _, obj := range objs {
		if err := r.Patch(ctx, obj, client.Apply, client.ForceOwnership, client.FieldOwner(RolePresetFieldManager)); err != nil {
			return false, fmt.Errorf("apply preset %T %s/%s: %w", obj, obj.GetNamespace(), obj.GetName(), err)
		}
	}
	return true, nil
}
