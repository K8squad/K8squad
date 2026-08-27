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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// SetupSkillWebhookWithManager registers the Skill validating webhook
// (story A2 / ISI-3285): spec.mcpToolRefs must resolve to existing
// MCPServer objects — fail-closed against dangling refs, with
// failurePolicy=fail.
func SetupSkillWebhookWithManager(mgr ctrl.Manager) error {
	v := &SkillCustomValidator{Validator: &CrossRefValidator{Reader: mgr.GetClient()}}
	return ctrl.NewWebhookManagedBy(mgr, &ksquadv1alpha1.Skill{}).
		WithValidator(v).
		Complete()
}

// SkillCustomValidator validates Skill admission (story A2: dangling
// mcpToolRefs fail closed; ADR-042 webhook contract).
type SkillCustomValidator struct{ Validator *CrossRefValidator }

var _ admission.Validator[*ksquadv1alpha1.Skill] = &SkillCustomValidator{}

// ValidateCreate implements admission.Validator.
func (v *SkillCustomValidator) ValidateCreate(ctx context.Context, skill *ksquadv1alpha1.Skill) (admission.Warnings, error) {
	if skill == nil {
		return nil, apierrors.NewBadRequest("expected a Skill but got nil")
	}
	return nil, toInvalid("Skill", skill.Name, v.Validator.ValidateSkill(ctx, skill))
}

// ValidateUpdate implements admission.Validator.
func (v *SkillCustomValidator) ValidateUpdate(ctx context.Context, _, newObj *ksquadv1alpha1.Skill) (admission.Warnings, error) {
	return v.ValidateCreate(ctx, newObj)
}

// ValidateDelete implements admission.Validator (Skills delete freely;
// Runs re-check their MCP refs at assembly).
func (v *SkillCustomValidator) ValidateDelete(_ context.Context, _ *ksquadv1alpha1.Skill) (admission.Warnings, error) {
	return nil, nil
}
