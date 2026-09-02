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
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/contextasm"
)

const ctxRunUID = "11111111-2222-3333-4444-555555555555"

// stubSources is an in-memory contextasm.Sources honoring the pinned-revision
// contract (a non-matching pin errors — deterministic-resume).
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
		AcceptanceCriteria: []string{"CI green three runs straight"},
		Comments:           []contextasm.Comment{{Author: "pm", Content: "seen on arm64", WrittenAt: "2026-08-01T00:00:00Z"}},
	}, nil
}

func (s stubSources) ProjectMeta(_ context.Context, _, revision string) (contextasm.ProjectMeta, error) {
	if revision != "" && revision != s.goalRev {
		return contextasm.ProjectMeta{}, errors.New("pinned project generation no longer resolves")
	}
	return contextasm.ProjectMeta{ProjectRevision: s.goalRev, RepoURL: "https://github.com/acme/widget", RepoRef: "main", Goals: []string{"reliable CI"}}, nil
}

func (s stubSources) MemoryRecall(_ context.Context, _, _ string, _ []string, _ int) ([]contextasm.RecallDoc, error) {
	return nil, nil
}

func (s stubSources) Artifacts(_ context.Context, _ string) ([]contextasm.ArtifactLink, error) {
	return nil, nil
}

type stubAssemblers struct{ src contextasm.Sources }

func (a stubAssemblers) For(string) *contextasm.Assembler { return contextasm.NewAssembler(a.src, 8) }

func ctxFixtures(snapshot *api.ContextSnapshot) (*api.Run, *api.Agent, *api.Project) {
	run := &api.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "team-a", UID: types.UID(ctxRunUID)},
		Spec: api.RunSpec{
			TeamRef:     api.ObjectRef{Name: "team-a"},
			ProjectRef:  api.ObjectRef{Name: "proj-1"},
			WorkItemRef: "99999999-8888-7777-6666-555555555555",
			Agents:      []api.ObjectRef{{Name: "coder"}},
		},
		Status: api.RunStatus{ContextSnapshot: snapshot},
	}
	agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: "team-a"}, Spec: api.AgentSpec{Model: "claude-sonnet-4"}}
	project := &api.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "proj-1", Namespace: "team-a", Generation: 1},
		Spec:       api.ProjectSpec{Repo: api.RepoSpec{URL: "https://github.com/acme/widget", Ref: "main"}, Goals: []string{"reliable CI"}},
	}
	return run, agent, project
}

func newDispatch(t *testing.T, run *api.Run, agent *api.Agent, project *api.Project, asm ContextAssemblers) *operatorDispatch {
	t.Helper()
	cl := fake.NewClientBuilder().WithScheme(dispatchScheme(t)).WithObjects(run, agent, project).Build()
	return &operatorDispatch{
		cfg:    OperatorDispatchConfig{Client: cl, ContextAssemblers: asm},
		source: fakeDispatchSource{title: "Fix the flake", body: "make test/flake green", fence: "7"},
	}
}

// AC1: with a pinned snapshot and the context side-channel wired, buildTask
// sets a non-empty tier-framed SystemContext while env.Input still carries the
// concrete work instruction (additive, not a replacement).
func TestBuildTaskInjectsSystemContext(t *testing.T) {
	snap := &api.ContextSnapshot{WorkItemRevision: "rev-1", GoalRevision: "1"}
	run, agent, project := ctxFixtures(snap)
	d := newDispatch(t, run, agent, project, stubAssemblers{src: stubSources{wiRev: "rev-1", goalRev: "1"}})

	task, err := d.buildTask(context.Background(), ctxRunUID, ctxRunUID)
	if err != nil {
		t.Fatalf("buildTask: %v", err)
	}
	if task.Envelope.Input != "make test/flake green" {
		t.Errorf("Input = %q, want the work-item body (SystemContext must be additive)", task.Envelope.Input)
	}
	sc := task.Envelope.SystemContext
	if sc == "" {
		t.Fatal("SystemContext empty; expected the assembled §8.5 envelope")
	}
	for _, want := range []string{"AUTHORITATIVE CONTEXT", "CI green three runs straight", "seen on arm64", "reliable CI"} {
		if !strings.Contains(sc, want) {
			t.Errorf("SystemContext missing %q; got:\n%s", want, sc)
		}
	}
}

