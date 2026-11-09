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

// TestFinalCommentContractShape pins the board-approved rev2 contract (ISI-7195,
// Henrik direction 2026-11-08): the durable comment must read as a clear,
// natural summary of the work plus an explicit ask when a board decision is
// needed — Paperclip-style, NOT a rigid stamped template. These assertions
// guard the two load-bearing properties (verbatim publication, no trace dumps)
// and the natural-summary + board-ask shape.
func TestFinalCommentContractShape(t *testing.T) {
	for _, want := range []string{
		"VERBATIM",                     // the final message is published verbatim as the durable comment
		"natural summary",              // the shape the board asked for
		"decision",                     // the explicit board-ask clause
		"Do not impose fixed headings", // the rejection of the stamped template
	} {
		if !strings.Contains(finalCommentContract, want) {
			t.Errorf("finalCommentContract missing required phrase %q", want)
		}
	}
	if !strings.Contains(finalCommentContract, "Do not paste tool logs") {
		t.Error("finalCommentContract must forbid trace/log dumps in the final message")
	}
}

// TestFinalCommentContractRejectsStampedTemplate is a regression guard: Henrik
// explicitly rejected the rev1 five-section stamped template (ISI-7195
// 2026-11-08). The contract must not reintroduce those fixed headings as a
// required per-response form.
func TestFinalCommentContractRejectsStampedTemplate(t *testing.T) {
	for _, forbidden := range []string{
		"under these five headings",
		"**What was asked**",
		"**Decisions & why**",
		"**Follow-ups**",
	} {
		if strings.Contains(finalCommentContract, forbidden) {
			t.Errorf("finalCommentContract reintroduced rejected stamped-template element %q", forbidden)
		}
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
