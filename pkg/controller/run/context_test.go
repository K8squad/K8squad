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

package run

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/contextasm"
	"github.com/K8squad/K8squad/pkg/reconcile"
)

// stubSources is an in-memory contextasm.Sources for wiring tests: it returns
// a fixed rich work item + project meta and honors the pinned-revision
// contract (a non-matching pin errors, exercising deterministic-resume).
type stubSources struct {
	wiRev   string
	goalRev string
	err     error
}

func (s stubSources) WorkItem(_ context.Context, id, rev string) (contextasm.WorkItemFacts, error) {
	if s.err != nil {
		return contextasm.WorkItemFacts{}, s.err
	}
	if rev != "" && rev != s.wiRev {
		return contextasm.WorkItemFacts{}, errors.New("pinned work-item revision no longer resolves")
	}
	return contextasm.WorkItemFacts{
		ID:                 id,
		Revision:           s.wiRev,
		Title:              "Fix the flake",
		Description:        "make test/flake green",
		AcceptanceCriteria: []string{"CI is green three runs straight"},
		Comments:           []contextasm.Comment{{Author: "pm", Content: "seen intermittently on arm64", WrittenAt: "2026-08-01T00:00:00Z"}},
	}, nil
}

func (s stubSources) ProjectMeta(_ context.Context, _ /*projectRef*/, revision string) (contextasm.ProjectMeta, error) {
	if revision != "" && revision != s.goalRev {
		return contextasm.ProjectMeta{}, errors.New("pinned project generation no longer resolves")
	}
	return contextasm.ProjectMeta{
		ProjectRevision: s.goalRev,
		RepoURL:         "https://github.com/acme/widget",
		RepoRef:         "main",
		Goals:           []string{"ship a reliable CI"},
	}, nil
}

func (s stubSources) MemoryRecall(_ context.Context, _, _ string, _ []string, _ int) ([]contextasm.RecallDoc, error) {
	return nil, nil
}

func (s stubSources) Artifacts(_ context.Context, _ string) ([]contextasm.ArtifactLink, error) {
	return nil, nil
}

// stubAssemblers is a ContextAssemblers over a fixed stubSources.
type stubAssemblers struct{ src contextasm.Sources }

func (a stubAssemblers) For(string) *contextasm.Assembler { return contextasm.NewAssembler(a.src, 8) }

func runWithAgent() (*api.Run, *api.Agent, *api.Project) {
	run := &api.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "default", UID: types.UID("uid-run-1"), Generation: 4},
		Spec: api.RunSpec{
			WorkItemRef: "wi-abc-123",
			TeamRef:     api.ObjectRef{Name: "team-a"},
			ProjectRef:  api.ObjectRef{Name: "proj-1"},
			Agents:      []api.ObjectRef{{Name: "coder"}},
		},
	}
	agent := &api.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: "default"},
		Spec:       api.AgentSpec{Model: "claude-sonnet-4"},
	}
	project := &api.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "proj-1", Namespace: "default", Generation: 1},
		Spec:       api.ProjectSpec{Repo: api.RepoSpec{URL: "https://github.com/acme/widget", Ref: "main"}, Goals: []string{"ship a reliable CI"}},
	}
	return run, agent, project
}

// AC2: the reconciler assembles and pins the snapshot at Claiming → Running.
func TestReconcilePinsContextSnapshot(t *testing.T) {
	run, agent, project := runWithAgent()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(run, agent, project).WithStatusSubresource(&api.Run{}).Build()

	r := &Reconciler{
		Client:            c,
		Source:            fakeSource{step: reconcile.StepRunning, found: true},
		Now:               func() metav1.Time { return fixedNow },
		ContextAssemblers: stubAssemblers{src: stubSources{wiRev: "rev-1", goalRev: "1"}},
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "run-1", Namespace: "default"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got api.Run
	if err := c.Get(context.Background(), types.NamespacedName{Name: "run-1", Namespace: "default"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	snap := got.Status.ContextSnapshot
	if snap == nil {
		t.Fatal("ContextSnapshot not pinned")
	}
	if snap.WorkItemRevision != "rev-1" {
		t.Errorf("WorkItemRevision = %q, want rev-1", snap.WorkItemRevision)
	}
	if snap.GoalRevision != "1" {
		t.Errorf("GoalRevision = %q, want 1", snap.GoalRevision)
	}
	if snap.ContextWindow == nil || *snap.ContextWindow != 200000 {
		t.Errorf("ContextWindow = %v, want 200000 (claude-sonnet-4)", snap.ContextWindow)
	}
	if snap.Budget == nil {
		t.Error("Budget not recorded on snapshot")
	}
	if snap.AssembledAt == nil {
		t.Error("AssembledAt not stamped")
	}
}

// AC3 (immutability arm): a second reconcile reuses the pinned snapshot rather
// than re-assembling latest — the resume determinism guarantee.
func TestReconcileSnapshotIsImmutable(t *testing.T) {
	run, agent, project := runWithAgent()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(run, agent, project).WithStatusSubresource(&api.Run{}).Build()

	r := &Reconciler{
		Client:            c,
		Source:            fakeSource{step: reconcile.StepRunning, found: true},
		Now:               func() metav1.Time { return fixedNow },
		ContextAssemblers: stubAssemblers{src: stubSources{wiRev: "rev-1", goalRev: "1"}},
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "run-1", Namespace: "default"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	// The stored work item now "drifts" to a new revision; a re-read of latest
	// would change the snapshot. The pinned snapshot must be kept as-is.
	r.ContextAssemblers = stubAssemblers{src: stubSources{wiRev: "rev-2", goalRev: "2"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	var got api.Run
	_ = c.Get(context.Background(), req.NamespacedName, &got)
	if got.Status.ContextSnapshot == nil || got.Status.ContextSnapshot.WorkItemRevision != "rev-1" {
		t.Errorf("snapshot re-assembled to latest; want the pinned rev-1, got %+v", got.Status.ContextSnapshot)
	}
}

// AC4: a Sources read error fails the reconcile closed (requeue), and no
// partial snapshot is written.
func TestReconcileContextAssemblyFailsClosed(t *testing.T) {
	run, agent, project := runWithAgent()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(run, agent, project).WithStatusSubresource(&api.Run{}).Build()

	r := &Reconciler{
		Client:            c,
		Source:            fakeSource{step: reconcile.StepRunning, found: true},
		Now:               func() metav1.Time { return fixedNow },
		ContextAssemblers: stubAssemblers{src: stubSources{err: errors.New("coord down")}},
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "run-1", Namespace: "default"}}); err == nil {
		t.Fatal("expected fail-closed reconcile error, got nil")
	}
	var got api.Run
	_ = c.Get(context.Background(), types.NamespacedName{Name: "run-1", Namespace: "default"}, &got)
	if got.Status.ContextSnapshot != nil {
		t.Error("partial snapshot written on assembly failure")
	}
}

// AC6: with the side-channel disabled (nil ContextAssemblers), the reconciler
// is unchanged — no snapshot, no error.
func TestReconcileNoContextSideChannelIsNoop(t *testing.T) {
	run, agent, project := runWithAgent()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(run, agent, project).WithStatusSubresource(&api.Run{}).Build()

	if _, err := reconcileOnce(t, c, fakeSource{step: reconcile.StepRunning, found: true}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got api.Run
	_ = c.Get(context.Background(), types.NamespacedName{Name: "run-1", Namespace: "default"}, &got)
	if got.Status.ContextSnapshot != nil {
		t.Error("snapshot pinned with no ContextAssemblers wired")
	}
}
