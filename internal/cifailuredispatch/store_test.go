package cifailuredispatch

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/K8squad/K8squad/pkg/controller/cifailure"
	"github.com/K8squad/K8squad/pkg/coord"
)

// fakeWriter records the EnsureCIFailureWorkItem input and returns a scripted result.
type fakeWriter struct {
	got    coord.EnsureCIFailureWorkItemInput
	result coord.EnsureCIFailureWorkItemResult
	err    error
	calls  int
}

func (f *fakeWriter) EnsureCIFailureWorkItem(_ context.Context, in coord.EnsureCIFailureWorkItemInput) (coord.EnsureCIFailureWorkItemResult, error) {
	f.calls++
	f.got = in
	if f.err != nil {
		return coord.EnsureCIFailureWorkItemResult{}, f.err
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

func newStore(w ciItemWriter, d ciItemDispatcher) *SystemCIFailureItemStore {
	return &SystemCIFailureItemStore{writer: w, dispatch: d}
}

func sampleReq() cifailure.FailureRequest {
	return cifailure.FailureRequest{
		DedupLabel: "ksquad.github.ci=deadbeef",
		ProjectID:  "proj-uid",
		TeamID:     "team-uid",
		AgentID:    "triage-bot",
		Principal:  "user:alice", // the human EnabledBy
		RepoURL:    "https://github.com/acme/widget",
		CheckName:  "e2e",
		HeadSHA:    "abc123",
		Conclusion: "failure",
		Title:      "Analyze CI failure: e2e @ abc123",
	}
}

// TestFreshCreateDispatchesUnderSystemIdentity is the core governance assertion:
// a fresh create is authored under the SYSTEM principal (never an agent) and the
// dispatch is stamped Initiator=system with Principal=the human EnabledBy.
func TestFreshCreateDispatchesUnderSystemIdentity(t *testing.T) {
	w := &fakeWriter{result: coord.EnsureCIFailureWorkItemResult{
		Item:    coord.WorkItemRecord{ID: "wi-1", State: "backlog"},
		Created: true,
	}}
	d := &fakeDispatch{}
	created, err := newStore(w, d).EnsureCIFailure(context.Background(), sampleReq())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !created {
		t.Fatal("expected created=true for a fresh insert")
	}
	// Authored under the SYSTEM identity, never the agent or the human.
	if w.got.Principal != cifailure.Initiator {
		t.Fatalf("work item must be authored under %q, got %q", cifailure.Initiator, w.got.Principal)
	}
	if w.got.DedupLabel != "ksquad.github.ci=deadbeef" {
		t.Fatalf("dedup label not threaded: %q", w.got.DedupLabel)
	}
	// Dispatch stamped Initiator=system, Principal=the human EnabledBy, to the agent.
	if d.calls != 1 {
		t.Fatalf("expected exactly one dispatch, got %d", d.calls)
	}
	if d.got.Initiator != cifailure.Initiator {
		t.Fatalf("dispatch Initiator must be %q, got %q", cifailure.Initiator, d.got.Initiator)
	}
	if d.got.Principal != "user:alice" {
		t.Fatalf("dispatch Principal must be the human EnabledBy, got %q", d.got.Principal)
	}
	if d.got.AgentID != "triage-bot" {
		t.Fatalf("dispatch AgentID wrong: %q", d.got.AgentID)
	}
}

// An already-advanced item (not in backlog) is not re-dispatched (idempotent no-op).
func TestExistingAdvancedItemNotRedispatched(t *testing.T) {
	w := &fakeWriter{result: coord.EnsureCIFailureWorkItemResult{
		Item:    coord.WorkItemRecord{ID: "wi-1", State: "in_progress"},
		Created: false,
	}}
	d := &fakeDispatch{}
	created, err := newStore(w, d).EnsureCIFailure(context.Background(), sampleReq())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if created {
		t.Fatal("expected created=false for an existing item")
	}
	if d.calls != 0 {
		t.Fatalf("an item past backlog must not be re-dispatched, got %d calls", d.calls)
	}
}

// Self-heal: a prior pass created the item but failed before dispatch; it is
// still in backlog, so this pass re-dispatches it even though created=false.
func TestSelfHealBacklogItemDispatches(t *testing.T) {
	w := &fakeWriter{result: coord.EnsureCIFailureWorkItemResult{
		Item:    coord.WorkItemRecord{ID: "wi-1", State: "backlog"},
		Created: false,
	}}
	d := &fakeDispatch{}
	if _, err := newStore(w, d).EnsureCIFailure(context.Background(), sampleReq()); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if d.calls != 1 {
		t.Fatalf("a backlog item should be (re)dispatched, got %d calls", d.calls)
	}
}

// A benign ErrStateConflict on dispatch (item already claimed by a racing pass)
// is swallowed, not surfaced as a pass failure.
func TestDispatchStateConflictSwallowed(t *testing.T) {
	w := &fakeWriter{result: coord.EnsureCIFailureWorkItemResult{
		Item:    coord.WorkItemRecord{ID: "wi-1", State: "backlog"},
		Created: true,
	}}
	d := &fakeDispatch{err: coord.ErrStateConflict}
	if _, err := newStore(w, d).EnsureCIFailure(context.Background(), sampleReq()); err != nil {
		t.Fatalf("ErrStateConflict must be swallowed, got %v", err)
	}
}

// A non-conflict dispatch error fails the pass.
func TestDispatchErrorFailsPass(t *testing.T) {
	w := &fakeWriter{result: coord.EnsureCIFailureWorkItemResult{
		Item:    coord.WorkItemRecord{ID: "wi-1", State: "backlog"},
		Created: true,
	}}
	d := &fakeDispatch{err: errors.New("boom")}
	if _, err := newStore(w, d).EnsureCIFailure(context.Background(), sampleReq()); err == nil {
		t.Fatal("expected a non-conflict dispatch error to fail the pass")
	}
}

// A request with no Principal (EnabledBy) provenance fails closed before any write.
func TestMissingPrincipalFailsClosed(t *testing.T) {
	w := &fakeWriter{}
	d := &fakeDispatch{}
	req := sampleReq()
	req.Principal = ""
	_, err := newStore(w, d).EnsureCIFailure(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "provenance") {
		t.Fatalf("expected a provenance error, got %v", err)
	}
	if w.calls != 0 || d.calls != 0 {
		t.Fatal("no write or dispatch may happen without provenance")
	}
}

func TestNewSystemCIFailureItemStoreRejectsNil(t *testing.T) {
	if _, err := NewSystemCIFailureItemStore(nil, nil); err == nil {
		t.Fatal("expected nil writer to be rejected")
	}
}
