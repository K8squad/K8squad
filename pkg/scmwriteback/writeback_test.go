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

package scmwriteback

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/K8squad/K8squad/pkg/scm"
)

// fakeStore is an in-memory Store: a fixed pending set + a recorded-marker log.
type fakeStore struct {
	pending  []Pending
	listErr  error
	recErr   error
	recorded []markerCall
}

type markerCall struct{ workItemID, runID, note string }

func (f *fakeStore) PendingWriteBacks(_ context.Context, _ string) ([]Pending, error) {
	return f.pending, f.listErr
}

func (f *fakeStore) RecordWriteBack(_ context.Context, workItemID, runID, note string) error {
	if f.recErr != nil {
		return f.recErr
	}
	f.recorded = append(f.recorded, markerCall{workItemID, runID, note})
	return nil
}

// fakePoster records CreateComment calls and returns a scripted result/error.
type fakePoster struct {
	calls   []postCall
	id      string
	err     error
	perCall map[string]error // externalID -> error override
}

type postCall struct{ repoURL, kind, externalID, comment string }

func (f *fakePoster) CreateComment(_ context.Context, repoURL, kind, externalID, comment string) (string, error) {
	f.calls = append(f.calls, postCall{repoURL, kind, externalID, comment})
	if f.perCall != nil {
		if e, ok := f.perCall[externalID]; ok {
			return "", e
		}
	}
	if f.err != nil {
		return "", f.err
	}
	id := f.id
	if id == "" {
		id = "cmt-1"
	}
	return id, nil
}

const proj = "11111111-1111-1111-1111-111111111111"
const repoURL = "https://github.com/K8squad/K8squad"

func run(t *testing.T, store *fakeStore, poster CommentPoster) (Stats, error) {
	t.Helper()
	return NewEngine(store).WriteBackProject(context.Background(), proj, repoURL, poster)
}

func TestWriteBack_PostsSucceeded(t *testing.T) {
	store := &fakeStore{pending: []Pending{{
		WorkItemID: "wi-1", RunID: "run-1", IssueRef: "K8squad/K8squad#42",
		TerminalStep: "succeeded", AgentName: "Winston", Title: "GitHub issue K8squad/K8squad#42",
	}}}
	poster := &fakePoster{id: "gh-99"}

	stats, err := run(t, store, poster)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if stats.Posted != 1 || stats.Pending != 1 || stats.Skipped != 0 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want Posted:1 Pending:1", stats)
	}
	if len(poster.calls) != 1 {
		t.Fatalf("want 1 post, got %d", len(poster.calls))
	}
	c := poster.calls[0]
	if c.repoURL != repoURL || c.kind != "issue" || c.externalID != "42" {
		t.Fatalf("post target = %+v, want issue #42 on %s", c, repoURL)
	}
	if !strings.Contains(c.comment, "succeeded") || !strings.Contains(c.comment, "Winston") {
		t.Fatalf("comment missing outcome/agent: %q", c.comment)
	}
	if !strings.Contains(c.comment, "ADR-0013") {
		t.Fatalf("comment must carry the ADR-0013 honesty note: %q", c.comment)
	}
	if len(store.recorded) != 1 || store.recorded[0].note != "posted:gh-99" {
		t.Fatalf("marker = %+v, want posted:gh-99", store.recorded)
	}
	if store.recorded[0].workItemID != "wi-1" || store.recorded[0].runID != "run-1" {
		t.Fatalf("marker keyed wrong: %+v", store.recorded[0])
	}
}

func TestWriteBack_FailedRendersFailure(t *testing.T) {
	store := &fakeStore{pending: []Pending{{
		WorkItemID: "wi-1", RunID: "run-1", IssueRef: "K8squad/K8squad#7",
		TerminalStep: "failed", AgentName: "", Title: "",
	}}}
	poster := &fakePoster{}
	stats, err := run(t, store, poster)
	if err != nil || stats.Posted != 1 {
		t.Fatalf("stats=%+v err=%v, want Posted:1 no err", stats, err)
	}
	if !strings.Contains(poster.calls[0].comment, "failed") || !strings.Contains(poster.calls[0].comment, "an agent") {
		t.Fatalf("failed comment wrong: %q", poster.calls[0].comment)
	}
}

func TestWriteBack_CancelledIsQuietButMarked(t *testing.T) {
	store := &fakeStore{pending: []Pending{{
		WorkItemID: "wi-1", RunID: "run-1", IssueRef: "K8squad/K8squad#7", TerminalStep: "cancelled",
	}}}
	poster := &fakePoster{}
	stats, err := run(t, store, poster)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if stats.Posted != 0 || stats.Skipped != 1 {
		t.Fatalf("stats=%+v, want Posted:0 Skipped:1", stats)
	}
	if len(poster.calls) != 0 {
		t.Fatalf("cancelled must post nothing, got %d", len(poster.calls))
	}
	if len(store.recorded) != 1 || !strings.Contains(store.recorded[0].note, "cancelled") {
		t.Fatalf("cancelled must still be marked skipped: %+v", store.recorded)
	}
}

