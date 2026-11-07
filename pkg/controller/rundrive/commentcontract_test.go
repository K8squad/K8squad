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
	"strings"
	"testing"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// TestFinalCommentContractSections pins the contract's five section headings —
// the board-approved comment template (ISI-7100 WS-3): the durable comment must
// read as a human summary of work done + decisions made, consistently across
// agents and runtimes.
func TestFinalCommentContractSections(t *testing.T) {
	for _, want := range []string{
		"What was asked",
		"What changed",
		"Decisions & why",
		"Verification",
		"Follow-ups",
	} {
		if !strings.Contains(finalCommentContract, want) {
			t.Errorf("finalCommentContract missing section %q", want)
		}
	}
	if !strings.Contains(finalCommentContract, "VERBATIM") {
		t.Error("finalCommentContract must tell the model its final message is published verbatim")
	}
	if !strings.Contains(finalCommentContract, "Do not paste tool logs") {
		t.Error("finalCommentContract must forbid trace/log dumps in the final message")
	}
}

// TestAppendFinalCommentContract covers the join shapes: an empty context
// becomes the bare contract; an assembled context keeps the contract LAST (the
// closing instruction) and never duplicates it.
func TestAppendFinalCommentContract(t *testing.T) {
	contract := strings.TrimSpace(finalCommentContract)
	if got := appendFinalCommentContract(""); got != contract {
		t.Errorf("empty context: got %q, want the bare contract", got)
	}
	if got := appendFinalCommentContract("  \t\n"); got != contract {
		t.Errorf("whitespace-only context: got %q, want the bare contract", got)
	}
	got := appendFinalCommentContract("assembled context")
	if !strings.HasPrefix(got, "assembled context\n\n") || !strings.HasSuffix(got, contract) {
		t.Errorf("existing context: contract must join last; got %q", got)
	}
	if strings.Count(got, "How to report") != 1 {
		t.Errorf("contract duplicated: %q", got)
	}
}

// TestBuildTaskCarriesCommentContractWithAssembler proves the contract rides
// AFTER an assembled §8.5 context while Input stays the bare work-item body —
// additive guidance, never a replacement of the instruction.
func TestBuildTaskCarriesCommentContractWithAssembler(t *testing.T) {
	contract := strings.TrimSpace(finalCommentContract)
	snap := &api.ContextSnapshot{WorkItemRevision: "rev-1", GoalRevision: "1"}
	run, agent, project := ctxFixtures(snap)
	d := newDispatch(t, run, agent, project, stubAssemblers{src: stubSources{wiRev: "rev-1", goalRev: "1"}})

	task, err := d.buildTask(context.Background(), ctxRunUID, ctxRunUID)
	if err != nil {
		t.Fatalf("buildTask: %v", err)
	}
	if task.Envelope.Input != "make test/flake green" {
		t.Errorf("Input = %q, want the work-item body", task.Envelope.Input)
	}
	sc := task.Envelope.SystemContext
	if !strings.Contains(sc, "AUTHORITATIVE CONTEXT") {
		t.Fatalf("SystemContext lost the assembled envelope; got:\n%s", sc)
	}
	if !strings.HasSuffix(sc, contract) {
		tailStr := sc
		if len(tailStr) > 300 {
			tailStr = tailStr[len(tailStr)-300:]
		}
		t.Errorf("SystemContext must END with the final-comment contract; got tail: %q", tailStr)
	}
}
