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

package rundrive

import (
	"context"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// TestSpecClassifierCarriesNamespaceAndCapabilityHash (Epic C, ADR-044
// steps 7+9): the warm-pool key carries the Run's team namespace (sandbox
// tenancy — pods boot where RBAC/NetworkPolicy/quota live) and the
// capability-manifest hash (identical envelopes share pool stock).
func TestSpecClassifierCarriesNamespaceAndCapabilityHash(t *testing.T) {
	capRun := newTestRun("44444444-4444-4444-4444-444444444444", "wi-4")
	capRun.Name = "cap-run"
	capRun.Status.CapabilityManifest = &api.CapabilityManifest{CapabilityHash: "abc123"}
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(capRun).Build()

	key, _, err := SpecClassifier(cl)(context.Background(), "44444444-4444-4444-4444-444444444444")
	if err != nil {
		t.Fatalf("classify capability run: %v", err)
	}
	if key.Namespace != capRun.Namespace {
		t.Fatalf("key.Namespace = %q, want the run namespace %q", key.Namespace, capRun.Namespace)
	}
	if key.CapabilityHash != "abc123" {
		t.Fatalf("key.CapabilityHash = %q, want abc123", key.CapabilityHash)
	}

	// A Run without a manifest classifies to the bare posture (empty
	// hash), not an error.
	bareRun := newTestRun("55555555-5555-5555-5555-555555555555", "wi-5")
	cl2 := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(bareRun).Build()
	key, _, err = SpecClassifier(cl2)(context.Background(), "55555555-5555-5555-5555-555555555555")
	if err != nil {
		t.Fatalf("classify bare run: %v", err)
	}
	if key.CapabilityHash != "" || key.Namespace != bareRun.Namespace {
		t.Fatalf("bare run classified (%q,%q), want (ns,%q)", key.Namespace, key.CapabilityHash, bareRun.Namespace)
	}
}
