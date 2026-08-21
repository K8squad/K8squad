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
	"github.com/K8squad/K8squad/pkg/warmpool"
)

// TestSpecClassifierResolvesRunSpec: the warm-pool key/class come from the
// Run CRD's spec.sandboxPolicy, with the story 1.3 admission defaults applied
// read-side (gvisor/interactive) — including for Runs whose spec predates the
// defaulting or that no longer resolve (deleted mid-bind: defaults, never an
// error, so classification never blocks a bind).
func TestSpecClassifierResolvesRunSpec(t *testing.T) {
	specRun := newTestRun("11111111-1111-1111-1111-111111111111", "wi-1")
	specRun.Name = "spec-run"
	specRun.Spec.SandboxPolicy = api.SandboxPolicy{RuntimeClass: "kata", Class: "batch"}
	defaultRun := newTestRun("22222222-2222-2222-2222-222222222222", "wi-2") // empty policy → defaults
	defaultRun.Name = "default-run"
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(specRun, defaultRun).Build()

	cls := SpecClassifier(cl)
	ctx := context.Background()

	key, class, err := cls(ctx, "11111111-1111-1111-1111-111111111111")
	if err != nil {
		t.Fatalf("classify spec run: %v", err)
	}
	if key.RuntimeClass != "kata" || class != warmpool.ClassBatch {
		t.Fatalf("spec run classified (%q,%q), want (kata,batch)", key.RuntimeClass, class)
	}

	key, class, err = cls(ctx, "22222222-2222-2222-2222-222222222222")
	if err != nil {
		t.Fatalf("classify default run: %v", err)
	}
	if key.RuntimeClass != "gvisor" || class != warmpool.ClassInteractive {
		t.Fatalf("default run classified (%q,%q), want (gvisor,interactive)", key.RuntimeClass, class)
	}

	// Unknown runID (deleted mid-bind): defaults, no error.
	key, class, err = cls(ctx, "33333333-3333-3333-3333-333333333333")
	if err != nil {
		t.Fatalf("classify unknown run: %v", err)
	}
	if key.RuntimeClass != "gvisor" || class != warmpool.ClassInteractive {
		t.Fatalf("unknown run classified (%q,%q), want defaults", key.RuntimeClass, class)
	}
}
