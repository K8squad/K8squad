// githubstatus_issuelocal_test.go — ISI-5678: the GitHub-issue → local work-item
// join that powers the "treated card" treatment. Covers the anchor-label
// derivation, the runState mapping, and the honest attach/skip behaviour of
// attachIssueLocals (a miss or nil seam leaves the issue without a Local block —
// never fabricated).
package apiserver

import (
	"context"
	"testing"

	"github.com/K8squad/K8squad/pkg/coord"
)

func TestIssueAnchorLabel(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"canonical", "https://github.com/K8squad/K8squad/issues/5678", "ksquad.github.issue=K8squad/K8squad#5678"},
		{"preserves casing", "https://github.com/Owner/Repo/issues/7", "ksquad.github.issue=Owner/Repo#7"},
		{"trailing path", "https://github.com/o/r/issues/42#issuecomment-1", "ksquad.github.issue=o/r#42"},
		{"not an issue url (PR)", "https://github.com/o/r/pull/9", ""},
		{"empty", "", ""},
		{"garbage", "not-a-url", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := issueAnchorLabel(tc.url); got != tc.want {
				t.Fatalf("issueAnchorLabel(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}

func TestIssueRunState(t *testing.T) {
	cases := map[string]string{
		"done":        "done",
		"in_progress": "running",
		"todo":        "todo",
		"blocked":     "todo", // unknown/adjacent degrades to the honest "todo"
		"":            "todo",
	}
	for in, want := range cases {
		if got := issueRunState(in); got != want {
			t.Fatalf("issueRunState(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeIssueLocalFinder is an in-memory IssueLocalFinder keyed on the anchor label.
type fakeIssueLocalFinder struct {
	byLabel map[string]coord.IssueLocalRecord
}

func (f *fakeIssueLocalFinder) FindIssueLocalByLabel(_ context.Context, _, _, label string) (coord.IssueLocalRecord, error) {
	if rec, ok := f.byLabel[label]; ok {
		return rec, nil
	}
	return coord.IssueLocalRecord{}, coord.ErrWorkItemNotFound
}

func TestAttachIssueLocals_JoinAndHonestAbsence(t *testing.T) {
	finder := &fakeIssueLocalFinder{byLabel: map[string]coord.IssueLocalRecord{
		"ksquad.github.issue=o/r#1": {WorkItemID: "wi-1", State: "in_progress", Assignee: "Alfred"},
		"ksquad.github.issue=o/r#2": {WorkItemID: "wi-2", State: "todo", Assignee: ""}, // unclaimed
	}}
	s := (&GithubStatusService{}).WithIssueLocals(finder)

	out := &GithubStatus{Issues: []GithubIssue{
		{Number: 1, URL: "https://github.com/o/r/issues/1"}, // bridged + claimed
		{Number: 2, URL: "https://github.com/o/r/issues/2"}, // bridged + unclaimed
		{Number: 3, URL: "https://github.com/o/r/issues/3"}, // not bridged (miss)
		{Number: 4, URL: "not-a-github-url"},                // unparseable → skipped
	}}
	s.attachIssueLocals(context.Background(), out, "", "proj-uid")

	if got := out.Issues[0].Local; got == nil || got.WorkItemID != "wi-1" || got.Agent != "Alfred" || got.RunState != "running" {
		t.Fatalf("issue#1 Local = %+v, want {wi-1 Alfred running}", got)
	}
	// Unclaimed bridge: Local IS attached with Agent=="" — the backend reports the
	// honest state; the frontend honesty guard (localRunBadge) decides treatment.
	if got := out.Issues[1].Local; got == nil || got.Agent != "" || got.RunState != "todo" {
		t.Fatalf("issue#2 Local = %+v, want {wi-2 \"\" todo}", got)
	}
	if out.Issues[2].Local != nil {
		t.Fatalf("issue#3 (miss) must have no Local block, got %+v", out.Issues[2].Local)
	}
	if out.Issues[3].Local != nil {
		t.Fatalf("issue#4 (unparseable url) must have no Local block, got %+v", out.Issues[3].Local)
	}
}
