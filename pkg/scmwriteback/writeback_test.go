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
	"unicode/utf8"

	"github.com/K8squad/K8squad/pkg/scm"
)

// fakeStore is an in-memory Store: a fixed pending set (terminal + initial) + a
// recorded-marker log.
type fakeStore struct {
	pending        []Pending // terminal leg
	initialPending []Pending // initial leg
	listErr        error
	initialListErr error
	recErr         error
	recorded       []markerCall
}

type markerCall struct{ workItemID, runID, eventType, note string }

func (f *fakeStore) PendingWriteBacks(_ context.Context, _ string) ([]Pending, error) {
	return f.pending, f.listErr
}

func (f *fakeStore) PendingInitialWriteBacks(_ context.Context, _ string) ([]Pending, error) {
	return f.initialPending, f.initialListErr
}

func (f *fakeStore) RecordWriteBack(_ context.Context, workItemID, runID, eventType, note string) error {
	if f.recErr != nil {
		return f.recErr
	}
	f.recorded = append(f.recorded, markerCall{workItemID, runID, eventType, note})
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

func runInitial(t *testing.T, store *fakeStore, poster CommentPoster) (Stats, error) {
	t.Helper()
	return NewEngine(store).WriteBackInitialProject(context.Background(), proj, repoURL, poster)
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

// TestRenderComment_ReportsCreatedTickets is the ISI-4872 honesty check: a
// decomposition run's comment lists the created child tickets (identifiers +
// count) sourced from the audit log, never "artifacts stored in workspace".
func TestRenderComment_ReportsCreatedTickets(t *testing.T) {
	got := renderComment(Pending{
		TerminalStep: "succeeded", AgentName: "quill", Title: "Decompose the epic",
		CreatedItems: []CreatedItem{
			{ID: "child-a", Title: "Wire the ResolveMCP gate"},
			{ID: "child-b", Title: "Mint the run token"},
		},
	})
	if !strings.Contains(got, "Created 2 sub-ticket(s)") {
		t.Errorf("comment must report the created count: %q", got)
	}
	for _, want := range []string{"child-a", "child-b", "Wire the ResolveMCP gate", "Mint the run token"} {
		if !strings.Contains(got, want) {
			t.Errorf("comment must list created ticket %q: %q", want, got)
		}
	}
	if strings.Contains(got, "workspace") {
		t.Errorf("comment must not claim artifacts stored in workspace: %q", got)
	}
}

// TestRenderComment_NoCreatedTicketsStaysQuiet: an ordinary run that authored no
// sub-tickets carries no created-ticket section and no false "0 created" success
// claim — the comment is unchanged from the plain outcome line.
func TestRenderComment_NoCreatedTicketsStaysQuiet(t *testing.T) {
	got := renderComment(Pending{TerminalStep: "succeeded", AgentName: "coder", Title: "Fix the bug"})
	if strings.Contains(got, "sub-ticket") {
		t.Errorf("a run that created nothing must not mention sub-tickets: %q", got)
	}
}

// TestRenderComment_FailedPartialCreate: a run that created some children before
// failing reports the true partial set — never rounded up to success.
func TestRenderComment_FailedPartialCreate(t *testing.T) {
	got := renderComment(Pending{
		TerminalStep: "failed", AgentName: "quill", Title: "Decompose",
		CreatedItems: []CreatedItem{{ID: "child-a", Title: "One"}},
	})
	if !strings.Contains(got, "failed") {
		t.Errorf("partial-create comment must still report failure: %q", got)
	}
	if !strings.Contains(got, "Created 1 sub-ticket(s)") || !strings.Contains(got, "before the run ended") {
		t.Errorf("partial-create comment must report the true created set: %q", got)
	}
}

// ── initial-findings leg (ISI-5602, ADR-0029) ──────────────────────────────

// TestWriteBackInitial_PostsOnce: a run with a substantive initial note posts
// exactly one comment and records the DISTINCT `github_writeback_initial` marker
// (not the terminal one), keyed on (work_item, run).
func TestWriteBackInitial_PostsOnce(t *testing.T) {
	store := &fakeStore{initialPending: []Pending{{
		WorkItemID: "wi-1", RunID: "run-1", IssueRef: "K8squad/K8squad#42", Kind: KindInitial,
		AgentName: "Winston", Title: "Fix the reader guard",
		Body: "I plan to default the workspace PVC on create and guard the missing-PVC case in readerspec.",
	}}}
	poster := &fakePoster{id: "gh-1"}

	stats, err := runInitial(t, store, poster)
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
	if c.externalID != "42" || c.kind != "issue" {
		t.Fatalf("post target = %+v, want issue #42", c)
	}
	for _, want := range []string{"Initial findings", "Winston", "Fix the reader guard", "workspace PVC", "ADR-0013"} {
		if !strings.Contains(c.comment, want) {
			t.Fatalf("initial comment missing %q: %q", want, c.comment)
		}
	}
	if len(store.recorded) != 1 || store.recorded[0].eventType != markerInitial {
		t.Fatalf("marker = %+v, want one %q marker", store.recorded, markerInitial)
	}
	if store.recorded[0].workItemID != "wi-1" || store.recorded[0].runID != "run-1" || store.recorded[0].note != "posted:gh-1" {
		t.Fatalf("marker keyed/noted wrong: %+v", store.recorded[0])
	}
}

// TestWriteBackInitial_SkipsEmptyBody: an empty or below-floor body posts nothing
// but is still marked (with the initial marker) so the pending query stops
// re-considering it — the terminal leg still covers the issue.
func TestWriteBackInitial_SkipsEmptyBody(t *testing.T) {
	for name, body := range map[string]string{
		"empty":  "",
		"blank":  "   \n\t  ",
		"thin":   "too short",
		"floor-": strings.Repeat("x", initialBodyFloor-1),
	} {
		store := &fakeStore{initialPending: []Pending{{
			WorkItemID: "wi-1", RunID: "run-1", IssueRef: "K8squad/K8squad#7", Kind: KindInitial, Body: body,
		}}}
		poster := &fakePoster{}
		stats, err := runInitial(t, store, poster)
		if err != nil {
			t.Fatalf("%s: err: %v", name, err)
		}
		if stats.Posted != 0 || stats.Skipped != 1 || len(poster.calls) != 0 {
			t.Fatalf("%s: empty/thin body must skip without posting: stats=%+v calls=%d", name, stats, len(poster.calls))
		}
		if len(store.recorded) != 1 || store.recorded[0].eventType != markerInitial {
			t.Fatalf("%s: empty body must still be marked with the initial marker: %+v", name, store.recorded)
		}
	}
}

// TestWriteBackInitial_ScopedAndGuarded: the initial leg reuses the same
// cross-repo guard and permanent-error handling as terminal — a foreign repo or a
// permanent provider error is marked (with the initial marker), never posted or
// retried.
func TestWriteBackInitial_ScopedAndGuarded(t *testing.T) {
	good := Pending{WorkItemID: "wi-g", RunID: "run-g", IssueRef: "someone/else#3", Kind: KindInitial,
		Body: strings.Repeat("real finding text ", 5)}
	store := &fakeStore{initialPending: []Pending{good}}
	poster := &fakePoster{}
	stats, _ := runInitial(t, store, poster)
	if stats.Posted != 0 || stats.Skipped != 1 || len(poster.calls) != 0 {
		t.Fatalf("cross-repo initial must skip without posting: stats=%+v calls=%d", stats, len(poster.calls))
	}
	if len(store.recorded) != 1 || store.recorded[0].eventType != markerInitial || !strings.Contains(store.recorded[0].note, "!=") {
		t.Fatalf("cross-repo initial must be marked with the initial marker: %+v", store.recorded)
	}

	// Permanent provider error → skipped+marked, pass does not fail.
	store = &fakeStore{initialPending: []Pending{{
		WorkItemID: "wi-p", RunID: "run-p", IssueRef: "K8squad/K8squad#9", Kind: KindInitial,
		Body: strings.Repeat("real finding text ", 5),
	}}}
	poster = &fakePoster{err: &scm.ProviderError{HTTPCode: 404, Message: "issue gone"}}
	stats, err := runInitial(t, store, poster)
	if err != nil {
		t.Fatalf("permanent error must not fail the initial pass: %v", err)
	}
	if stats.Skipped != 1 || stats.Failed != 0 {
		t.Fatalf("stats=%+v, want Skipped:1 Failed:0", stats)
	}
	if len(store.recorded) != 1 || store.recorded[0].eventType != markerInitial {
		t.Fatalf("permanent-error initial must be marked with the initial marker: %+v", store.recorded)
	}
}

// TestWriteBackInitial_IndependentFromTerminal: the two legs post independently
// and record DISTINCT markers, so each fires exactly once per run and neither
// dedups the other.
func TestWriteBackInitial_IndependentFromTerminal(t *testing.T) {
	store := &fakeStore{
		pending: []Pending{{
			WorkItemID: "wi-1", RunID: "run-1", IssueRef: "K8squad/K8squad#42", TerminalStep: "succeeded", AgentName: "Winston",
		}},
		initialPending: []Pending{{
			WorkItemID: "wi-1", RunID: "run-1", IssueRef: "K8squad/K8squad#42", Kind: KindInitial, AgentName: "Winston",
			Body: strings.Repeat("initial finding text ", 4),
		}},
	}
	eng := NewEngine(store)

	is, err := eng.WriteBackInitialProject(context.Background(), proj, repoURL, &fakePoster{id: "gh-i"})
	if err != nil || is.Posted != 1 {
		t.Fatalf("initial leg: stats=%+v err=%v, want Posted:1", is, err)
	}
	ts, err := eng.WriteBackProject(context.Background(), proj, repoURL, &fakePoster{id: "gh-t"})
	if err != nil || ts.Posted != 1 {
		t.Fatalf("terminal leg: stats=%+v err=%v, want Posted:1", ts, err)
	}
	if len(store.recorded) != 2 {
		t.Fatalf("want 2 markers (one per leg), got %+v", store.recorded)
	}
	gotInitial, gotTerminal := false, false
	for _, m := range store.recorded {
		switch m.eventType {
		case markerInitial:
			gotInitial = true
		case markerTerminal:
			gotTerminal = true
		default:
			t.Fatalf("unexpected marker event_type %q", m.eventType)
		}
	}
	if !gotInitial || !gotTerminal {
		t.Fatalf("each leg must record its own distinct marker: %+v", store.recorded)
	}
}

func TestWriteBackInitial_ListErrorSurfaced(t *testing.T) {
	store := &fakeStore{initialListErr: errors.New("db down")}
	if _, err := runInitial(t, store, &fakePoster{}); err == nil {
		t.Fatal("a list error must fail the initial pass")
	}
}

func TestWriteBackInitial_NilSafe(t *testing.T) {
	var nilEngine *Engine
	if s, err := nilEngine.WriteBackInitialProject(context.Background(), proj, repoURL, &fakePoster{}); err != nil || s.Posted != 0 {
		t.Fatalf("nil engine must no-op: %+v %v", s, err)
	}
	if s, err := NewEngine(nil).WriteBackInitialProject(context.Background(), proj, repoURL, &fakePoster{}); err != nil || s.Posted != 0 {
		t.Fatalf("nil store must no-op: %+v %v", s, err)
	}
	if s, err := NewEngine(&fakeStore{initialPending: []Pending{{IssueRef: "a/b#1", Kind: KindInitial, Body: strings.Repeat("x ", 30)}}}).
		WriteBackInitialProject(context.Background(), proj, repoURL, nil); err != nil || s.Posted != 0 {
		t.Fatalf("nil provider must no-op: %+v %v", s, err)
	}
}

// TestSanitizeInitialBody pins the §5 sanitizer: secret redaction, control-char
// strip, substance floor, and length cap.
func TestSanitizeInitialBody(t *testing.T) {
	// Substance floor.
	if got := sanitizeInitialBody("short"); got != "" {
		t.Errorf("below-floor body must sanitize to empty, got %q", got)
	}

	// Secret redaction across shapes.
	secrets := []string{
		"token ghp_" + strings.Repeat("a", 36),
		"github_pat_" + strings.Repeat("B", 30),
		"xoxb-" + strings.Repeat("1", 20),
		"AKIA" + strings.Repeat("Z", 16),
		"Authorization: Bearer abcdef1234567890",
	}
	for _, sec := range secrets {
		body := "Here is a finding with a secret: " + sec + " and some more context text."
		got := sanitizeInitialBody(body)
		if strings.Contains(got, "ghp_") || strings.Contains(got, "github_pat_") ||
			strings.Contains(got, "xoxb-") || strings.Contains(got, "AKIAZZZ") ||
			strings.Contains(got, "abcdef1234567890") {
			t.Errorf("secret not redacted in %q -> %q", body, got)
		}
		if !strings.Contains(got, "«redacted»") {
			t.Errorf("expected a redaction marker in %q", got)
		}
	}

	// PEM private key block redaction.
	pem := "Context line.\n-----BEGIN RSA PRIVATE KEY-----\nMIIabc\nxyz\n-----END RSA PRIVATE KEY-----\nmore context after the block here."
	if got := sanitizeInitialBody(pem); strings.Contains(got, "PRIVATE KEY") || !strings.Contains(got, "«redacted»") {
		t.Errorf("PEM block not redacted: %q", got)
	}

	// Control-char strip (keep \n/\t, drop a NUL and a BEL).
	ctl := "line one with enough length to clear the floor\x00\x07\nline two"
	got := sanitizeInitialBody(ctl)
	if strings.ContainsRune(got, 0x00) || strings.ContainsRune(got, 0x07) {
		t.Errorf("control chars not stripped: %q", got)
	}
	if !strings.Contains(got, "line one") || !strings.Contains(got, "line two") {
		t.Errorf("newline/content must survive the strip: %q", got)
	}

	// Length cap + truncation marker, and valid UTF-8 after a multi-byte cut.
	long := strings.Repeat("é", initialBodyCap+500) // 2-byte rune, > cap in runes
	capped := sanitizeInitialBody(long)
	if !strings.Contains(capped, "truncated") {
		t.Errorf("over-cap body must carry a truncation marker: len=%d", len([]rune(capped)))
	}
	if !utf8.ValidString(capped) {
		t.Error("capped body must remain valid UTF-8 (no mid-rune cut)")
	}
}

// TestRenderInitialComment_EmptyWhenThin: renderInitialComment returns "" for a
// thin body (the caller then stays quiet and marks it skipped).
func TestRenderInitialComment_EmptyWhenThin(t *testing.T) {
	if got := renderInitialComment(Pending{Kind: KindInitial, Body: "tiny"}); got != "" {
		t.Errorf("thin initial body must render empty, got %q", got)
	}
	if got := renderInitialComment(Pending{Kind: KindInitial, AgentName: "", Body: strings.Repeat("real ", 20)}); !strings.Contains(got, "an agent") {
		t.Errorf("missing agent name must fall back to 'an agent': %q", got)
	}
}
