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

// SetupRoleWebhookWithManager registers the Role validating webhook with the
// manager (phase-lifecycle, ISI-4431 E3 / NFR-3). The webhook mirrors the
// CRD's item/enum rules (defense in depth for clusters where CEL/OpenAPI
// surfaced errors are undesired) and adds the friendly-message checks the
// generated schema states tersely: unknown phase strings in activePhases and
// coordinatorMode set without coordinator=true.
//
// The Role stays data-only and is NOT reconciled (NFR-3): this webhook is the
// only server-side behavior on a Role. The one-coordinator-per-Team invariant
// is enforced at Team admission (a Role is reusable across teams), NOT here.
func SetupRoleWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &Role{}).
		WithValidator(&Role{}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-ksquad-io-v1alpha1-role,mutating=false,failurePolicy=fail,sideEffects=None,groups=ksquad.io,resources=roles,verbs=create;update,versions=v1alpha1,name=vrole-v1alpha1.ksquad.io,admissionReviewVersions=v1

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

	return warnings, nil
}
