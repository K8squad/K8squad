package issuetrigger

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/K8squad/K8squad/pkg/scm"
)

const (
	testRepoURL  = "https://github.com/Acme/Widget"
	testProject  = "proj-uid"
	testTeam     = "team-uid"
	testAgent    = "triage-bot"
	testEnabler  = "user:henrik"
	testIssueURL = "https://github.com/Acme/Widget/issues/42"
)

// enabledAt is the forward-only watermark used across tests.
var enabledAt = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func basePolicy() *Policy {
	return &Policy{
		Enabled:        true,
		TriageAgentID:  testAgent,
		OnlyUnassigned: true,
		EnabledBy:      testEnabler,
		EnabledAt:      enabledAt,
		ProjectID:      testProject,
		TeamID:         testTeam,
	}
}

func issueRow(t *testing.T, state string, p scm.MirrorPayload) scm.MirrorRow {
	t.Helper()
	// default a fresh created/updated (after watermark) unless set in p.
	if p.CreatedAt.IsZero() {
		p.CreatedAt = enabledAt.Add(time.Hour)
	}
	if p.UpdatedAt.IsZero() {
		p.UpdatedAt = p.CreatedAt
	}
	if p.URL == "" {
		p.URL = testIssueURL
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return scm.MirrorRow{
		Kind:       scm.RecordTypeIssue,
		ExternalID: "42",
		State:      state,
		Title:      "Something is broken",
		Payload:    raw,
	}
}

// recordingStore captures EnsureTriage calls and (optionally) simulates the real
// store's create-if-absent + dispatch-only-while-backlog convergence so a replay
// test can assert exactly-once dispatch.
type recordingStore struct {
	calls      []TriageRequest
	dispatched map[string]bool // label → already dispatched (simulates backlog guard)
}

func newRecordingStore() *recordingStore {
	return &recordingStore{dispatched: map[string]bool{}}
}

func (s *recordingStore) EnsureTriage(_ context.Context, req TriageRequest) (bool, error) {
	s.calls = append(s.calls, req)
	if s.dispatched[req.DedupLabel] {
		return false, nil // already handled: no re-dispatch (past backlog)
	}
	s.dispatched[req.DedupLabel] = true
	return true, nil
}

func (s *recordingStore) dispatchCount() int {
	n := 0
	for _, v := range s.dispatched {
		if v {
			n++
		}
	}
	return n
}

func run(t *testing.T, pol *Policy, store TriageItemStore, rows ...scm.MirrorRow) error {
	t.Helper()
	d := &Dispatcher{Policy: staticPolicy{pol}, Store: store}
	return d.TriageChanges(context.Background(), "ns", "name", nil, testRepoURL, rows)
}

type staticPolicy struct{ p *Policy }

func (s staticPolicy) TriagePolicy(context.Context, string, string) (*Policy, error) {
	return s.p, nil
}

func TestQualifiesAndDispatches(t *testing.T) {
	store := newRecordingStore()
	if err := run(t, basePolicy(), store, issueRow(t, "open", scm.MirrorPayload{})); err != nil {
		t.Fatalf("TriageChanges: %v", err)
	}
	if len(store.calls) != 1 {
		t.Fatalf("want 1 EnsureTriage call, got %d", len(store.calls))
	}
	got := store.calls[0]
	if got.DedupLabel != "ksquad.github.issue=Acme/Widget#42" {
		t.Errorf("dedup label = %q", got.DedupLabel)
	}
	if got.TriageAgentID != testAgent || got.Principal != testEnabler {
		t.Errorf("agent/principal = %q/%q", got.TriageAgentID, got.Principal)
	}
	if got.ProjectID != testProject || got.TeamID != testTeam {
		t.Errorf("scope = %q/%q", got.ProjectID, got.TeamID)
	}
	if !strings.Contains(got.Title, "#42") {
		t.Errorf("title = %q", got.Title)
	}
}

func TestSkipsNonIssueAndClosed(t *testing.T) {
	store := newRecordingStore()
	pr := scm.MirrorRow{Kind: scm.RecordTypePR, ExternalID: "7", State: "open"}
	check := scm.MirrorRow{Kind: scm.RecordTypeCheckRun, ExternalID: "c1", State: "completed"}
	closed := issueRow(t, "closed", scm.MirrorPayload{})
	if err := run(t, basePolicy(), store, pr, check, closed); err != nil {
		t.Fatalf("TriageChanges: %v", err)
	}
	if len(store.calls) != 0 {
		t.Fatalf("want 0 calls for non-issue/closed rows, got %d", len(store.calls))
	}
}

func TestForwardOnly(t *testing.T) {
	// created AND updated before the watermark ⇒ skipped (backlog).
	old := scm.MirrorPayload{CreatedAt: enabledAt.Add(-48 * time.Hour), UpdatedAt: enabledAt.Add(-24 * time.Hour)}
	// created before but updated after ⇒ qualifies (fresh activity).
	touched := scm.MirrorPayload{CreatedAt: enabledAt.Add(-48 * time.Hour), UpdatedAt: enabledAt.Add(time.Minute)}

	store := newRecordingStore()
	if err := run(t, basePolicy(), store, issueRow(t, "open", old)); err != nil {
		t.Fatal(err)
	}
	if len(store.calls) != 0 {
		t.Fatalf("pre-watermark issue should be skipped, got %d calls", len(store.calls))
	}

	store2 := newRecordingStore()
	if err := run(t, basePolicy(), store2, issueRow(t, "open", touched)); err != nil {
		t.Fatal(err)
	}
	if len(store2.calls) != 1 {
		t.Fatalf("updated-after-watermark issue should qualify, got %d calls", len(store2.calls))
	}
}

func TestForwardOnlyZeroTimestampsFailSafe(t *testing.T) {
	// A row whose timestamps never got captured must NOT be treated as fresh.
	raw, _ := json.Marshal(scm.MirrorPayload{URL: testIssueURL}) // zero Created/Updated
	row := scm.MirrorRow{Kind: scm.RecordTypeIssue, ExternalID: "42", State: "open", Payload: raw}
	store := newRecordingStore()
	if err := run(t, basePolicy(), store, row); err != nil {
		t.Fatal(err)
	}
	if len(store.calls) != 0 {
		t.Fatalf("zero-timestamp issue should fail safe (skip), got %d calls", len(store.calls))
	}
}

func TestOnlyUnassigned(t *testing.T) {
	assigned := scm.MirrorPayload{Assignees: []string{"someone"}}

	store := newRecordingStore()
	if err := run(t, basePolicy(), store, issueRow(t, "open", assigned)); err != nil {
		t.Fatal(err)
	}
	if len(store.calls) != 0 {
		t.Fatalf("onlyUnassigned should skip assigned issue, got %d calls", len(store.calls))
	}

	pol := basePolicy()
	pol.OnlyUnassigned = false
	store2 := newRecordingStore()
	if err := run(t, pol, store2, issueRow(t, "open", assigned)); err != nil {
		t.Fatal(err)
	}
	if len(store2.calls) != 1 {
		t.Fatalf("onlyUnassigned=false should triage assigned issue, got %d calls", len(store2.calls))
	}
}

func TestLabelFilter(t *testing.T) {
	pol := basePolicy()
	pol.LabelFilter = []string{"bug", "Needs-Triage"}

	// no matching label ⇒ skip.
	store := newRecordingStore()
	if err := run(t, pol, store, issueRow(t, "open", scm.MirrorPayload{Labels: []string{"enhancement"}})); err != nil {
		t.Fatal(err)
	}
	if len(store.calls) != 0 {
		t.Fatalf("non-matching label should skip, got %d calls", len(store.calls))
	}

	// matching label, case-insensitive ⇒ qualify.
	store2 := newRecordingStore()
	if err := run(t, pol, store2, issueRow(t, "open", scm.MirrorPayload{Labels: []string{"NEEDS-TRIAGE"}})); err != nil {
		t.Fatal(err)
	}
	if len(store2.calls) != 1 {
		t.Fatalf("matching label (case-insensitive) should qualify, got %d calls", len(store2.calls))
	}
}

func TestNoDoubleDispatchOnReplay(t *testing.T) {
	store := newRecordingStore()
	pol := basePolicy()
	row := issueRow(t, "open", scm.MirrorPayload{})

	for i := 0; i < 3; i++ { // three level-triggered passes over the same snapshot
		if err := run(t, pol, store, row); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if store.dispatchCount() != 1 {
		t.Fatalf("replay must dispatch exactly once, got %d", store.dispatchCount())
	}
	if len(store.calls) != 3 {
		t.Fatalf("EnsureTriage should be called every pass (dedup is store-side), got %d", len(store.calls))
	}
}

func TestDisabledAndMisconfigured(t *testing.T) {
	store := newRecordingStore()
	// nil policy ⇒ no-op.
	if err := run(t, nil, store, issueRow(t, "open", scm.MirrorPayload{})); err != nil {
		t.Fatalf("nil policy: %v", err)
	}
	// disabled ⇒ no-op.
	pol := basePolicy()
	pol.Enabled = false
	if err := run(t, pol, store, issueRow(t, "open", scm.MirrorPayload{})); err != nil {
		t.Fatalf("disabled policy: %v", err)
	}
	if len(store.calls) != 0 {
		t.Fatalf("disabled/nil policy must not dispatch, got %d calls", len(store.calls))
	}

	// enabled but no agent ⇒ loud error.
	bad := basePolicy()
	bad.TriageAgentID = ""
	if err := run(t, bad, store, issueRow(t, "open", scm.MirrorPayload{})); err == nil {
		t.Fatal("enabled policy with empty TriageAgentID must error")
	}
}

func TestDeriveIssueRefFallbackToRepoSlug(t *testing.T) {
	// payload has no URL ⇒ derive from repoURL slug + ExternalID.
	raw, _ := json.Marshal(scm.MirrorPayload{CreatedAt: enabledAt.Add(time.Hour)})
	row := scm.MirrorRow{Kind: scm.RecordTypeIssue, ExternalID: "99", State: "open", Payload: raw}
	store := newRecordingStore()
	if err := run(t, basePolicy(), store, row); err != nil {
		t.Fatal(err)
	}
	if len(store.calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(store.calls))
	}
	if store.calls[0].DedupLabel != "ksquad.github.issue=Acme/Widget#99" {
		t.Errorf("fallback dedup label = %q", store.calls[0].DedupLabel)
	}
}
