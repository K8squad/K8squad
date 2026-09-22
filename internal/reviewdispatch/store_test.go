package reviewdispatch

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/K8squad/K8squad/pkg/controller/reviewtrigger"
	"github.com/K8squad/K8squad/pkg/coord"
)

// fakeWriter records the EnsureReviewWorkItem input and returns a scripted result.
type fakeWriter struct {
	got    coord.EnsureReviewWorkItemInput
	result coord.EnsureReviewWorkItemResult
	err    error
	calls  int
}

func (f *fakeWriter) EnsureReviewWorkItem(_ context.Context, in coord.EnsureReviewWorkItemInput) (coord.EnsureReviewWorkItemResult, error) {
	f.calls++
	f.got = in
	if f.err != nil {
		return coord.EnsureReviewWorkItemResult{}, f.err
	}
	return f.result, nil
}

// fakeDispatch records the RequestDispatch input and returns a scripted error.
type fakeDispatch struct {
	got   coord.RequestDispatchInput
	err   error
	calls int
}

func (f *fakeDispatch) RequestDispatch(_ context.Context, in coord.RequestDispatchInput) (coord.WorkItemDispatchResult, error) {
	f.calls++
	f.got = in
	return coord.WorkItemDispatchResult{}, f.err
}

func newStore(w reviewItemWriter, d reviewItemDispatcher) *SystemReviewItemStore {
	return &SystemReviewItemStore{writer: w, dispatch: d}
}

func sampleReq() reviewtrigger.ReviewRequest {
	return reviewtrigger.ReviewRequest{
		DedupLabel:      "ksquad.review=deadbeef",
		ProjectID:       "proj-uid",
		TeamID:          "team-uid",
		ReviewerAgentID: "reviewer-bot",
		Principal:       "user:alice", // the human EnabledBy
		RepoURL:         "https://github.com/acme/widget",
		PRNumber:        "42",
		HeadSHA:         "abc123",
		Title:           "Review widget#42: add gizmo",
	}
}

// TestEnsureReviewFreshCreateDispatchesUnderSystemIdentity is the core D1 assertion:
// a fresh create is authored under the SYSTEM principal (never an agent) and the
// dispatch is stamped Initiator=system with Principal=the human EnabledBy.
func TestEnsureReviewFreshCreateDispatchesUnderSystemIdentity(t *testing.T) {
	w := &fakeWriter{result: coord.EnsureReviewWorkItemResult{
		Item:    coord.WorkItemRecord{ID: "wi-1", State: "backlog"},
		Created: true,
	}}
	d := &fakeDispatch{}
	created, err := newStore(w, d).EnsureReview(context.Background(), sampleReq())
	if err != nil {
		t.Fatalf("EnsureReview: %v", err)
	}
	if !created {
		t.Fatal("want created=true for a fresh insert")
	}
	// Create authored under the SYSTEM principal — the ISI-4711 custody-wall guarantee.
	if w.got.Principal != reviewtrigger.Initiator {
		t.Errorf("create principal = %q, want SYSTEM %q", w.got.Principal, reviewtrigger.Initiator)
	}
	if w.got.DedupLabel != "ksquad.review=deadbeef" || w.got.ProjectID != "proj-uid" || w.got.TeamID != "team-uid" {
		t.Errorf("create scope wrong: %+v", w.got)
	}
	// ISI-4767 E5: the create carries the plaintext PR-review anchor so the console
	// read model can join this review back to the PR card. Slug is lower-cased.
	wantAnchor := "ksquad.github.pr=acme/widget#42"
	if len(w.got.ExtraLabels) != 1 || w.got.ExtraLabels[0] != wantAnchor {
		t.Errorf("create ExtraLabels = %v, want [%q]", w.got.ExtraLabels, wantAnchor)
	}
	// Dispatch: system Initiator, human EnabledBy Principal, correct agent + item.
	if d.calls != 1 {
		t.Fatalf("dispatch calls = %d, want 1", d.calls)
	}
	if d.got.Initiator != reviewtrigger.Initiator {
		t.Errorf("dispatch Initiator = %q, want %q", d.got.Initiator, reviewtrigger.Initiator)
	}
	if d.got.Principal != "user:alice" {
		t.Errorf("dispatch Principal = %q, want human EnabledBy user:alice", d.got.Principal)
	}
	if d.got.AgentID != "reviewer-bot" || d.got.WorkItemID != "wi-1" || d.got.TeamID != "team-uid" {
		t.Errorf("dispatch target wrong: %+v", d.got)
	}
}

