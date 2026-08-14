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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Principal attribution (story 1.6 / ISI-2522, arch §5.1 r20 note, epics
// Epic-1 story 1.6 AC).
//
// Two attribution signals land on the squad-composition CRDs (Team, Project,
// Agent, Run):
//
//   - createdBy — metadata-level (`metadata.annotations[ksquad.io/created-by]`),
//     immutable after creation, stamped at admission from the authenticated
//     principal (admission request UserInfo). CEL cannot validate annotation
//     contents, so immutability and create-time override rejection are
//     enforced by the attribution admission webhook (internal/webhook).
//
//   - ownedBy — spec-level (`spec.ownedBy`), a mutable owner principal ref.
//     This is the authoritative ownership signal for resource-scoped
//     permission checks (Epic 15.3) — not a display field.
//
// Both are indexed for RBAC scope queries (internal/index) and surfaced in
// the audit trail (story 2.6).

const (
	// CreatedByAnnotation is the metadata-level, admission-stamped principal
	// that created the resource (arch §5.1 r20, ISI-2303). Set at admission
	// from the authenticated principal; immutable after creation.
	CreatedByAnnotation = "ksquad.io/created-by"

	// InitiatedByAnnotation is the Run annotation carrying the triggering
	// human actor (arch §5.1 r20 note, §8, §12.4). Wiring of the value is
	// story 15.5 (Run identity propagation); the constant lands here so the
	// annotation vocabulary is defined once.
	InitiatedByAnnotation = "ksquad.io/initiated-by"
)

// Cache field-index keys for RBAC scope queries (Epic 15.3). These are the
// keys passed to client.MatchingFields after
// internal/index.RegisterAttributionIndexes registered them on the manager
// cache.
const (
	// CreatedByIndexKey indexes metadata.annotations[ksquad.io/created-by].
	CreatedByIndexKey = "metadata.annotations[ksquad.io/created-by]"

	// OwnedByIndexKey indexes spec.ownedBy.
	OwnedByIndexKey = "spec.ownedBy"
)

// PrincipalRef identifies an authenticated principal for attribution and
// ownership. Until Epic 15.4's identity middleware lands, the principal is
// the Kubernetes-authenticated username from the admission request UserInfo
// (e.g. "henrik", "system:serviceaccount:ksquad:apiserver"). Once the
// ksquad auth schema (§12.3) exists, principals map to auth user ids; the
// apiserver BFF path stamps the user_id directly (arch §5.1 r20 note).
//
// +kubebuilder:validation:MaxLength=253
// +kubebuilder:validation:MinLength=1
type PrincipalRef string

// AttributionValidationErrorReasons are the machine-readable condition
// reasons used by the attribution webhook when rejecting a request.
const (
	// ReasonCreatedByImmutable is used when an update attempts to change
	// metadata.annotations[ksquad.io/created-by].
	ReasonCreatedByImmutable = "CreatedByImmutable"

	// ReasonCreatedByNotSettable is used when a create carries an explicit
	// createdBy value that does not match the authenticated principal.
	ReasonCreatedByNotSettable = "CreatedByNotSettable"

	// ReasonUnauthenticated is used when no authenticated principal is
	// available in the admission request to stamp attribution from.
	ReasonUnauthenticated = "Unauthenticated"
)

// GetCreatedBy returns the principal stamped in the created-by annotation,
// and whether it is present.
func GetCreatedBy(obj metav1.Object) (PrincipalRef, bool) {
	v, ok := obj.GetAnnotations()[CreatedByAnnotation]
	if !ok || v == "" {
		return "", false
	}
	return PrincipalRef(v), true
}

// SetCreatedBy stamps the created-by annotation. Only admission may call
// this (the webhook); the annotation is immutable afterwards.
func SetCreatedBy(obj metav1.Object, principal PrincipalRef) {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[CreatedByAnnotation] = string(principal)
	obj.SetAnnotations(annotations)
}

// OwnedByHolder is implemented by the CRD types that carry spec.ownedBy
// (Team, Project, Agent, Run — story 1.6). Keeping the accessor pair behind
// an interface lets the attribution webhook and the cache indexers stay
// generic over client.Object instead of referencing concrete types.
type OwnedByHolder interface {
	client.Object

	// GetOwnedBy returns the spec.ownedBy principal ref.
	GetOwnedBy() PrincipalRef

	// SetOwnedBy sets the spec.ownedBy principal ref.
	SetOwnedBy(PrincipalRef)
}

// CreatedByIndexValue is the cache field-index extractor for the created-by
// annotation (CreatedByIndexKey). Objects without the annotation index no
// value.
func CreatedByIndexValue(obj client.Object) []string {
	if p, ok := GetCreatedBy(obj); ok {
		return []string{string(p)}
	}
	return nil
}

// OwnedByIndexValue is the cache field-index extractor for spec.ownedBy
// (OwnedByIndexKey). Objects without an owner (or types that do not carry
// ownedBy) index no value.
func OwnedByIndexValue(obj client.Object) []string {
	holder, ok := obj.(OwnedByHolder)
	if !ok {
		return nil
	}
	if p := holder.GetOwnedBy(); p != "" {
		return []string{string(p)}
	}
	return nil
}
