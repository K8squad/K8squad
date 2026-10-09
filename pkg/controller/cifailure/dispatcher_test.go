package cifailure

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/K8squad/K8squad/pkg/scm"
)

// --- fakes -------------------------------------------------------------------

type fakePolicy struct {
	pol *Policy
	err error
}

func (f fakePolicy) CIFailurePolicy(context.Context, string, string) (*Policy, error) {
	return f.pol, f.err
}

type fakeStore struct {
	seen map[string]bool // labels already created (idempotency)
	reqs []FailureRequest
	err  error
}

func newFakeStore() *fakeStore { return &fakeStore{seen: map[string]bool{}} }

func (f *fakeStore) EnsureCIFailure(_ context.Context, req FailureRequest) (bool, error) {
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return false, f.err
	}
	if f.seen[req.DedupLabel] {
		return false, nil
	}
	f.seen[req.DedupLabel] = true
	return true, nil
}

// createdLabels returns the distinct dedup labels the store actually inserted
// (Created=true), which is what dedup/re-trigger assertions turn on.
func (f *fakeStore) createdLabels() map[string]bool { return f.seen }

var enabledAt = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

// checkRow builds a check_run mirror row with the given conclusion, head SHA,
// head branch and completion time.
func checkRow(externalID, checkName, conclusion, headSHA, headRef string, completedAt time.Time) scm.MirrorRow {
	payload, _ := json.Marshal(scm.MirrorPayload{
		Conclusion: conclusion,
		HeadSHA:    headSHA,
		HeadRef:    headRef,
		UpdatedAt:  completedAt,
	})
	return scm.MirrorRow{
		Kind:       scm.RecordTypeCheckRun,
		ExternalID: externalID,
		State:      "completed",
		Title:      checkName,
		Payload:    payload,
	}
}

func enabledPolicy() *Policy {
	return &Policy{
		Enabled:     true,
		AgentID:     "agent-triage",
		Conclusions: []string{"failure"},
		EnabledBy:   "user:henrik",
		EnabledAt:   enabledAt,
		ProjectID:   "proj-1",
		TeamID:      "team-1",
	}
}

func run(t *testing.T, d *Dispatcher, rows []scm.MirrorRow) error {
	t.Helper()
	return d.HandleCIFailures(context.Background(), "ns", "proj", nil, "https://github.com/acme/app", rows)
}

// after/at the watermark so the forward-only gate passes by default.
var afterEnabled = enabledAt.Add(time.Hour)

// --- tests -------------------------------------------------------------------

func TestDisabledOrUnconfiguredIsNoop(t *testing.T) {
	for _, pol := range []*Policy{nil, {Enabled: false, AgentID: "a"}} {
		store := newFakeStore()
		d := &Dispatcher{Policy: fakePolicy{pol: pol}, Store: store}
		if err := run(t, d, []scm.MirrorRow{checkRow("1", "ci", "failure", "sha1", "", afterEnabled)}); err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if len(store.reqs) != 0 {
			t.Fatalf("disabled policy should never touch the store, got %d reqs", len(store.reqs))
		}
	}
}

func TestEnabledButNoAgentFails(t *testing.T) {
	pol := enabledPolicy()
	pol.AgentID = ""
	d := &Dispatcher{Policy: fakePolicy{pol: pol}, Store: newFakeStore()}
	if err := run(t, d, nil); err == nil {
		t.Fatal("expected error for enabled policy with empty AgentID")
	}
}