// TestEnsureReviewIdempotentSkipsDispatch: an already-dispatched item (found, past
// backlog) is a no-op — created=false and NO re-dispatch (which would 409).
func TestEnsureReviewIdempotentSkipsDispatch(t *testing.T) {
	for _, state := range []string{"todo", "in_progress", "in_review", "done"} {
		w := &fakeWriter{result: coord.EnsureReviewWorkItemResult{
			Item:    coord.WorkItemRecord{ID: "wi-1", State: state},
			Created: false,
		}}
		d := &fakeDispatch{}
		created, err := newStore(w, d).EnsureReview(context.Background(), sampleReq())
		if err != nil {
			t.Fatalf("state %s: EnsureReview: %v", state, err)
		}
		if created {
			t.Errorf("state %s: want created=false", state)
		}
		if d.calls != 0 {
			t.Errorf("state %s: want no dispatch on an already-advanced item, got %d", state, d.calls)
		}
	}
}

// TestEnsureReviewSelfHealsBacklogOrphan: a prior pass created the item but died
// before dispatch (found, still backlog). This pass re-dispatches it even though
// created=false.
func TestEnsureReviewSelfHealsBacklogOrphan(t *testing.T) {
	w := &fakeWriter{result: coord.EnsureReviewWorkItemResult{
		Item:    coord.WorkItemRecord{ID: "wi-1", State: "backlog"},
		Created: false, // already existed
	}}
	d := &fakeDispatch{}
	created, err := newStore(w, d).EnsureReview(context.Background(), sampleReq())
	if err != nil {
		t.Fatalf("EnsureReview: %v", err)
	}
	if created {
		t.Fatal("want created=false for an existing orphan")
	}
	if d.calls != 1 {
		t.Fatalf("want the backlog orphan re-dispatched, got %d dispatch calls", d.calls)
	}
}

// TestEnsureReviewSwallowsStateConflict: a dispatch race that lands ErrStateConflict
// (a parallel pass already advanced the item) is benign — the pass succeeds.
func TestEnsureReviewSwallowsStateConflict(t *testing.T) {
	w := &fakeWriter{result: coord.EnsureReviewWorkItemResult{
		Item:    coord.WorkItemRecord{ID: "wi-1", State: "backlog"},
		Created: true,
	}}
	d := &fakeDispatch{err: coord.ErrStateConflict}
	if _, err := newStore(w, d).EnsureReview(context.Background(), sampleReq()); err != nil {
		t.Fatalf("ErrStateConflict must be swallowed, got %v", err)
	}
}

// TestEnsureReviewPropagatesRealDispatchError: a non-conflict dispatch error fails
// the pass so the reconcile retries.
func TestEnsureReviewPropagatesRealDispatchError(t *testing.T) {
	w := &fakeWriter{result: coord.EnsureReviewWorkItemResult{
		Item:    coord.WorkItemRecord{ID: "wi-1", State: "backlog"},
		Created: true,
	}}
	boom := errors.New("dispatch backend down")
	d := &fakeDispatch{err: boom}
	if _, err := newStore(w, d).EnsureReview(context.Background(), sampleReq()); !errors.Is(err, boom) {
		t.Fatalf("want the dispatch error surfaced, got %v", err)
	}
}

// TestEnsureReviewRequiresPrincipal: a request with no EnabledBy provenance fails
// closed BEFORE any create — the D1 provenance is mandatory.
func TestEnsureReviewRequiresPrincipal(t *testing.T) {
	req := sampleReq()
	req.Principal = ""
	w := &fakeWriter{}
	d := &fakeDispatch{}
	if _, err := newStore(w, d).EnsureReview(context.Background(), req); err == nil {
		t.Fatal("want an error for a request with no Principal")
	}
	if w.calls != 0 || d.calls != 0 {
		t.Fatal("must not create or dispatch without provenance")
	}
}

// TestEnsureReviewCreateErrorNoDispatch: a create failure surfaces and never
// dispatches.
func TestEnsureReviewCreateErrorNoDispatch(t *testing.T) {
	w := &fakeWriter{err: errors.New("insert failed")}
	d := &fakeDispatch{}
	if _, err := newStore(w, d).EnsureReview(context.Background(), sampleReq()); err == nil {
		t.Fatal("want the create error surfaced")
	}
	if d.calls != 0 {
		t.Fatal("must not dispatch after a failed create")
	}
}

// TestReviewBodyHasNoSecretsAndPinsHead: the body links the PR and pins the head
// SHA, and never embeds a token.
func TestReviewBodyHasNoSecretsAndPinsHead(t *testing.T) {
	body := reviewBody(sampleReq())
	if !strings.Contains(body, "widget") || !strings.Contains(body, "#42") || !strings.Contains(body, "abc123") {
		t.Errorf("body missing PR context: %q", body)
	}
	if strings.Contains(strings.ToLower(body), "token") || strings.Contains(body, "secret=") {
		t.Errorf("body must not carry secrets: %q", body)
	}
}
