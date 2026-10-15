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
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/K8squad/K8squad/pkg/scm"
)

// fakePROpener is the PROpener test double: CreatePR/UpdatePRBranch/GetRepo
// behavior is injected per test.
type fakePROpener struct {
	createPR     func(head, base string) (string, error)
	updateBranch func(prNumber string) error
	getRepo      func() (*scm.Repository, error)

	created []string // "head→base" in call order
	updates int
}

func (f *fakePROpener) CreatePR(_ context.Context, _, head, base, _, _ string) (string, error) {
	f.created = append(f.created, head+"→"+base)
	if f.createPR != nil {
		return f.createPR(head, base)
	}
	return "42", nil
}

func (f *fakePROpener) UpdatePRBranch(_ context.Context, _, prNumber string) error {
	f.updates++
	if f.updateBranch != nil {
		return f.updateBranch(prNumber)
	}
	return nil
}

func (f *fakePROpener) GetRepo(_ context.Context, _ string) (*scm.Repository, error) {
	if f.getRepo != nil {
		return f.getRepo()
	}
	return &scm.Repository{DefaultBranch: "main"}, nil
}

// prSourceProvider adapts a fakePROpener onto the scm.SourceProvider parameter
// shape WriteBackPRsProject takes: it embeds the (nil) interface for the
// methods this leg never calls and delegates CreatePR/UpdatePRBranch/GetRepo
// explicitly — a second embedded struct would make GetRepo ambiguous.
type prSourceProvider struct {
	scm.SourceProvider
	fake *fakePROpener
}

func (p prSourceProvider) CreatePR(ctx context.Context, repoURL, head, base, title, body string) (string, error) {
	return p.fake.CreatePR(ctx, repoURL, head, base, title, body)
}

func (p prSourceProvider) UpdatePRBranch(ctx context.Context, repoURL, prNumber string) error {
	return p.fake.UpdatePRBranch(ctx, repoURL, prNumber)
}

func (p prSourceProvider) GetRepo(ctx context.Context, repoURL string) (*scm.Repository, error) {
	return p.fake.GetRepo(ctx, repoURL)
}

func prPending() PRPending {
	return PRPending{
		WorkItemID:   "11111111-1111-1111-1111-111111111111",
		RunID:        "22222222-2222-2222-2222-222222222222",
		IssueRef:     "owner/repo#7",
		SourceBranch: "agent/coder/wi-1",
		AgentName:    "coder-1",
		Title:        "Fix the thing",
		TeamID:       "squad-a",
	}
}

func TestWriteBackPRsProjectOpensAndMarks(t *testing.T) {
	store := &fakeStore{prPending: []PRPending{prPending()}}
	opener := &fakePROpener{}
	eng := NewEngine(store)

	stats, err := eng.WriteBackPRsProject(context.Background(), "proj", "https://github.com/owner/repo", prSourceProvider{fake: opener})
	if err != nil {
		t.Fatalf("WriteBackPRsProject: %v", err)
	}
	if stats.Pending != 1 || stats.Posted != 1 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want pending=1 posted=1 failed=0", stats)
	}
	if len(opener.created) != 1 || opener.created[0] != "agent/coder/wi-1→main" {
		t.Fatalf("created = %v, want one PR agent/coder/wi-1→main", opener.created)
	}
	if opener.updates != 1 {
		t.Fatalf("updates = %d, want 1 (post-open branch update)", opener.updates)
	}
	if len(store.recorded) != 1 || store.recorded[0].eventType != markerPR || !strings.HasPrefix(store.recorded[0].note, "opened-pr:42") {
		t.Fatalf("recorded = %+v, want one opened-pr:42 %s marker", store.recorded, markerPR)
	}
}

func TestWriteBackPRsProjectInertWithoutPROpener(t *testing.T) {
	store := &fakeStore{prPending: []PRPending{prPending()}}
	eng := NewEngine(store)
	// A provider that does not satisfy PROpener (GitLab today): the leg is a
	// silent no-op — no pending query, no error (fail-closed capability check).
	stats, err := eng.WriteBackPRsProject(context.Background(), "proj", "https://github.com/owner/repo", nil)
	if err != nil || stats != (Stats{}) {
		t.Fatalf("stats=%+v err=%v, want zero no-op", stats, err)
	}
}

func TestWriteBackPRsProjectCrossRepoGuard(t *testing.T) {
	p := prPending()
	p.IssueRef = "other/repo#7"
	store := &fakeStore{prPending: []PRPending{p}}
	opener := &fakePROpener{}
	eng := NewEngine(store)

	stats, err := eng.WriteBackPRsProject(context.Background(), "proj", "https://github.com/owner/repo", prSourceProvider{fake: opener})
	if err != nil {
		t.Fatalf("WriteBackPRsProject: %v", err)
	}
	if stats.Skipped != 1 || stats.Posted != 0 {
		t.Fatalf("stats = %+v, want skipped=1 posted=0", stats)
	}
	if len(opener.created) != 0 {
		t.Fatalf("opened a PR cross-repo: %v", opener.created)
	}
	if len(store.recorded) != 1 || !strings.Contains(store.recorded[0].note, "skipped:") {
		t.Fatalf("recorded = %+v, want one skipped marker", store.recorded)
	}
}

