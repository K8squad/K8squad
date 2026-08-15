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

// Package webhook hosts the story 1.6 (ISI-2522) attribution admission
// webhook: createdBy (immutable, stamped at admission) and ownedBy
// (mutable, defaults to the same principal) on the squad-composition CRDs
// (Team, Project, Agent, Run).
package webhook

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// Sentinel errors carrying the machine-readable rejection reasons from
// api/v1alpha1. Compare with errors.Is.
var (
	// ErrCreatedByImmutable: an update attempted to change (or introduce)
	// the created-by annotation.
	ErrCreatedByImmutable = fmt.Errorf("%s: metadata.annotations[%s] is immutable once set", ksquadv1.ReasonCreatedByImmutable, ksquadv1.CreatedByAnnotation)

	// ErrCreatedByNotSettable: a create carried an explicit created-by value
	// for a principal other than the authenticated requester.
	ErrCreatedByNotSettable = fmt.Errorf("%s: metadata.annotations[%s] may not be set explicitly; it is stamped at admission from the authenticated principal", ksquadv1.ReasonCreatedByNotSettable, ksquadv1.CreatedByAnnotation)

	// ErrUnauthenticated: no authenticated principal was available in the
	// admission request to stamp attribution from (fail closed, arch §5.2).
	ErrUnauthenticated = fmt.Errorf("%s: no authenticated principal in admission request; cannot stamp %s", ksquadv1.ReasonUnauthenticated, ksquadv1.CreatedByAnnotation)
)

// AttributionWebhook implements admission.CustomDefaulter and
// admission.CustomValidator generically over runtime.Object, so it serves
// every CRD that carries the attribution contract:
//
//   - types with metadata annotations get the immutable created-by stamp
//     (ksquadv1.CreatedByAnnotation);
//   - types implementing ksquadv1.OwnedByHolder additionally get the mutable
//     spec.ownedBy default (Team, Project, Agent, Run).
//
// The principal is the admission request's authenticated username
// (admission.Request.UserInfo — Kubernetes native authn). Once Epic 15.4's
// identity middleware lands, the apiserver BFF path stamps the ksquad auth
// user_id directly and the same webhook keeps working — only the principal
// vocabulary changes (arch §5.1 r20 note, §12.3/§12.4).
type AttributionWebhook struct{}

var (
	_ admission.CustomDefaulter = &AttributionWebhook{}
	_ admission.CustomValidator = &AttributionWebhook{}
)

// requestPrincipal resolves the authenticated principal from the admission
// request in context. Empty string means no request / no user info (direct
// non-admission calls, unit tests).
func requestPrincipal(ctx context.Context) ksquadv1.PrincipalRef {
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return ""
	}
	return ksquadv1.PrincipalRef(req.UserInfo.Username)
}

// asClientObject narrows runtime.Object to client.Object. Every object
// decoded from an admission request satisfies it.
func asClientObject(obj runtime.Object) (client.Object, error) {
	cobj, ok := obj.(client.Object)
	if !ok {
		return nil, fmt.Errorf("attribution webhook: object %T does not implement client.Object", obj)
	}
	return cobj, nil
}

// Default implements admission.CustomDefaulter. It stamps attribution from
// the authenticated principal, filling only what is unset:
//
//   - created-by annotation: set when absent (never overwritten — an
//     explicit client-set value is left for the validator to reject);
//   - spec.ownedBy (when the type is an OwnedByHolder): set when empty —
//     ownedBy is mutable, so ownership transfer via later updates is legal.
//
// Default runs for updates too; on update the values are already set and
// Default is a no-op.
func (w *AttributionWebhook) Default(ctx context.Context, obj runtime.Object) error {
	cobj, err := asClientObject(obj)
	if err != nil {
		return err
	}

	principal := requestPrincipal(ctx)
	if principal == "" {
		// No principal to default from (e.g. direct non-admission call):
		// leave unset. Create-time validation fails closed instead.
		return nil
	}

	if _, ok := ksquadv1.GetCreatedBy(cobj); !ok {
		ksquadv1.SetCreatedBy(cobj, principal)
	}

	if holder, ok := cobj.(ksquadv1.OwnedByHolder); ok {
		if holder.GetOwnedBy() == "" {
			holder.SetOwnedBy(principal)
		}
	}

	return nil
}

// ValidateCreate implements admission.CustomValidator. Creation fails
// closed when no authenticated principal is present, and rejects an explicit
// created-by value for anyone other than the authenticated requester (the
// defaulter has already stamped the requester's own principal by the time
// validation runs, so the self-match is the normal defaulted path).
func (w *AttributionWebhook) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	cobj, err := asClientObject(obj)
	if err != nil {
		return nil, err
	}

	principal := requestPrincipal(ctx)
	if principal == "" {
		return nil, ErrUnauthenticated
	}

	if existing, ok := ksquadv1.GetCreatedBy(cobj); ok && existing != principal {
		return nil, fmt.Errorf("%w (got %q, authenticated principal %q)", ErrCreatedByNotSettable, existing, principal)
	}

	return nil, nil
}

// ValidateUpdate implements admission.CustomValidator. The created-by
// annotation is immutable: it may neither change nor be introduced after
// creation. ownedBy is deliberately NOT validated here — it is the mutable
// ownership signal (story 1.6 AC, Epic 15.3 scope queries).
func (w *AttributionWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldCObj, err := asClientObject(oldObj)
	if err != nil {
		return nil, err
	}
	newCObj, err := asClientObject(newObj)
	if err != nil {
		return nil, err
	}

	oldPrincipal, oldOK := ksquadv1.GetCreatedBy(oldCObj)
	newPrincipal, newOK := ksquadv1.GetCreatedBy(newCObj)

	switch {
	case oldOK && newOK && oldPrincipal != newPrincipal:
		return nil, fmt.Errorf("%w (was %q, now %q)", ErrCreatedByImmutable, oldPrincipal, newPrincipal)
	case !oldOK && newOK:
		return nil, fmt.Errorf("%w (introduced after creation: %q)", ErrCreatedByImmutable, newPrincipal)
	case oldOK && !newOK:
		return nil, fmt.Errorf("%w (removed after creation: was %q)", ErrCreatedByImmutable, oldPrincipal)
	}

	return nil, nil
}

// ValidateDelete implements admission.CustomValidator. Attribution never
// blocks deletion.
func (w *AttributionWebhook) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// SetupAttributionWebhookWithManager registers the attribution defaulter and
// validator for each provided object type on the manager's webhook server.
// Pass the attributed CRD types (story 1.6: Team, Project, Agent, Run):
//
//	err := webhook.SetupAttributionWebhookWithManager(mgr,
//		&ksquadv1.Team{}, &ksquadv1.Project{}, &ksquadv1.Agent{}, &ksquadv1.Run{})
//
// Per-type +kubebuilder:webhook markers land on the type files together
// with the spec.ownedBy fields (follow-up once story 1.2's types merge), so
// controller-gen emits matching webhook manifests when the manager wiring
// (story 1.3) turns the webhook server on.
func SetupAttributionWebhookWithManager(mgr manager.Manager, objs ...client.Object) error {
	w := &AttributionWebhook{}
	for _, obj := range objs {
		if err := ctrl.WebhookManagedBy(mgr).
			For(obj).
			WithDefaulter(w).
			WithValidator(w).
			Complete(); err != nil {
			return fmt.Errorf("registering attribution webhook for %T: %w", obj, err)
		}
	}
	return nil
}
