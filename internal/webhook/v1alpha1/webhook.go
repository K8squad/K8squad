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

	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// Webhook manifests for these validators are emitted from the per-type
// +kubebuilder:webhook markers on api/v1alpha1/*_types.go (story 1.6),
// which register attribution defaulting/validation and chain these
// cross-ref guards on the same validating paths.

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

// RunCustomValidator validates Run admission (story 1.3 + the story 4.2
// trusted-dev escape gate).
type RunCustomValidator struct{ Validator *CrossRefValidator }

var _ admission.CustomValidator = &RunCustomValidator{}

// ValidateCreate implements admission.CustomValidator.
func (v *RunCustomValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	run, ok := obj.(*ksquadv1alpha1.Run)
	if !ok {
		return nil, apierrors.NewBadRequest("expected a Run but got " + obj.GetObjectKind().GroupVersionKind().String())
	}
	errs := v.Validator.ValidateRun(ctx, run)
	errs = append(errs, v.Validator.ValidateRunTrustedDev(run, nil, requesterFrom(ctx))...)
	return nil, toInvalid("Run", run.Name, errs)
}

// ValidateUpdate implements admission.CustomValidator.
func (v *RunCustomValidator) ValidateUpdate(ctx context.Context, old, newObj runtime.Object) (admission.Warnings, error) {
	newRun, ok := newObj.(*ksquadv1alpha1.Run)
	if !ok {
		return nil, apierrors.NewBadRequest("expected a Run but got " + newObj.GetObjectKind().GroupVersionKind().String())
	}
	oldRun, _ := old.(*ksquadv1alpha1.Run)
	errs := v.Validator.ValidateRun(ctx, newRun)
	errs = append(errs, v.Validator.ValidateRunTrustedDev(newRun, oldRun, requesterFrom(ctx))...)
	return nil, toInvalid("Run", newRun.Name, errs)
}

// requesterFrom extracts the authenticated requester from the admission
// context. No request in context (non-admission callers) yields the zero
// UserInfo — anonymous, i.e. unprivileged: the trusted-dev gate fails
// closed, same as every other guard here.
func requesterFrom(ctx context.Context) authenticationv1.UserInfo {
	if req, err := admission.RequestFromContext(ctx); err == nil {
		return req.UserInfo
	}
	return authenticationv1.UserInfo{}
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

// CrossRefValidators bundles the story 1.3 cross-object validators so the
// wiring layer (internal/webhook attribution setup, story 1.6) can chain
// them onto the shared validating paths instead of registering duplicate
// webhook entries for the same resources.
type CrossRefValidators struct {
	Team  admission.CustomValidator
	Agent admission.CustomValidator
	Run   admission.CustomValidator
}

// NewCrossRefValidators builds the three story 1.3 validators over reader.
// Guards run with failurePolicy=fail: a broken webhook denies rather than
// admits (fail-closed, story 1.3).
func NewCrossRefValidators(reader client.Reader) *CrossRefValidators {
	v := &CrossRefValidator{Reader: reader}
	return &CrossRefValidators{
		Team:  &TeamCustomValidator{Validator: v},
		Agent: &AgentCustomValidator{Validator: v},
		Run:   &RunCustomValidator{Validator: v},
	}
}

// For returns the cross-ref validator for obj's concrete type, or nil when
// the type carries no story 1.3 guards (e.g. Project).
func (c *CrossRefValidators) For(obj runtime.Object) admission.CustomValidator {
	switch obj.(type) {
	case *ksquadv1alpha1.Team:
		return c.Team
	case *ksquadv1alpha1.Agent:
		return c.Agent
	case *ksquadv1alpha1.Run:
		return c.Run
	default:
		return nil
	}
}