// AC3: re-driving (rebuilding the task) re-reads the pinned snapshot and
// renders a byte-identical SystemContext.
func TestBuildTaskSystemContextIsDeterministic(t *testing.T) {
	snap := &api.ContextSnapshot{WorkItemRevision: "rev-1", GoalRevision: "1"}
	run, agent, project := ctxFixtures(snap)
	d := newDispatch(t, run, agent, project, stubAssemblers{src: stubSources{wiRev: "rev-1", goalRev: "1"}})

	first, err := d.buildTask(context.Background(), ctxRunUID, ctxRunUID)
	if err != nil {
		t.Fatalf("buildTask 1: %v", err)
	}
	second, err := d.buildTask(context.Background(), ctxRunUID, ctxRunUID)
	if err != nil {
		t.Fatalf("buildTask 2: %v", err)
	}
	if first.Envelope.SystemContext != second.Envelope.SystemContext {
		t.Error("SystemContext not byte-identical across re-drive (deterministic-resume violated)")
	}
}

// AC3 (loud-failure arm): a pinned revision that no longer resolves fails the
// build closed, never silently falling back to latest.
func TestBuildTaskPinnedRevisionMismatchFailsClosed(t *testing.T) {
	snap := &api.ContextSnapshot{WorkItemRevision: "rev-1", GoalRevision: "1"}
	run, agent, project := ctxFixtures(snap)
	// Sources now only knows rev-2: the pinned rev-1 must error.
	d := newDispatch(t, run, agent, project, stubAssemblers{src: stubSources{wiRev: "rev-2", goalRev: "1"}})

	if _, err := d.buildTask(context.Background(), ctxRunUID, ctxRunUID); err == nil {
		t.Fatal("expected fail-closed on pinned-revision mismatch, got nil")
	}
}

// Race fix (#5): a missing snapshot must NOT silently ship title+body only.
// With the side-channel wired but the reconciler not having pinned yet,
// buildTask assembles fresh so the dispatch is still fully contextualised.
func TestBuildTaskFreshAssembleWhenNoSnapshot(t *testing.T) {
	run, agent, project := ctxFixtures(nil)
	d := newDispatch(t, run, agent, project, stubAssemblers{src: stubSources{wiRev: "rev-1", goalRev: "1"}})

	task, err := d.buildTask(context.Background(), ctxRunUID, ctxRunUID)
	if err != nil {
		t.Fatalf("buildTask: %v", err)
	}
	if !strings.Contains(task.Envelope.SystemContext, "CI green three runs straight") {
		t.Errorf("SystemContext not assembled fresh when snapshot is nil; got:\n%s", task.Envelope.SystemContext)
	}
	if task.Envelope.Input != "make test/flake green" {
		t.Errorf("Input = %q, want the work-item body", task.Envelope.Input)
	}
}

// AC6: with the side-channel OFF (no ContextAssemblers), the bare title+body
// dispatch is unchanged — SystemContext stays empty.
func TestBuildTaskNoAssemblerLeavesSystemContextEmpty(t *testing.T) {
	run, agent, project := ctxFixtures(nil)
	d := newDispatch(t, run, agent, project, nil)

	task, err := d.buildTask(context.Background(), ctxRunUID, ctxRunUID)
	if err != nil {
		t.Fatalf("buildTask: %v", err)
	}
	if task.Envelope.SystemContext != "" {
		t.Errorf("SystemContext = %q, want empty (side-channel off)", task.Envelope.SystemContext)
	}
}
