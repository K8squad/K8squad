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
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
)

// TestGroupVersion pins the versioned contract every K8squad CRD shares
// (story 1.1 / arch §5.1). If someone renames the group or bumps the version
// without meaning to, this fails loudly.
func TestGroupVersion(t *testing.T) {
	if got, want := GroupVersion.Group, "ksquad.io"; got != want {
		t.Errorf("GroupVersion.Group = %q, want %q", got, want)
	}
	if got, want := GroupVersion.Version, "v1alpha1"; got != want {
		t.Errorf("GroupVersion.Version = %q, want %q", got, want)
	}
}

// TestAddToScheme proves the group's types register into a runtime scheme — the
// wiring the operator/apiserver rely on. Team must be recognized under the
// ksquad.io/v1alpha1 GVK once AddToScheme runs.
func TestAddToScheme(t *testing.T) {
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	gvk := GroupVersion.WithKind("Team")
	if !s.Recognizes(gvk) {
		t.Fatalf("scheme does not recognize %s", gvk)
	}
}
