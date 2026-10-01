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

package contextsource

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/K8squad/K8squad/pkg/contextasm"
)

// ISI-5279 WS-D: an ordinary board item carries no ksquad.github.issue label —
// GitHubDetails returns the zero value and never touches the mirror table.
func TestSourceGitHubDetailsNoLabel(t *testing.T) {
	s, mock := newSourceWithDB(t)
	mock.ExpectQuery(`unnest\(wi.labels\)`).
		WithArgs("wi-1", githubIssueLabelPrefix+"%").
		WillReturnRows(sqlmock.NewRows([]string{"l"})) // no rows → not a GitHub item

	gh, err := s.GitHubDetails(context.Background(), "proj-1", "wi-1")
	if err != nil {
		t.Fatalf("GitHubDetails: %v", err)
	}
	if !reflect.DeepEqual(gh, contextasm.GitHubDetails{}) {
		t.Errorf("expected zero value, got %+v", gh)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A labelled item whose issue IS mirrored surfaces ref/url/state/title/body and
// the last-sync observation. The mirror payload's own URL wins over the derived
// one; mirrored_at renders RFC3339.
func TestSourceGitHubDetailsMirrored(t *testing.T) {
	s, mock := newSourceWithDB(t)
	synced := time.Date(2026, 9, 30, 8, 15, 0, 0, time.UTC)
	mock.ExpectQuery(`unnest\(wi.labels\)`).
		WithArgs("wi-2", githubIssueLabelPrefix+"%").
		WillReturnRows(sqlmock.NewRows([]string{"l"}).AddRow("ksquad.github.issue=acme/widget#42"))
	mock.ExpectQuery(`FROM scm.mirror_record`).
		WithArgs("team-a", "proj-1", "42").
		WillReturnRows(sqlmock.NewRows([]string{"state", "title", "actor", "payload", "mirrored_at"}).
			AddRow("open", "Flaky cache", "octocat",
				[]byte(`{"body":"Cold caches flake the suite.","url":"https://github.com/acme/widget/issues/42"}`),
				synced))

	gh, err := s.GitHubDetails(context.Background(), "proj-1", "wi-2")
	if err != nil {
		t.Fatalf("GitHubDetails: %v", err)
	}
	if gh.IssueRef != "acme/widget#42" {
		t.Errorf("ref = %q", gh.IssueRef)
	}
	if gh.IssueURL != "https://github.com/acme/widget/issues/42" {
		t.Errorf("url = %q", gh.IssueURL)
	}
	if gh.State != "open" || gh.Title != "Flaky cache" || gh.Actor != "octocat" {
		t.Errorf("state/title/actor = %q/%q/%q", gh.State, gh.Title, gh.Actor)
	}
	if gh.Body != "Cold caches flake the suite." {
		t.Errorf("body = %q", gh.Body)
	}
	if gh.LastSyncedAt != "2026-09-30T08:15:00Z" {
		t.Errorf("lastSynced = %q", gh.LastSyncedAt)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// ISI-5308: a mirrored issue whose payload carries a comment thread surfaces the
// comments on GitHubDetails, newest-timestamp rendered RFC3339, so the assembler
// can emit each as an untrusted-external element.
func TestSourceGitHubDetailsMirroredComments(t *testing.T) {
	s, mock := newSourceWithDB(t)
	c1 := time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`unnest\(wi.labels\)`).
		WithArgs("wi-9", githubIssueLabelPrefix+"%").
		WillReturnRows(sqlmock.NewRows([]string{"l"}).AddRow("ksquad.github.issue=acme/widget#42"))
	mock.ExpectQuery(`FROM scm.mirror_record`).
		WithArgs("team-a", "proj-1", "42").
		WillReturnRows(sqlmock.NewRows([]string{"state", "title", "actor", "payload", "mirrored_at"}).
			AddRow("open", "Flaky cache", "octocat",
				[]byte(`{"body":"body","comments":[{"actor":"alice","body":"first","created_at":"2026-09-30T09:00:00Z"},{"actor":"bob","body":"second"}]}`),
				c1))

	gh, err := s.GitHubDetails(context.Background(), "proj-1", "wi-9")
	if err != nil {
		t.Fatalf("GitHubDetails: %v", err)
	}
	if len(gh.Comments) != 2 {
		t.Fatalf("comments = %d, want 2 (%+v)", len(gh.Comments), gh.Comments)
	}
	if gh.Comments[0].Author != "alice" || gh.Comments[0].Body != "first" || gh.Comments[0].WrittenAt != "2026-09-30T09:00:00Z" {
		t.Errorf("comment[0] wrong: %+v", gh.Comments[0])
	}
	if gh.Comments[1].Author != "bob" || gh.Comments[1].Body != "second" || gh.Comments[1].WrittenAt != "" {
		t.Errorf("comment[1] wrong (no timestamp → empty): %+v", gh.Comments[1])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A labelled item whose issue has NOT been mirrored yet still returns the ref +
// a URL derived from the label alone — the agent gets the link; body/state fill
// in once the mirror catches up.
func TestSourceGitHubDetailsLabelledButUnmirrored(t *testing.T) {
	s, mock := newSourceWithDB(t)
	mock.ExpectQuery(`unnest\(wi.labels\)`).
		WithArgs("wi-3", githubIssueLabelPrefix+"%").
		WillReturnRows(sqlmock.NewRows([]string{"l"}).AddRow("ksquad.github.issue=acme/widget#7"))
	mock.ExpectQuery(`FROM scm.mirror_record`).
		WithArgs("team-a", "proj-1", "7").
		WillReturnRows(sqlmock.NewRows([]string{"state", "title", "actor", "payload", "mirrored_at"})) // not mirrored yet

	gh, err := s.GitHubDetails(context.Background(), "proj-1", "wi-3")
	if err != nil {
		t.Fatalf("GitHubDetails: %v", err)
	}
	if gh.IssueRef != "acme/widget#7" || gh.IssueURL != "https://github.com/acme/widget/issues/7" {
		t.Errorf("ref/url = %q/%q", gh.IssueRef, gh.IssueURL)
	}
	if gh.State != "" || gh.Body != "" || gh.LastSyncedAt != "" {
		t.Errorf("expected empty mirror fields, got %+v", gh)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A label that matched the prefix but is not owner/repo#N is ignored (no bogus
// link, no mirror query).
func TestSourceGitHubDetailsMalformedLabel(t *testing.T) {
	s, mock := newSourceWithDB(t)
	mock.ExpectQuery(`unnest\(wi.labels\)`).
		WithArgs("wi-4", githubIssueLabelPrefix+"%").
		WillReturnRows(sqlmock.NewRows([]string{"l"}).AddRow("ksquad.github.issue=not-a-ref"))

	gh, err := s.GitHubDetails(context.Background(), "proj-1", "wi-4")
	if err != nil {
		t.Fatalf("GitHubDetails: %v", err)
	}
	if !reflect.DeepEqual(gh, contextasm.GitHubDetails{}) {
		t.Errorf("expected zero value for malformed label, got %+v", gh)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestParseGithubIssueLabel(t *testing.T) {
	cases := []struct {
		label                      string
		wantRef, wantRepo, wantNum string
		wantOK                     bool
	}{
		{"ksquad.github.issue=acme/widget#42", "acme/widget#42", "acme/widget", "42", true},
		{"ksquad.github.issue=org/sub.repo#1", "org/sub.repo#1", "org/sub.repo", "1", true},
		{"ksquad.github.issue=acme/widget#0", "acme/widget#0", "acme/widget", "0", true},
		{"other.label=x", "", "", "", false},                      // wrong prefix
		{"ksquad.github.issue=acme/widget", "", "", "", false},    // no number
		{"ksquad.github.issue=acme/widget#", "", "", "", false},   // empty number
		{"ksquad.github.issue=#42", "", "", "", false},            // empty repo
		{"ksquad.github.issue=widget#42", "", "", "", false},      // repo lacks owner/
		{"ksquad.github.issue=acme/widget#4x", "", "", "", false}, // non-numeric
	}
	for _, c := range cases {
		ref, repo, num, ok := parseGithubIssueLabel(c.label)
		if ok != c.wantOK || ref != c.wantRef || repo != c.wantRepo || num != c.wantNum {
			t.Errorf("parse(%q) = (%q,%q,%q,%v), want (%q,%q,%q,%v)",
				c.label, ref, repo, num, ok, c.wantRef, c.wantRepo, c.wantNum, c.wantOK)
		}
	}
}
