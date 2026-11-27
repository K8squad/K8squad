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

package v1alpha1

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// SetupRoleWebhookWithManager registers the Role validating + mutating
// webhooks with the manager (phase-lifecycle, ISI-4431 E3 / NFR-3; org-chart
// edge, ISI-7946 WS-A). The validator mirrors the CRD's item/enum rules
// (defense in depth for clusters where CEL/OpenAPI surfaced errors are
// undesired) and adds the friendly-message checks the generated schema
// states tersely: unknown phase strings in activePhases, coordinatorMode set
// without coordinator=true, self/cross-namespace reportsTo edges, empty or
// duplicate-stage processes. The defaulter stamps the derived
// ksquad.io/reports-to label from spec.reportsTo (the compat mirror
// pkg/orgops reads).
//
// The Role stays data-only and is NOT reconciled (NFR-3): these webhooks are
// the only server-side behavior on a Role. The one-coordinator-per-Team and
// hierarchy invariants are enforced at Team admission (a Role is reusable
// across teams), NOT here.
func SetupRoleWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &Role{}).
		WithDefaulter(&Role{}).
		WithValidator(&Role{}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-ksquad-io-v1alpha1-role,mutating=true,failurePolicy=fail,sideEffects=None,groups=ksquad.io,resources=roles,verbs=create;update,versions=v1alpha1,name=mrole-orgchart.ksquad.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-ksquad-io-v1alpha1-role,mutating=false,failurePolicy=fail,sideEffects=None,groups=ksquad.io,resources=roles,verbs=create;update,versions=v1alpha1,name=vrole-v1alpha1.ksquad.io,admissionReviewVersions=v1

var _ admission.Defaulter[*Role] = &Role{}
var _ admission.Validator[*Role] = &Role{}

// validRolePhases are the six middle working lifecycle phases a role may be
// active in (ISI-4431 E3). backlog/todo/done/cancelled are intake/terminal
// lanes never "worked" by a role, so they are deliberately excluded — a role
// active in one of those is a config error. Mirrors the CRD items:Enum on
// RoleSpec.ActivePhases (defense in depth + a friendly message).
var validRolePhases = map[string]bool{
	"design":         true,
	"planning":       true,
	"implementation": true,
	"code_review":    true,
	"testing":        true,
	"documentation":  true,
}

// validCoordinatorModes are the allowed coordinatorMode values (ISI-4431 Q4).
// Mirrors the CRD Enum on RoleSpec.CoordinatorMode.
var validCoordinatorModes = map[string]bool{
	"auto":    true,
	"propose": true,
}

// Default implements admission.CustomDefaulter: it derives the
// ksquad.io/reports-to label from spec.reportsTo (ISI-7946 WS-A), keeping
// pkg/orgops scope derivation and kubectl label queries working unchanged.
// When spec.reportsTo is UNSET a pre-existing label is preserved: legacy
// label-only charts keep deriving scopes until they migrate (the validator
// nudges with a warning). When spec.reportsTo IS set it wins over any stale
// label value.
func (r *Role) Default(_ context.Context, obj *Role) error {
	if obj == nil {
		return fmt.Errorf("expected a Role object but got %T", obj)
	}
	if obj.Spec.ReportsTo == nil || obj.Spec.ReportsTo.Name == "" {
		return nil
	}
	labels := obj.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	labels[LabelReportsTo] = obj.Spec.ReportsTo.Name
	obj.Labels = labels
	return nil
}

// ValidateCreate implements admission.Validator.
func (r *Role) ValidateCreate(_ context.Context, obj *Role) (admission.Warnings, error) {
	if obj == nil {
		return nil, fmt.Errorf("expected a Role object but got %T", obj)
	}
	return validateRole(obj)
}

// ValidateUpdate implements admission.Validator.
func (r *Role) ValidateUpdate(_ context.Context, _, newObj *Role) (admission.Warnings, error) {
	if newObj == nil {
		return nil, fmt.Errorf("expected a Role object but got %T", newObj)
	}
	return validateRole(newObj)
}

// ValidateDelete implements admission.Validator.
func (r *Role) ValidateDelete(_ context.Context, _ *Role) (admission.Warnings, error) {
	// Deletion is always allowed; a Role is data-only and not reconciled.
	return nil, nil
}

// validateRole is the single validation entry point shared by create and
// update.
func validateRole(r *Role) (admission.Warnings, error) {
	spec := r.Spec
	var warnings admission.Warnings

	// activePhases: reject unknown phase strings (kubebuilder items:Enum also
	// fails closed; this adds the friendly message and defends clusters where
	// the OpenAPI enum is stripped). Empty activePhases ⇒ phase-agnostic, the
	// back-compat default, and is always valid.
	for _, p := range spec.ActivePhases {
		if !validRolePhases[p] {
			return warnings, fmt.Errorf(
				"spec.activePhases[%q] is not a working lifecycle phase; allowed values are design, planning, implementation, code_review, testing, documentation (backlog/todo/done/cancelled are intake/terminal lanes, never worked by a role)", p)
		}
	}

	// coordinatorMode is meaningful only for a coordinator role. When set on a
	// non-coordinator role we normalize + warn rather than reject: the field is
	// simply inert (ISI-4431 E3 note "or normalize: ignore + warn"). We do NOT
	// mutate the stored object (validating webhook), so the warning tells the
	// author the value has no effect.
	if spec.CoordinatorMode != "" && !spec.Coordinator {
		warnings = append(warnings, fmt.Sprintf(
			"spec.coordinatorMode=%q is ignored because spec.coordinator is false; set coordinator=true or remove coordinatorMode", spec.CoordinatorMode))
	}

	// coordinatorMode value must be one of the enum values when set (mirrors
	// the CRD Enum; friendly message for clusters without OpenAPI enforcement).
	if spec.CoordinatorMode != "" && !validCoordinatorModes[spec.CoordinatorMode] {
		return warnings, fmt.Errorf(
			"spec.coordinatorMode %q is not one of auto, propose", spec.CoordinatorMode)
	}

	// Org-chart edge (ISI-7946 WS-A): reject the self-edge and the
	// cross-namespace shape here — the Team-level graph checks cannot see
	// them. A dangling manager (a Role that does not exist) is a Team
	// admission concern (GuardTeamOrgChart), not a Role one: Roles are
	// reusable and may be applied before their manager.
	if spec.ReportsTo != nil {
		if spec.ReportsTo.Name == "" {
			return warnings, fmt.Errorf("spec.reportsTo.name is required when reportsTo is set")
		}
		if spec.ReportsTo.Name == r.Name {
			return warnings, fmt.Errorf(
				"spec.reportsTo.name %q equals the Role's own name: a role cannot report to itself", r.Name)
		}
		if spec.ReportsTo.Namespace != "" && spec.ReportsTo.Namespace != r.Namespace {
			return warnings, fmt.Errorf(
				"spec.reportsTo.namespace %q must be empty or the Role's own namespace %q: reporting edges are namespace-local", spec.ReportsTo.Namespace, r.Namespace)
		}
	}

	// Manager process (ISI-7946 WS-A): an explicitly empty process is a
	// config error (mirrors the CRD MinItems=1) and duplicate stage names
	// make the ordered pipeline ambiguous (mirrors the listMapKey). Stages
	// naming Roles outside the team and delegation cycles are Team
	// admission concerns (GuardTeamOrgChart).
	if spec.Process != nil {
		if len(spec.Process) == 0 {
			return warnings, fmt.Errorf(
				"spec.process must not be empty: a manager process is an ordered list of stages (remove the field entirely for a role without a process)")
		}
		seen := make(map[string]bool, len(spec.Process))
		for _, st := range spec.Process {
			if st.Stage == "" {
				return warnings, fmt.Errorf("spec.process stage names must be non-empty")
			}
			if st.RoleRef.Name == "" {
				return warnings, fmt.Errorf(
					"spec.process stage %q must name the Role that executes it (roleRef.name)", st.Stage)
			}
			if seen[st.Stage] {
				return warnings, fmt.Errorf(
					"spec.process stage %q is defined twice: stage names must be unique", st.Stage)
			}
			seen[st.Stage] = true
		}
	}

	// Legacy label nudge: a ksquad.io/reports-to label without spec.reportsTo
	// still works (compat — Default preserves it), but it predates the model
	// edge; warn so authors migrate to spec.reportsTo.
	if r.Labels[LabelReportsTo] != "" && (spec.ReportsTo == nil || spec.ReportsTo.Name == "") {
		warnings = append(warnings, fmt.Sprintf(
			"metadata.labels[%s]=%q predates spec.reportsTo; migrate the edge to spec.reportsTo (the label is a derived mirror)", LabelReportsTo, r.Labels[LabelReportsTo]))
	}

	return warnings, nil
}
