package issuedispatch

import (
	"context"
	"errors"
	"testing"

	"github.com/K8squad/K8squad/pkg/controller/issuetrigger"
	"github.com/K8squad/K8squad/pkg/coord"
)

type fakeWriter struct {
	res coord.EnsureReviewWorkItemResult
	err error
	got coord.EnsureReviewWorkItemInput
}

func (f *fakeWriter) EnsureReviewWorkItem(_ context.Context, in coord.EnsureReviewWorkItemInput) (coord.EnsureReviewWorkItemResult, error) {
	f.got = in
	return f.res, f.err
}

type fakeDispatcher struct {
	err   error
	calls int
	got   coord.RequestDispatchInput
}

func (f *fakeDispatcher) RequestDispatch(_ context.Context, in coord.RequestDispatchInput) (coord.WorkItemDispatchResult, error) {
	f.calls++
	f.got = in
	return coord.WorkItemDispatchResult{}, f.err
}

func newStore(w *fakeWriter, d *fakeDispatcher) *SystemTriageItemStore {
	return &SystemTriageItemStore{writer: w, dispatch: d}
}

func baseReq() issuetrigger.TriageRequest {
	return issuetrigger.TriageRequest{
		DedupLabel:    "ksquad.github.issue=acme/widget#42",
		ProjectID:     "proj",
		TeamID:        "team",
		TriageAgentID: "triage-bot",
		Principal:     "user:henrik",
		IssueRef:      "acme/widget#42",
		IssueURL:      "https://github.com/acme/widget/issues/42",
		Title:         "Triage acme/widget#42",
	}
}

func TestFreshCreateDispatches(t *testing.T) {
	w := &fakeWriter{res: coord.EnsureReviewWorkItemResult{
		Item: coord.WorkItemRecord{ID: "wi-1", State: "backlog"}, Created: true,
	}}
	d := &fakeDispatcher{}
	created, err := newStore(w, d).EnsureTriage(context.Background(), baseReq())
	if err != nil {
		t.Fatalf("EnsureTriage: %v", err)
	}
	if !created {
		t.Error("want created=true")
	}
	if d.calls != 1 {
		t.Fatalf("want 1 dispatch, got %d", d.calls)
	}
	// Governance: author under SYSTEM identity, dispatch stamped system initiator +
	// human principal.
	if w.got.Principal != issuetrigger.Initiator {
		t.Errorf("author principal = %q, want %q", w.got.Principal, issuetrigger.Initiator)
	}
	if d.got.Initiator != issuetrigger.Initiator {
		t.Errorf("dispatch initiator = %q, want %q", d.got.Initiator, issuetrigger.Initiator)
	}
	if d.got.Principal != "user:henrik" {
		t.Errorf("dispatch principal = %q, want human EnabledBy", d.got.Principal)
	}
	if d.got.AgentID != "triage-bot" {
		t.Errorf("dispatch agent = %q", d.got.AgentID)
	}
}

func TestExistingPastBacklogNoDispatch(t *testing.T) {
	w := &fakeWriter{res: coord.EnsureReviewWorkItemResult{
		Item: coord.WorkItemRecord{ID: "wi-1", State: "todo"}, Created: false,
	}}
	d := &fakeDispatcher{}
	created, err := newStore(w, d).EnsureTriage(context.Background(), baseReq())
	if err != nil {
		t.Fatalf("EnsureTriage: %v", err)
	}
	if created {
		t.Error("want created=false for existing item")
	}
	if d.calls != 0 {
		t.Fatalf("already-handled item must not re-dispatch, got %d", d.calls)
	}
}

func TestSelfHealBacklogDispatches(t *testing.T) {
	// A prior pass created the item but never dispatched: it sits in backlog and
	// this pass must dispatch it even though created=false.
	w := &fakeWriter{res: coord.EnsureReviewWorkItemResult{
		Item: coord.WorkItemRecord{ID: "wi-1", State: "backlog"}, Created: false,
	}}
	d := &fakeDispatcher{}
	if _, err := newStore(w, d).EnsureTriage(context.Background(), baseReq()); err != nil {
		t.Fatalf("EnsureTriage: %v", err)
	}
	if d.calls != 1 {
		t.Fatalf("self-heal should dispatch the stranded backlog item, got %d", d.calls)
	}
}

func TestStateConflictSwallowed(t *testing.T) {
	w := &fakeWriter{res: coord.EnsureReviewWorkItemResult{
		Item: coord.WorkItemRecord{ID: "wi-1", State: "backlog"}, Created: true,
	}}
	d := &fakeDispatcher{err: coord.ErrStateConflict}
	if _, err := newStore(w, d).EnsureTriage(context.Background(), baseReq()); err != nil {
		t.Fatalf("ErrStateConflict must be swallowed, got %v", err)
	}
}

func TestDispatchErrorPropagated(t *testing.T) {
	w := &fakeWriter{res: coord.EnsureReviewWorkItemResult{
		Item: coord.WorkItemRecord{ID: "wi-1", State: "backlog"}, Created: true,
	}}
	d := &fakeDispatcher{err: errors.New("boom")}
	if _, err := newStore(w, d).EnsureTriage(context.Background(), baseReq()); err == nil {
		t.Fatal("a non-conflict dispatch error must propagate")
	}
}

func TestMissingPrincipalFailsClosed(t *testing.T) {
	w := &fakeWriter{}
	d := &fakeDispatcher{}
	req := baseReq()
	req.Principal = ""
	if _, err := newStore(w, d).EnsureTriage(context.Background(), req); err == nil {
		t.Fatal("missing Principal must fail closed")
	}
	if w.got.DedupLabel != "" {
		t.Error("writer must not be called when Principal is missing")
	}
}