func TestWriteBackPRsProjectPermanentCreateErrorSkips(t *testing.T) {
	store := &fakeStore{prPending: []PRPending{prPending()}}
	opener := &fakePROpener{
		createPR: func(_, _ string) (string, error) {
			// 422: PR already exists for the branch (idempotent re-pass).
			return "", &scm.ProviderError{HTTPCode: http.StatusUnprocessableEntity, Message: "A pull request already exists"}
		},
	}
	eng := NewEngine(store)

	stats, err := eng.WriteBackPRsProject(context.Background(), "proj", "https://github.com/owner/repo", prSourceProvider{fake: opener})
	if err != nil {
		t.Fatalf("WriteBackPRsProject: %v", err)
	}
	if stats.Skipped != 1 || stats.Posted != 0 {
		t.Fatalf("stats = %+v, want skipped=1 posted=0", stats)
	}
	if len(store.recorded) != 1 || !strings.Contains(store.recorded[0].note, "skipped:") {
		t.Fatalf("recorded = %+v, want one skipped marker", store.recorded)
	}
}

func TestWriteBackPRsProjectTransientCreateErrorFailsPass(t *testing.T) {
	store := &fakeStore{prPending: []PRPending{prPending()}}
	opener := &fakePROpener{
		createPR: func(_, _ string) (string, error) {
			return "", &scm.ProviderError{HTTPCode: http.StatusTooManyRequests, Message: "rate limited"}
		},
	}
	eng := NewEngine(store)

	stats, err := eng.WriteBackPRsProject(context.Background(), "proj", "https://github.com/owner/repo", prSourceProvider{fake: opener})
	if err == nil {
		t.Fatal("expected transient error to fail the pass")
	}
	if stats.Failed != 1 || len(store.recorded) != 0 {
		t.Fatalf("stats = %+v recorded = %+v, want failed=1 and no marker (retry next pass)", stats, store.recorded)
	}
}

func TestWriteBackPRsProjectConflictRetriesThenEscalates(t *testing.T) {
	store := &fakeStore{prPending: []PRPending{prPending()}}
	opener := &fakePROpener{
		updateBranch: func(_ string) error {
			return &scm.ProviderError{HTTPCode: http.StatusConflict, Message: "merge conflict"}
		},
	}
	eng := NewEngine(store)

	stats, err := eng.WriteBackPRsProject(context.Background(), "proj", "https://github.com/owner/repo", prSourceProvider{fake: opener})
	if err != nil {
		t.Fatalf("hard conflict is a terminal disposition, not a pass failure: %v", err)
	}
	if stats.Posted != 1 {
		t.Fatalf("stats = %+v, want posted=1", stats)
	}
	if opener.updates != MaxPRConflictAttempts {
		t.Fatalf("updates = %d, want bounded %d attempts", opener.updates, MaxPRConflictAttempts)
	}
	if len(store.conflicts) != MaxPRConflictAttempts {
		t.Fatalf("conflict rows = %d, want %d", len(store.conflicts), MaxPRConflictAttempts)
	}
	if !strings.Contains(store.conflicts[MaxPRConflictAttempts-1], "escalated:") {
		t.Fatalf("last conflict note = %q, want escalation note", store.conflicts[MaxPRConflictAttempts-1])
	}
	if len(store.escalations) != 1 {
		t.Fatalf("escalations = %+v, want exactly one", store.escalations)
	}
	esc := store.escalations[0]
	if esc.SourceBranch != "agent/coder/wi-1" || esc.PRNumber != "42" || esc.Attempts != MaxPRConflictAttempts || esc.Squad != "squad-a" {
		t.Fatalf("escalation = %+v, missing branch/PR/attempts/squad", esc)
	}
}

func TestWriteBackPRsProjectConflictClearsOnRetry(t *testing.T) {
	store := &fakeStore{prPending: []PRPending{prPending()}}
	calls := 0
	opener := &fakePROpener{
		updateBranch: func(_ string) error {
			calls++
			if calls == 1 {
				return &scm.ProviderError{HTTPCode: http.StatusConflict, Message: "merge conflict"}
			}
			return nil // a concurrent merge landed between attempts; race cleared
		},
	}
	eng := NewEngine(store)

	if _, err := eng.WriteBackPRsProject(context.Background(), "proj", "https://github.com/owner/repo", prSourceProvider{fake: opener}); err != nil {
		t.Fatalf("WriteBackPRsProject: %v", err)
	}
	if opener.updates != 2 || len(store.conflicts) != 1 || len(store.escalations) != 0 {
		t.Fatalf("updates=%d conflicts=%v escalations=%v, want 2 updates, 1 conflict row, no escalation",
			opener.updates, store.conflicts, store.escalations)
	}
}

func TestWriteBackPRsProjectBaseOverrideAndFallback(t *testing.T) {
	p := prPending()
	p.BaseBranch = "team-main-x"
	store := &fakeStore{prPending: []PRPending{p}}
	opener := &fakePROpener{}
	eng := NewEngine(store)

	if _, err := eng.WriteBackPRsProject(context.Background(), "proj", "https://github.com/owner/repo", prSourceProvider{fake: opener}); err != nil {
		t.Fatalf("WriteBackPRsProject: %v", err)
	}
	if opener.created[0] != "agent/coder/wi-1→team-main-x" {
		t.Fatalf("base override not honored: %v", opener.created)
	}

	// GetRepo failure degrades to the pinned fallback, never fails the pass.
	p2 := prPending()
	p2.RunID = "33333333-3333-3333-3333-333333333333"
	store2 := &fakeStore{prPending: []PRPending{p2}}
	opener2 := &fakePROpener{getRepo: func() (*scm.Repository, error) { return nil, fmt.Errorf("boom") }}
	eng2 := NewEngine(store2)
	if _, err := eng2.WriteBackPRsProject(context.Background(), "proj", "https://github.com/owner/repo", prSourceProvider{fake: opener2}); err != nil {
		t.Fatalf("WriteBackPRsProject: %v", err)
	}
	if opener2.created[0] != "agent/coder/wi-1→main" {
		t.Fatalf("fallback base not used: %v", opener2.created)
	}
}