// Qualifies on failure only: a success/neutral/in-progress run never triages;
// only a conclusion in the configured set does.
func TestQualifiesOnFailureOnly(t *testing.T) {
	store := newFakeStore()
	d := &Dispatcher{Policy: fakePolicy{pol: enabledPolicy()}, Store: store}

	rows := []scm.MirrorRow{
		checkRow("1", "unit", "success", "shaA", "", afterEnabled),
		checkRow("2", "lint", "neutral", "shaA", "", afterEnabled),
		checkRow("3", "build", "", "shaA", "", afterEnabled), // still running, no conclusion
		checkRow("4", "e2e", "failure", "shaA", "", afterEnabled),
		// a non-check row must be ignored entirely
		{Kind: scm.RecordTypePR, ExternalID: "9", State: "open", Title: "pr"},
	}
	if err := run(t, d, rows); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(store.createdLabels()) != 1 {
		t.Fatalf("expected exactly one triage (the failure), got %d: %+v", len(store.createdLabels()), store.reqs)
	}
	// the one request is the e2e failure, titled with the short SHA
	var got *FailureRequest
	for i := range store.reqs {
		if store.reqs[i].CheckName == "e2e" {
			got = &store.reqs[i]
		}
	}
	if got == nil {
		t.Fatal("expected the e2e failure to be dispatched")
	}
	if got.Conclusion != "failure" || got.HeadSHA != "shaA" {
		t.Fatalf("wrong request fields: %+v", got)
	}
	if got.Title != "Analyze CI failure: e2e @ shaA" {
		t.Fatalf("unexpected title %q", got.Title)
	}
	if got.Principal != "user:henrik" {
		t.Fatalf("expected EnabledBy principal, got %q", got.Principal)
	}
}

// Optional conclusions: timed_out / cancelled qualify only when configured.
func TestConfiguredConclusionsWiden(t *testing.T) {
	pol := enabledPolicy()
	pol.Conclusions = []string{"failure", "timed_out"}
	store := newFakeStore()
	d := &Dispatcher{Policy: fakePolicy{pol: pol}, Store: store}

	rows := []scm.MirrorRow{
		checkRow("1", "a", "timed_out", "shaA", "", afterEnabled),
		checkRow("2", "b", "cancelled", "shaA", "", afterEnabled), // NOT configured ⇒ skip
		checkRow("3", "c", "FAILURE", "shaA", "", afterEnabled),   // case-insensitive
	}
	if err := run(t, d, rows); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(store.createdLabels()) != 2 {
		t.Fatalf("expected timed_out + failure to qualify (not cancelled), got %d: %+v", len(store.createdLabels()), store.reqs)
	}
}

// Dedups re-runs on the same SHA: running the same failing snapshot twice (a
// redelivered webhook / poll tick) creates exactly one triage item.
func TestDedupsReRunsOnSameSHA(t *testing.T) {
	store := newFakeStore()
	d := &Dispatcher{Policy: fakePolicy{pol: enabledPolicy()}, Store: store}
	rows := []scm.MirrorRow{checkRow("1", "ci", "failure", "shaA", "", afterEnabled)}

	for i := 0; i < 3; i++ {
		if err := run(t, d, rows); err != nil {
			t.Fatalf("pass %d: unexpected err: %v", i, err)
		}
	}
	if len(store.reqs) != 3 {
		t.Fatalf("store should be asked every pass, got %d", len(store.reqs))
	}
	if len(store.createdLabels()) != 1 {
		t.Fatalf("expected exactly one item created across re-runs, got %d", len(store.createdLabels()))
	}
}

