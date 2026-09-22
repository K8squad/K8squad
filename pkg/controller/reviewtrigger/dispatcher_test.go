package reviewtrigger

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/K8squad/K8squad/pkg/scm"
)

// --- fakes -------------------------------------------------------------------

type fakePolicy struct {
	pol *Policy
	err error
}

func (f fakePolicy) ReviewPolicy(context.Context, string, string) (*Policy, error) {
	return f.pol, f.err
}

type fakeMembers struct {
	agents map[string]bool
	err    error
	calls  int
}

func (f *fakeMembers) IsTeamAgent(_ context.Context, _ string, actor string) (bool, error) {
	f.calls++
	if f.err != nil {
		return false, f.err
	}
	return f.agents[actor], nil
}

type fakeStore struct {
	seen   map[string]bool // labels already created (idempotency)
	reqs   []ReviewRequest
	err    error
	failOn string
}

func newFakeStore() *fakeStore { return &fakeStore{seen: map[string]bool{}} }

func (f *fakeStore) EnsureReview(_ context.Context, req ReviewRequest) (bool, error) {
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return false, f.err
	}
	if f.failOn != "" && req.PRNumber == f.failOn {
		return false, errors.New("boom")
	}
	if f.seen[req.DedupLabel] {
		return false, nil
	}
	f.seen[req.DedupLabel] = true
	return true, nil
}

func prRow(number, state, actor, headSHA string) scm.MirrorRow {
	payload, _ := json.Marshal(scm.MirrorPayload{HeadSHA: headSHA})
	return scm.MirrorRow{
		Kind:       scm.RecordTypePR,
		ExternalID: number,
		State:      state,
		Actor:      actor,
		Title:      "Some change",
		Payload:    payload,
	}
}

func enabledPolicy() *Policy {
	return &Policy{
		Enabled:         true,
		ReviewerAgentID: "agent-reviewer",
		Scope:           scopeAll,
		Trigger:         triggerOnNewCommits,
		EnabledBy:       "user:henrik",
		ProjectID:       "proj-1",
		TeamID:          "team-1",
	}
}

func run(t *testing.T, d *Dispatcher, rows []scm.MirrorRow) error {
	t.Helper()
	return d.ReviewChanges(context.Background(), "ns", "proj", nil, "https://github.com/acme/app", rows)
}

// --- tests -------------------------------------------------------------------