func TestWriteBack_CrossRepoSkipped(t *testing.T) {
	store := &fakeStore{pending: []Pending{{
		WorkItemID: "wi-1", RunID: "run-1", IssueRef: "someone/else#3", TerminalStep: "succeeded",
	}}}
	poster := &fakePoster{}
	stats, _ := run(t, store, poster)
	if stats.Posted != 0 || stats.Skipped != 1 || len(poster.calls) != 0 {
		t.Fatalf("cross-repo must skip without posting: stats=%+v calls=%d", stats, len(poster.calls))
	}
	if len(store.recorded) != 1 || !strings.Contains(store.recorded[0].note, "!=") {
		t.Fatalf("cross-repo must be marked: %+v", store.recorded)
	}
}

func TestWriteBack_UnparseableRefSkipped(t *testing.T) {
	store := &fakeStore{pending: []Pending{{
		WorkItemID: "wi-1", RunID: "run-1", IssueRef: "not-a-ref", TerminalStep: "succeeded",
	}}}
	poster := &fakePoster{}
	stats, _ := run(t, store, poster)
	if stats.Skipped != 1 || len(poster.calls) != 0 {
		t.Fatalf("unparseable ref must skip: stats=%+v", stats)
	}
}

func TestWriteBack_PermanentProviderErrorMarkedNotFailed(t *testing.T) {
	store := &fakeStore{pending: []Pending{{
		WorkItemID: "wi-1", RunID: "run-1", IssueRef: "K8squad/K8squad#42", TerminalStep: "succeeded",
	}}}
	poster := &fakePoster{err: &scm.ProviderError{HTTPCode: 404, Message: "issue gone"}}
	stats, err := run(t, store, poster)
	if err != nil {
		t.Fatalf("permanent error must not fail the pass: %v", err)
	}
	if stats.Skipped != 1 || stats.Failed != 0 {
		t.Fatalf("stats=%+v, want Skipped:1 Failed:0", stats)
	}
	if len(store.recorded) != 1 || !strings.Contains(store.recorded[0].note, "permanent") {
		t.Fatalf("permanent error must be marked so it never retries: %+v", store.recorded)
	}
}

// TestWriteBack_PermanentStatusesAllSkipped pins the ISI-4803 fix end-to-end:
// the production GitHubProvider now maps every CreateComment failure onto a
// *scm.ProviderError carrying the HTTP status (github.go classifyGitHubWriteError),
// so a real 404 (issue deleted/transferred), 403 (missing issues:write, archived
// repo, locked issue), 410 (gone), or 422 (unparseable repo URL / id) must be
// marked-and-skipped — never returned as a transient failure that requeues the
// repo-sync reconcile forever.
func TestWriteBack_PermanentStatusesAllSkipped(t *testing.T) {
	for _, code := range []int{403, 404, 410, 422} {
		store := &fakeStore{pending: []Pending{{
			WorkItemID: "wi-1", RunID: "run-1", IssueRef: "K8squad/K8squad#42", TerminalStep: "succeeded",
		}}}
		poster := &fakePoster{err: &scm.ProviderError{HTTPCode: code, Message: "permanent"}}
		stats, err := run(t, store, poster)
		if err != nil {
			t.Fatalf("code %d: permanent must not fail the pass: %v", code, err)
		}
		if stats.Skipped != 1 || stats.Failed != 0 {
			t.Fatalf("code %d: stats=%+v, want Skipped:1 Failed:0", code, stats)
		}
		if len(store.recorded) != 1 || !strings.Contains(store.recorded[0].note, "permanent") {
			t.Fatalf("code %d: must be marked so it never retries: %+v", code, store.recorded)
		}
	}
}

// TestWriteBack_TransientStatusesFailPass guards the other half of the
// classification: a rate limit (429), a 5xx, or a transport error (HTTPCode 0,
// what github.go emits for a timeout/DNS failure) must stay transient — fail
// the pass so the level-triggered reconcile retries, and never mark (which would
// silently drop a deliverable write-back).
func TestWriteBack_TransientStatusesFailPass(t *testing.T) {
	for _, code := range []int{0, 429, 500, 502, 503} {
		store := &fakeStore{pending: []Pending{{
			WorkItemID: "wi-1", RunID: "run-1", IssueRef: "K8squad/K8squad#42", TerminalStep: "succeeded",
		}}}
		poster := &fakePoster{err: &scm.ProviderError{HTTPCode: code, Message: "transient"}}
		stats, err := run(t, store, poster)
		if err == nil {
			t.Fatalf("code %d: transient must fail the pass for a retry", code)
		}
		if stats.Failed != 1 || stats.Posted != 0 {
			t.Fatalf("code %d: stats=%+v, want Failed:1", code, stats)
		}
		if len(store.recorded) != 0 {
			t.Fatalf("code %d: transient must NOT mark: %+v", code, store.recorded)
		}
	}
}