// A new failing SHA re-triggers: a fresh commit that fails the same check yields
// a distinct dedup label and thus a new triage item.
func TestNewFailingSHARetriggers(t *testing.T) {
	store := newFakeStore()
	d := &Dispatcher{Policy: fakePolicy{pol: enabledPolicy()}, Store: store}

	if err := run(t, d, []scm.MirrorRow{checkRow("1", "ci", "failure", "shaA", "", afterEnabled)}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if err := run(t, d, []scm.MirrorRow{checkRow("1", "ci", "failure", "shaB", "", afterEnabled)}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(store.createdLabels()) != 2 {
		t.Fatalf("a new failing SHA should re-trigger, got %d distinct labels", len(store.createdLabels()))
	}
	// And a different check on the same SHA is also distinct.
	if err := run(t, d, []scm.MirrorRow{checkRow("2", "other", "failure", "shaB", "", afterEnabled)}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(store.createdLabels()) != 3 {
		t.Fatalf("a different check on the same SHA should be distinct, got %d", len(store.createdLabels()))
	}
}

// Forward-only (D4): a failure that COMPLETED before the EnabledAt watermark is
// never triaged, so enabling the policy does not retroactively fire on the
// backlog already in the mirror.
func TestForwardOnlyFromEnabledAt(t *testing.T) {
	store := newFakeStore()
	d := &Dispatcher{Policy: fakePolicy{pol: enabledPolicy()}, Store: store}

	beforeEnabled := enabledAt.Add(-time.Hour)
	rows := []scm.MirrorRow{
		checkRow("1", "old", "failure", "shaOld", "", beforeEnabled), // pre-watermark ⇒ skip
		checkRow("2", "new", "failure", "shaNew", "", afterEnabled),  // post-watermark ⇒ triage
		checkRow("3", "untimed", "failure", "shaX", "", time.Time{}), // no timestamp ⇒ skip (can't prove recency)
	}
	if err := run(t, d, rows); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(store.createdLabels()) != 1 {
		t.Fatalf("only the post-watermark failure should triage, got %d: %+v", len(store.createdLabels()), store.reqs)
	}
	if store.reqs[0].CheckName != "new" {
		t.Fatalf("expected the post-watermark 'new' failure, got %q", store.reqs[0].CheckName)
	}
}

// A run completed exactly AT the watermark triages (inclusive lower bound).
func TestWatermarkInclusive(t *testing.T) {
	store := newFakeStore()
	d := &Dispatcher{Policy: fakePolicy{pol: enabledPolicy()}, Store: store}
	if err := run(t, d, []scm.MirrorRow{checkRow("1", "ci", "failure", "shaA", "", enabledAt)}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(store.createdLabels()) != 1 {
		t.Fatalf("a run completed exactly at the watermark should triage, got %d", len(store.createdLabels()))
	}
}

// BranchFilter narrows by head branch when set; empty accepts every ref.
func TestBranchFilter(t *testing.T) {
	pol := enabledPolicy()
	pol.BranchFilter = []string{"main"}
	store := newFakeStore()
	d := &Dispatcher{Policy: fakePolicy{pol: pol}, Store: store}

	rows := []scm.MirrorRow{
		checkRow("1", "a", "failure", "shaA", "main", afterEnabled),    // matches
		checkRow("2", "b", "failure", "shaB", "feature", afterEnabled), // filtered out
		checkRow("3", "c", "failure", "shaC", "", afterEnabled),        // unknown branch, filter set ⇒ skip
	}
	if err := run(t, d, rows); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(store.createdLabels()) != 1 {
		t.Fatalf("only the main-branch failure should triage, got %d: %+v", len(store.createdLabels()), store.reqs)
	}
	if store.reqs[0].CheckName != "a" {
		t.Fatalf("expected the main-branch check, got %q", store.reqs[0].CheckName)
	}
}

// A store error fails the whole pass so the next level-triggered reconcile retries.
func TestStoreErrorFailsPass(t *testing.T) {
	store := newFakeStore()
	store.err = errors.New("boom")
	d := &Dispatcher{Policy: fakePolicy{pol: enabledPolicy()}, Store: store}
	if err := run(t, d, []scm.MirrorRow{checkRow("1", "ci", "failure", "shaA", "", afterEnabled)}); err == nil {
		t.Fatal("expected the pass to fail when the store errors")
	}
}

// A policy-read error fails the pass.
func TestPolicyErrorFailsPass(t *testing.T) {
	d := &Dispatcher{Policy: fakePolicy{err: errors.New("db down")}, Store: newFakeStore()}
	if err := run(t, d, nil); err == nil {
		t.Fatal("expected policy read error to fail the pass")
	}
}
