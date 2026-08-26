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

package conformance

import (
	"context"
	"sync"

	"github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/shim"
	"github.com/K8squad/K8squad/pkg/shim/runtimes"
)

// scriptedRunner is the deterministic shim.Runner the conformance suite drives
// the real pkg/shim engine with, in place of the production os/exec runner. It
// replays a fixed Progress plan then settles on a terminal Outcome, so every
// lifecycle, SSE-sequencing and dedup guarantee the engine owns is exercised
// with no live coding-agent CLI — the property that makes the suite runnable in
// a $0 CI lane.
//
// It doubles as an observation point: it records how many times the engine
// launched it (proving submit-reattach dedup, C1) and the exact ExecSpec the
// runtime adapter produced (proving the Ollama lane's credential + endpoint
// mapping, story 5.7).
type scriptedRunner struct {
	// steps is the ordered Progress the runtime "emits" before terminating.
	steps []shim.Progress
	// outcome is the terminal result; the engine maps a canceled context to
	// TaskCanceled itself, so a conformant plan reports completed or failed.
	outcome shim.Outcome
	// blockUntilCancel holds the runtime open (after emitting steps) until the
	// context is canceled — used to exercise the CancelTask drain path (C8).
	blockUntilCancel bool

	mu    sync.Mutex
	runs  int
	specs []runtimes.ExecSpec
}

// Run implements shim.Runner: it emits the scripted plan, optionally blocks for
// cancellation, and returns the terminal Outcome. It never touches spec.Env's
// credential — mirroring the production runner's NFR-SEC3 discipline.
func (r *scriptedRunner) Run(ctx context.Context, spec runtimes.ExecSpec, emit func(shim.Progress)) (shim.Outcome, error) {
	r.mu.Lock()
	r.runs++
	r.specs = append(r.specs, spec)
	r.mu.Unlock()

	for _, p := range r.steps {
		if ctx.Err() != nil {
			return shim.Outcome{}, ctx.Err()
		}
		emit(p)
	}

	if r.blockUntilCancel {
		<-ctx.Done()
		return shim.Outcome{}, ctx.Err()
	}
	if ctx.Err() != nil {
		return shim.Outcome{}, ctx.Err()
	}
	return r.outcome, nil
}

// launchCount returns how many times the engine invoked the runtime (C1: a
// reattach must not launch a second execution).
func (r *scriptedRunner) launchCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runs
}

// lastSpec returns the most recent ExecSpec the runtime adapter produced, for
// asserting the resolved credential + model-endpoint env mapping.
func (r *scriptedRunner) lastSpec() (runtimes.ExecSpec, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.specs) == 0 {
		return runtimes.ExecSpec{}, false
	}
	return r.specs[len(r.specs)-1], true
}

// conformantPlan returns the Progress plan a well-behaved runtime emits during
// the conformance Run: an untrusted progress message, a tool call start+result,
// a usage report, and one artifact-ref per advertised artifact kind (so the
// artifact-emission and capability-honesty checks have real events to assert).
// The task id + work item are stamped by the engine, so only the artifact
// payload's work-item binding is set here.
func conformantPlan(workItemID string, artifactKinds []string) []shim.Progress {
	plan := []shim.Progress{
		{Kind: a2a.EventMessage, Message: &a2a.MessagePayload{Role: "agent", Text: "starting run", Trust: "untrusted"}},
		{Kind: a2a.EventTool, Tool: &a2a.ToolPayload{Name: "shell", Phase: "start"}},
		{Kind: a2a.EventTool, Tool: &a2a.ToolPayload{Name: "shell", Phase: "result", OK: true, Summary: "ok"}},
		{Kind: a2a.EventUsage, Usage: &a2a.UsagePayload{Model: "conformance", Input: 100, Output: 42}},
	}
	for _, kind := range artifactKinds {
		plan = append(plan, shim.Progress{
			Kind: a2a.EventArtifactRef,
			Artifact: &a2a.ArtifactRef{
				Kind:       kind,
				WorkItemID: workItemID,
				URI:        "coord://artifacts/" + workItemID + "/" + kind,
				SHA256:     "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			},
		})
	}
	return plan
}
