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
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// TestPVCSpecEffectiveAccessModes pins the §9.4 defaulting rule: an unset
// spec.accessModes resolves to [ReadWriteOnce] (serialize-via-lease regime),
// while an explicit value is passed through verbatim.
func TestPVCSpecEffectiveAccessModes(t *testing.T) {
	tests := []struct {
		name string
		in   []corev1.PersistentVolumeAccessMode
		want []corev1.PersistentVolumeAccessMode
	}{
		{
			name: "unset defaults to RWO",
			in:   nil,
			want: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		},
		{
			name: "empty defaults to RWO",
			in:   []corev1.PersistentVolumeAccessMode{},
			want: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		},
		{
			name: "explicit RWX passes through",
			in:   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			want: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &PVCSpec{AccessModes: tt.in}
			got := s.EffectiveAccessModes()
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("EffectiveAccessModes() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestPVCSpecEffectiveAccessModesReturnsCopy ensures the default is not the
// shared package var: mutating the result must not corrupt later calls.
func TestPVCSpecEffectiveAccessModesReturnsCopy(t *testing.T) {
	s := &PVCSpec{}
	first := s.EffectiveAccessModes()
	first[0] = corev1.ReadWriteMany // mutate the caller's copy

	second := s.EffectiveAccessModes()
	if second[0] != corev1.ReadWriteOnce {
		t.Fatalf("shared default mutated: got %v, want [ReadWriteOnce]", second)
	}
}