func TestWriteBack_TransientProviderErrorFailsPassNoMarker(t *testing.T) {
	store := &fakeStore{pending: []Pending{{
		WorkItemID: "wi-1", RunID: "run-1", IssueRef: "K8squad/K8squad#42", TerminalStep: "succeeded",
	}}}
	poster := &fakePoster{err: &scm.ProviderError{HTTPCode: 502, Message: "bad gateway"}}
	stats, err := run(t, store, poster)
	if err == nil {
		t.Fatal("transient error must fail the pass for a level-triggered retry")
	}
	if stats.Failed != 1 || stats.Posted != 0 {
		t.Fatalf("stats=%+v, want Failed:1", stats)
	}
	if len(store.recorded) != 0 {
		t.Fatalf("transient error must NOT mark (so the next pass retries): %+v", store.recorded)
	}
}

func TestWriteBack_MixedContinuesPastFailure(t *testing.T) {
	store := &fakeStore{pending: []Pending{
		{WorkItemID: "wi-a", RunID: "run-a", IssueRef: "K8squad/K8squad#1", TerminalStep: "succeeded"},
		{WorkItemID: "wi-b", RunID: "run-b", IssueRef: "K8squad/K8squad#2", TerminalStep: "succeeded"},
	}}
	poster := &fakePoster{perCall: map[string]error{"1": &scm.ProviderError{HTTPCode: 502, Message: "flake"}}}
	stats, err := run(t, store, poster)
	if err == nil {
		t.Fatal("want the transient failure surfaced")
	}
	if stats.Posted != 1 || stats.Failed != 1 {
		t.Fatalf("stats=%+v, want Posted:1 Failed:1 (kept going past #1)", stats)
	}
	// Only the successful item is marked.
	if len(store.recorded) != 1 || store.recorded[0].workItemID != "wi-b" {
		t.Fatalf("only wi-b should be marked: %+v", store.recorded)
	}
}

func TestWriteBack_EmptyAndNilSafe(t *testing.T) {
	if s, err := run(t, &fakeStore{}, &fakePoster{}); err != nil || s.Pending != 0 || s.Posted != 0 {
		t.Fatalf("empty pending must no-op: %+v %v", s, err)
	}
	var nilEngine *Engine
	if s, err := nilEngine.WriteBackProject(context.Background(), proj, repoURL, &fakePoster{}); err != nil || s.Posted != 0 {
		t.Fatalf("nil engine must no-op: %+v %v", s, err)
	}
	if s, err := NewEngine(nil).WriteBackProject(context.Background(), proj, repoURL, &fakePoster{}); err != nil || s.Posted != 0 {
		t.Fatalf("nil store must no-op: %+v %v", s, err)
	}
	if s, err := NewEngine(&fakeStore{pending: []Pending{{IssueRef: "a/b#1", TerminalStep: "succeeded"}}}).
		WriteBackProject(context.Background(), proj, repoURL, nil); err != nil || s.Posted != 0 {
		t.Fatalf("nil provider must no-op: %+v %v", s, err)
	}
}

func TestWriteBack_ListErrorSurfaced(t *testing.T) {
	store := &fakeStore{listErr: errors.New("db down")}
	if _, err := run(t, store, &fakePoster{}); err == nil {
		t.Fatal("a list error must fail the pass")
	}
}

func TestParseIssueRef(t *testing.T) {
	cases := []struct {
		in           string
		repo, number string
		ok           bool
	}{
		{"K8squad/K8squad#42", "K8squad/K8squad", "42", true},
		{"a/b#1", "a/b", "1", true},
		{"no-hash", "", "", false},
		{"noslash#5", "", "", false},
		{"a/b#", "", "", false},
		{"#5", "", "", false},
		{"a/b#5x", "", "", false},
	}
	for _, c := range cases {
		r, n, ok := parseIssueRef(c.in)
		if ok != c.ok || r != c.repo || n != c.number {
			t.Errorf("parseIssueRef(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, r, n, ok, c.repo, c.number, c.ok)
		}
	}
}

func TestRepoSlug(t *testing.T) {
	cases := map[string]string{
		"https://github.com/K8squad/K8squad":     "K8squad/K8squad",
		"https://github.com/K8squad/K8squad.git": "K8squad/K8squad",
		"https://github.com/o/r/":                "o/r",
		"https://gitlab.com/o/r/sub":             "o/r",
		"git@nohost":                             "",
		"":                                       "",
	}
	for in, want := range cases {
		if got := repoSlug(in); got != want {
			t.Errorf("repoSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderComment_OnlySucceededFailed(t *testing.T) {
	if renderComment(Pending{TerminalStep: "cancelled"}) != "" {
		t.Error("cancelled must render empty")
	}
	if renderComment(Pending{TerminalStep: "running"}) != "" {
		t.Error("non-terminal must render empty")
	}
	if got := renderComment(Pending{TerminalStep: "succeeded", AgentName: "X", Title: "T"}); !strings.Contains(got, "T") || !strings.Contains(got, "X") {
		t.Errorf("succeeded body missing agent/title: %q", got)
	}
}
