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

// Package v1alpha1 contains API Schema definitions for the ksquad v1alpha1 API
// group. Every K8squad CRD (Team, Agent, Role, Skill, Project, Run, and the
// platform-scoped OTelConfig — see arch §5.1) shares this single versioned
// contract. Story 1.1 scaffolds the group; story 1.2 adds the concrete types.
// +kubebuilder:object:generate=true
// +groupName=ksquad.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "ksquad.io", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &schemeBuilder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// schemeBuilder is a minimal replacement for the deprecated
// controller-runtime pkg/scheme.Builder. Per that deprecation, an api package
// should depend only on apimachinery (not controller-runtime), so this is
// backed directly by runtime.SchemeBuilder. The Register(&Type{}, &TypeList{})
// shape is preserved so the per-type init() registrations across this package
// stay untouched.
type schemeBuilder struct {
	GroupVersion schema.GroupVersion
	runtime.SchemeBuilder
}

// Register adds one or more objects to the underlying SchemeBuilder so they
// can be added to a Scheme. Register mutates the builder.
func (b *schemeBuilder) Register(objects ...runtime.Object) *schemeBuilder {
	b.SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(b.GroupVersion, objects...)
		metav1.AddToGroupVersion(s, b.GroupVersion)
		return nil
	})
	return b
}

// AddToScheme registers this builder's types into s.
func (b *schemeBuilder) AddToScheme(s *runtime.Scheme) error {
	return b.SchemeBuilder.AddToScheme(s)
}
