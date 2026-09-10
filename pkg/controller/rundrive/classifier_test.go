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

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// TestSpecClassifierAddsProjectPVCDimension (ISI-4127): a Run whose Project
// carries spec.workspacePVC classifies into a pool key naming the per-Project
// claim (so Boot mounts it); a Project without one — or one that no longer
// resolves — leaves the dimension empty and never blocks the bind.
func TestSpecClassifierAddsProjectPVCDimension(t *testing.T) {
	pvcRun := newTestRun("44444444-4444-4444-4444-444444444444", "wi-4")
	pvcRun.Name = "pvc-run"
	plainRun := newTestRun("55555555-5555-5555-5555-555555555555", "wi-5")
	plainRun.Name = "plain-run"
	plainRun.Spec.ProjectRef = api.ObjectRef{Name: "plain"}
	ghostRun := newTestRun("66666666-6666-6666-6666-666666666666", "wi-6")
	ghostRun.Name = "ghost-run"
	ghostRun.Spec.ProjectRef = api.ObjectRef{Name: "ghost"}

	pvcProject := &api.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec: api.ProjectSpec{
			Repo:         api.RepoSpec{URL: "https://github.com/acme/widget"},
			WorkspacePVC: &api.PVCSpec{Size: resource.MustParse("10Gi"), Class: "longhorn"},
		},
	}
	plainProject := &api.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: "default"},
		Spec:       api.ProjectSpec{Repo: api.RepoSpec{URL: "https://github.com/acme/plain"}},
	}

	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(pvcRun, plainRun, ghostRun, pvcProject, plainProject).Build()
	cls := SpecClassifier(cl)
	ctx := context.Background()

	key, _, err := cls(ctx, "44444444-4444-4444-4444-444444444444")
	if err != nil {
		t.Fatalf("classify pvc run: %v", err)
	}
	if key.ProjectPVC != "workspace-project-p" {
		t.Fatalf("ProjectPVC = %q, want workspace-project-p", key.ProjectPVC)
	}

	key, _, err = cls(ctx, "55555555-5555-5555-5555-555555555555")
	if err != nil {
		t.Fatalf("classify plain run: %v", err)
	}
	if key.ProjectPVC != "" {
		t.Fatalf("ProjectPVC = %q, want empty for a workspace-less Project", key.ProjectPVC)
	}

	// Unresolvable Project: no mount, no error (classify never blocks).
	key, _, err = cls(ctx, "66666666-6666-6666-6666-666666666666")
	if err != nil {
		t.Fatalf("classify ghost run: %v", err)
	}
	if key.ProjectPVC != "" {
		t.Fatalf("ProjectPVC = %q, want empty for an unresolvable Project", key.ProjectPVC)
	}
}