func TestNilPolicyIsNoOp(t *testing.T) {
	store := newFakeStore()
	d := &Dispatcher{Policy: fakePolicy{pol: nil}, Members: &fakeMembers{}, Store: store}
	if err := run(t, d, []scm.MirrorRow{prRow("1", "open", "a", "sha1")}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(store.reqs) != 0 {
		t.Fatalf("nil policy dispatched %d reviews, want 0", len(store.reqs))
	}
}

func TestDisabledPolicyIsNoOp(t *testing.T) {
	pol := enabledPolicy()
	pol.Enabled = false
	store := newFakeStore()
	d := &Dispatcher{Policy: fakePolicy{pol: pol}, Members: &fakeMembers{}, Store: store}
	if err := run(t, d, []scm.MirrorRow{prRow("1", "open", "a", "sha1")}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(store.reqs) != 0 {
		t.Fatalf("disabled policy dispatched %d reviews, want 0", len(store.reqs))
	}
}

func TestEnabledButNoReviewerFailsLoudly(t *testing.T) {
	pol := enabledPolicy()
	pol.ReviewerAgentID = ""
	d := &Dispatcher{Policy: fakePolicy{pol: pol}, Members: &fakeMembers{}, Store: newFakeStore()}
	if err := run(t, d, []scm.MirrorRow{prRow("1", "open", "a", "sha1")}); err == nil {
		t.Fatal("want error for enabled policy with empty ReviewerAgentID, got nil")
	}
}

func TestOnlyOpenPRRowsQualify(t *testing.T) {
	store := newFakeStore()
	d := &Dispatcher{Policy: fakePolicy{pol: enabledPolicy()}, Members: &fakeMembers{}, Store: store}
	rows := []scm.MirrorRow{
		prRow("1", "open", "a", "sha1"),
		prRow("2", "closed", "a", "sha2"),
		prRow("3", "merged", "a", "sha3"),
		{Kind: scm.RecordTypeIssue, ExternalID: "9", State: "open"},
		{Kind: scm.RecordTypeBranch, ExternalID: "main", State: "default"},
	}
	if err := run(t, d, rows); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(store.reqs) != 1 || store.reqs[0].PRNumber != "1" {
		t.Fatalf("want exactly PR #1 reviewed, got %+v", store.reqs)
	}
}

func TestScopeTeamAuthoredFiltersNonMembers(t *testing.T) {
	pol := enabledPolicy()
	pol.Scope = scopeTeamAuthored
	members := &fakeMembers{agents: map[string]bool{"team-bot": true}}
	store := newFakeStore()
	d := &Dispatcher{Policy: fakePolicy{pol: pol}, Members: members, Store: store}
	rows := []scm.MirrorRow{
		prRow("1", "open", "team-bot", "sha1"),
		prRow("2", "open", "outsider", "sha2"),
		prRow("3", "open", "", "sha3"), // unknown author never qualifies
	}
	if err := run(t, d, rows); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(store.reqs) != 1 || store.reqs[0].PRNumber != "1" {
		t.Fatalf("team_authored should review only PR #1, got %+v", store.reqs)
	}
}

func TestScopeAllSkipsMembershipCheck(t *testing.T) {
	members := &fakeMembers{}
	store := newFakeStore()
	d := &Dispatcher{Policy: fakePolicy{pol: enabledPolicy()}, Members: members, Store: store}
	if err := run(t, d, []scm.MirrorRow{prRow("1", "open", "outsider", "sha1")}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(store.reqs) != 1 {
		t.Fatalf("scope=all should review the PR, got %+v", store.reqs)
	}
	if members.calls != 0 {
		t.Fatalf("scope=all must not consult team membership, got %d calls", members.calls)
	}
}

func TestOnNewCommitsSkipsRowsWithoutHeadSHA(t *testing.T) {
	store := newFakeStore()
	d := &Dispatcher{Policy: fakePolicy{pol: enabledPolicy()}, Members: &fakeMembers{}, Store: store}
	if err := run(t, d, []scm.MirrorRow{prRow("1", "open", "a", "")}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(store.reqs) != 0 {
		t.Fatalf("on_new_commits with no head SHA must skip, got %+v", store.reqs)
	}
}

func TestOnOpenReviewsOncePerPRRegardlessOfSHA(t *testing.T) {
	pol := enabledPolicy()
	pol.Trigger = triggerOnOpen
	store := newFakeStore()
	d := &Dispatcher{Policy: fakePolicy{pol: pol}, Members: &fakeMembers{}, Store: store}
	// Two passes with different head SHAs for the same PR must produce the same
	// dedup label under on_open (SHA excluded), so the second pass is a no-op.
	if err := run(t, d, []scm.MirrorRow{prRow("7", "open", "a", "shaA")}); err != nil {
		t.Fatal(err)
	}
	if err := run(t, d, []scm.MirrorRow{prRow("7", "open", "a", "shaB")}); err != nil {
		t.Fatal(err)
	}
	if len(store.reqs) != 2 {
		t.Fatalf("store consulted %d times, want 2", len(store.reqs))
	}
	if store.reqs[0].DedupLabel != store.reqs[1].DedupLabel {
		t.Fatalf("on_open labels differ across SHAs: %q vs %q", store.reqs[0].DedupLabel, store.reqs[1].DedupLabel)
	}
	// Even with no head SHA, on_open still reviews (SHA not required).
	store2 := newFakeStore()
	d2 := &Dispatcher{Policy: fakePolicy{pol: pol}, Members: &fakeMembers{}, Store: store2}
	if err := run(t, d2, []scm.MirrorRow{prRow("8", "open", "a", "")}); err != nil {
		t.Fatal(err)
	}
	if len(store2.reqs) != 1 {
		t.Fatalf("on_open with no head SHA should still review, got %+v", store2.reqs)
	}
}

func TestOnNewCommitsDistinctLabelPerHead(t *testing.T) {
	store := newFakeStore()
	d := &Dispatcher{Policy: fakePolicy{pol: enabledPolicy()}, Members: &fakeMembers{}, Store: store}
	if err := run(t, d, []scm.MirrorRow{prRow("7", "open", "a", "shaA")}); err != nil {
		t.Fatal(err)
	}
	if err := run(t, d, []scm.MirrorRow{prRow("7", "open", "a", "shaB")}); err != nil {
		t.Fatal(err)
	}
	if store.reqs[0].DedupLabel == store.reqs[1].DedupLabel {
		t.Fatal("on_new_commits must yield a distinct label per head SHA")
	}
}

func TestDedupLabelWithinCoordBound(t *testing.T) {
	// maxLabelLen in pkg/coord/workitemwrite.go is 64.
	const maxLabelLen = 64
	l := dedupLabel(triggerOnNewCommits, "https://github.com/some-really-long-org-name/and-a-long-repository-name", "123456", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	if len(l) > maxLabelLen {
		t.Fatalf("dedup label %q is %d chars, exceeds coord maxLabelLen %d", l, len(l), maxLabelLen)
	}
	if got := dedupLabelPrefix; l[:len(got)] != got {
		t.Fatalf("dedup label missing prefix %q: %q", got, l)
	}
}

func TestReviewRequestProvenance(t *testing.T) {
	store := newFakeStore()
	d := &Dispatcher{Policy: fakePolicy{pol: enabledPolicy()}, Members: &fakeMembers{}, Store: store}
	if err := run(t, d, []scm.MirrorRow{prRow("1", "open", "a", "sha1")}); err != nil {
		t.Fatal(err)
	}
	got := store.reqs[0]
	if got.ReviewerAgentID != "agent-reviewer" {
		t.Fatalf("ReviewerAgentID = %q", got.ReviewerAgentID)
	}
	if got.Principal != "user:henrik" {
		t.Fatalf("Principal (EnabledBy) = %q, want the human authorizing act", got.Principal)
	}
	if got.ProjectID != "proj-1" || got.TeamID != "team-1" {
		t.Fatalf("coord scope not threaded: %+v", got)
	}
	if got.HeadSHA != "sha1" || got.PRNumber != "1" {
		t.Fatalf("PR identity not threaded: %+v", got)
	}
}

func TestPolicyErrorFailsPass(t *testing.T) {
	d := &Dispatcher{Policy: fakePolicy{err: errors.New("db down")}, Members: &fakeMembers{}, Store: newFakeStore()}
	if err := run(t, d, nil); err == nil {
		t.Fatal("policy resolve error must fail the pass")
	}
}

func TestStoreErrorFailsPass(t *testing.T) {
	store := newFakeStore()
	store.failOn = "2"
	d := &Dispatcher{Policy: fakePolicy{pol: enabledPolicy()}, Members: &fakeMembers{}, Store: store}
	rows := []scm.MirrorRow{prRow("1", "open", "a", "sha1"), prRow("2", "open", "a", "sha2")}
	if err := run(t, d, rows); err == nil {
		t.Fatal("store error must fail the reconcile so it retries")
	}
}

func TestIdempotentAcrossReRun(t *testing.T) {
	store := newFakeStore()
	d := &Dispatcher{Policy: fakePolicy{pol: enabledPolicy()}, Members: &fakeMembers{}, Store: store}
	rows := []scm.MirrorRow{prRow("1", "open", "a", "sha1")}
	for i := 0; i < 3; i++ {
		if err := run(t, d, rows); err != nil {
			t.Fatal(err)
		}
	}
	// Store is consulted every pass (level-triggered), but only the first
	// created — the fake store dedups on the label exactly as coord will.
	created := 0
	for range store.seen {
		created++
	}
	if created != 1 {
		t.Fatalf("re-running against an unchanged snapshot created %d items, want 1", created)
	}
}
