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

	"github.com/DATA-DOG/go-sqlmock"
	api "github.com/K8squad/K8squad/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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

// TestDiscussionReplyContractShape pins the WS-7a discussion-room variant
// (ISI-7367): the in-run reply the board reads in the room is shaped as a
// natural summary + explicit ask, inline code only for exact commands/paths,
// no tool narration or command+prose interleaving, detail parked in work
// items / linked issues, and a soft ~1200-char target.
func TestDiscussionReplyContractShape(t *testing.T) {
	for _, want := range []string{
		"read by the board directly in the room",
		"state that ask explicitly",
		"exact command, path, or identifier",
		"never interleaved with prose",
		"park that detail in work items or linked issues",
		"about 1200 characters",
		"soft target, not a limit",
	} {
		if !strings.Contains(discussionReplyContract, want) {
			t.Errorf("discussionReplyContract missing required phrase %q", want)
		}
	}
}

// TestDiscussionReplyContractRejectsStampedTemplate carries Henrik's ISI-7195
// rejection of the rigid five-section form over to the discussion variant.
func TestDiscussionReplyContractRejectsStampedTemplate(t *testing.T) {
	for _, forbidden := range []string{
		"under these five headings",
		"**What was asked**",
		"**Decisions & why**",
		"**Follow-ups**",
	} {
		if strings.Contains(discussionReplyContract, forbidden) {
			t.Errorf("discussionReplyContract reintroduced rejected stamped-template element %q", forbidden)
		}
	}
}

// TestAppendDispatchContracts covers the origin-aware seam (ISI-7367): a
// non-discussion envelope is byte-identical to the plain final-comment
// append; a discussion-origin envelope keeps BOTH contracts, the reply
// contract LAST, never duplicated.
func TestAppendDispatchContracts(t *testing.T) {
	plain := appendDispatchContracts("assembled context", false)
	if plain != appendFinalCommentContract("assembled context") {
		t.Errorf("non-discussion envelope must be identical to the plain append; got:\n%s", plain)
	}
	final := strings.TrimSpace(finalCommentContract)
	reply := strings.TrimSpace(discussionReplyContract)
	disc := appendDispatchContracts("assembled context", true)
	if !strings.Contains(disc, final) || !strings.HasSuffix(disc, reply) {
		tail := disc
		if len(tail) > 300 {
			tail = tail[len(tail)-300:]
		}
		t.Errorf("discussion envelope must carry both contracts, reply LAST; got tail: %q", tail)
	}
	if got, want := appendDispatchContracts("", true), final+"\n\n"+reply; got != want {
		t.Errorf("empty discussion context: got %q, want final+reply contracts", got)
	}
	if strings.Count(disc, "How to report") != 1 || strings.Count(disc, "How to post in the discussion room") != 1 {
		t.Errorf("contracts duplicated: %q", disc)
	}
}

// TestBuildTaskCarriesDiscussionReplyContract proves the origin-aware append
// at the buildTask seam (ISI-7367): a source='discussion' work item's
// SystemContext closes with the discussion-room reply contract after the
// final-comment contract, while Input stays the bare work-item body.
func TestBuildTaskCarriesDiscussionReplyContract(t *testing.T) {
	snap := &api.ContextSnapshot{WorkItemRevision: "rev-1", GoalRevision: "1"}
	run, agent, project := ctxFixtures(snap)
	cl := fake.NewClientBuilder().WithScheme(dispatchScheme(t)).WithObjects(run, agent, project, dispatchTeamObj()).Build()
	d := &operatorDispatch{
		cfg:    OperatorDispatchConfig{Client: cl, ContextAssemblers: stubAssemblers{src: stubSources{wiRev: "rev-1", goalRev: "1"}}},
		source: fakeDispatchSource{title: "Reply to the room", body: "answer the question", fence: "7", source: "discussion"},
	}
	task, err := d.buildTask(context.Background(), ctxRunUID, ctxRunUID)
	if err != nil {
		t.Fatalf("buildTask: %v", err)
	}
	if task.Envelope.Input != "answer the question" {
		t.Errorf("Input = %q, want the work-item body", task.Envelope.Input)
	}
	sc := task.Envelope.SystemContext
	if !strings.Contains(sc, "AUTHORITATIVE CONTEXT") {
		t.Fatalf("SystemContext lost the assembled envelope; got:\n%s", sc)
	}
	if !strings.Contains(sc, strings.TrimSpace(finalCommentContract)) {
		t.Error("discussion envelope must still carry the final-comment contract")
	}
	if !strings.HasSuffix(sc, strings.TrimSpace(discussionReplyContract)) {
		tail := sc
		if len(tail) > 300 {
			tail = tail[len(tail)-300:]
		}
		t.Errorf("SystemContext must END with the discussion reply contract; got tail: %q", tail)
	}
}

// TestBuildTaskOmitsDiscussionContractForBoardItems is the control: a
// board-sourced item keeps the plain final-comment append — the discussion
// variant must not leak into ordinary dispatches.
func TestBuildTaskOmitsDiscussionContractForBoardItems(t *testing.T) {
	snap := &api.ContextSnapshot{WorkItemRevision: "rev-1", GoalRevision: "1"}
	run, agent, project := ctxFixtures(snap)
	cl := fake.NewClientBuilder().WithScheme(dispatchScheme(t)).WithObjects(run, agent, project, dispatchTeamObj()).Build()
	d := &operatorDispatch{
		cfg:    OperatorDispatchConfig{Client: cl, ContextAssemblers: stubAssemblers{src: stubSources{wiRev: "rev-1", goalRev: "1"}}},
		source: fakeDispatchSource{title: "Fix the flake", body: "make test/flake green", fence: "7", source: "board"},
	}
	task, err := d.buildTask(context.Background(), ctxRunUID, ctxRunUID)
	if err != nil {
		t.Fatalf("buildTask: %v", err)
	}
	sc := task.Envelope.SystemContext
	if !strings.HasSuffix(sc, strings.TrimSpace(finalCommentContract)) {
		t.Error("board item must close with the plain final-comment contract")
	}
	if strings.Contains(sc, "How to post in the discussion room") {
		t.Error("discussion reply contract leaked into a board-sourced dispatch")
	}
}

// TestSqlDispatchSourceWorkItemSource pins the coord read behind the
// origin-aware append (ISI-7367): the source comes from coord.work_item, and
// a NULL column reads as the plain board default.
func TestSqlDispatchSourceWorkItemSource(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := sqlDispatchSource{db: db}
	const wi = "99999999-8888-7777-6666-555555555555"
	mock.ExpectQuery("SELECT source FROM coord.work_item").WithArgs(wi).
		WillReturnRows(sqlmock.NewRows([]string{"source"}).AddRow("discussion"))
	got, err := s.WorkItemSource(context.Background(), wi)
	if err != nil || got != "discussion" {
		t.Fatalf("WorkItemSource = %q, %v; want discussion, nil", got, err)
	}
	mock.ExpectQuery("SELECT source FROM coord.work_item").WithArgs(wi).
		WillReturnRows(sqlmock.NewRows([]string{"source"}).AddRow(nil))
	if got, err := s.WorkItemSource(context.Background(), wi); err != nil || got != "" {
		t.Fatalf("NULL source: WorkItemSource = %q, %v; want empty, nil", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
