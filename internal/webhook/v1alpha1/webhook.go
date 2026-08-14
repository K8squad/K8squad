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

// Package webhook wires the story 1.3 cross-object validating admission
// webhooks: Team, Agent and Run reference checks that CEL (self-only)
// cannot express. Same-object rules (FR-D3 runtime discipline, Skill
// source one-of, sandbox defaults) live as CEL/structural defaults on the
// CRD schemas themselves — see api/v1alpha1.
package webhook

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// +kubebuilder:webhook:path=/validate-ksquad-io-v1alpha1-team,mutating=false,failurePolicy=fail,sideEffects=None,groups=ksquad.io,resources=teams,versions=v1alpha1,verbs=create;update,admissionReviewVersions=v1,name=vteam.kb.io

// TeamCustomValidator validates Team admission (story 1.3).
type TeamCustomValidator struct{ Validator *CrossRefValidator }

var _ admission.CustomValidator = &TeamCustomValidator{}

// ValidateCreate implements admission.CustomValidator.
func (v *TeamCustomValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	team, ok := obj.(*ksquadv1alpha1.Team)
	if !ok {
		return nil, apierrors.NewBadRequest("expected a Team but got " + obj.GetObjectKind().GroupVersionKind().String())
	}
	return nil, toInvalid("Team", team.Name, v.Validator.ValidateTeam(ctx, team))
}

// ValidateUpdate implements admission.CustomValidator.
func (v *TeamCustomValidator) ValidateUpdate(ctx context.Context, _, newObj runtime.Object) (admission.Warnings, error) {
	return v.ValidateCreate(ctx, newObj)
}

// ValidateDelete implements admission.CustomValidator (Teams delete freely).
func (v *TeamCustomValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// +kubebuilder:webhook:path=/validate-ksquad-io-v1alpha1-agent,mutating=false,failurePolicy=fail,sideEffects=None,groups=ksquad.io,resources=agents,versions=v1alpha1,verbs=create;update,admissionReviewVersions=v1,name=vagent.kb.io

// AgentCustomValidator validates Agent admission (story 1.3).
type AgentCustomValidator struct{ Validator *CrossRefValidator }

var _ admission.CustomValidator = &AgentCustomValidator{}

// ValidateCreate implements admission.CustomValidator.
func (v *AgentCustomValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	agent, ok := obj.(*ksquadv1alpha1.Agent)
	if !ok {
		return nil, apierrors.NewBadRequest("expected an Agent but got " + obj.GetObjectKind().GroupVersionKind().String())
	}
	return nil, toInvalid("Agent", agent.Name, v.Validator.ValidateAgent(ctx, agent))
}

// ValidateUpdate implements admission.CustomValidator.
func (v *AgentCustomValidator) ValidateUpdate(ctx context.Context, _, newObj runtime.Object) (admission.Warnings, error) {
	return v.ValidateCreate(ctx, newObj)
}

// ValidateDelete implements admission.CustomValidator (Agents delete freely).
func (v *AgentCustomValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// +kubebuilder:webhook:path=/validate-ksquad-io-v1alpha1-run,mutating=false,failurePolicy=fail,sideEffects=None,groups=ksquad.io,resources=runs,versions=v1alpha1,verbs=create;update,admissionReviewVersions=v1,name=vrun.kb.io

// RunCustomValidator validates Run admission (story 1.3).
type RunCustomValidator struct{ Validator *CrossRefValidator }

var _ admission.CustomValidator = &RunCustomValidator{}

// ValidateCreate implements admission.CustomValidator.
func (v *RunCustomValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	run, ok := obj.(*ksquadv1alpha1.Run)
	if !ok {
		return nil, apierrors.NewBadRequest("expected a Run but got " + obj.GetObjectKind().GroupVersionKind().String())
	}
	return nil, toInvalid("Run", run.Name, v.Validator.ValidateRun(ctx, run))
}

// ValidateUpdate implements admission.CustomValidator.
func (v *RunCustomValidator) ValidateUpdate(ctx context.Context, _, newObj runtime.Object) (admission.Warnings, error) {
	return v.ValidateCreate(ctx, newObj)
}

// ValidateDelete implements admission.CustomValidator (kills are FR-A6, not
// admission's business).
func (v *RunCustomValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// toInvalid aggregates a field.ErrorList into the API error the webhook
// server serializes as the denial — the rendered message carries field
// path + observed value + fix for every violation.
func toInvalid(kind, name string, errs field.ErrorList) error {
	if len(errs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(
		ksquadv1alpha1.GroupVersion.WithKind(kind).GroupKind(), name, errs)
}

// +kubebuilder:rbac:groups=ksquad.io,resources=teams;agents;agentruntimes;roles;skills;projects,verbs=get;list;watch

// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch

// SetupWithManager registers the three validating webhooks on mgr's webhook
// server. Guards run with failurePolicy=fail: a broken webhook denies
// rather than admits (fail-closed, story 1.3).
func SetupWithManager(mgr manager.Manager) error {
	v := &CrossRefValidator{Reader: mgr.GetClient()}
	webhooks := []struct {
		obj       client.Object
		validator admission.CustomValidator
	}{
		{&ksquadv1alpha1.Team{}, &TeamCustomValidator{Validator: v}},
		{&ksquadv1alpha1.Agent{}, &AgentCustomValidator{Validator: v}},
		{&ksquadv1alpha1.Run{}, &RunCustomValidator{Validator: v}},
	}
	for _, w := range webhooks {
		if err := builder.WebhookManagedBy(mgr).
			For(w.obj).
			WithValidator(w.validator).
			Complete(); err != nil {
			return err
		}
	}
	return nil
}
