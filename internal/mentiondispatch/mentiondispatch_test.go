package mentiondispatch

// Run-minting coverage for dispatch-on-mention (ISI-5116; relocated from internal/apiserver in ISI-5125).
// The dispatcher is exercised against fakes — no Postgres — so these ride the default unit lane. The
// DB-backed guarantees (board-hide filter, (message,agent) idempotency at the SQL layer) are covered by
// the chaos-tagged contract tests.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/coord"
)

// --- fakes -------------------------------------------------------------------

type fakeCreate struct {
	calls []coord.CreateWorkItemInput
	rec   coord.WorkItemRecord
	err   error
}

func (f *fakeCreate) CreateWorkItem(_ context.Context, in coord.CreateWorkItemInput) (coord.WorkItemRecord, error) {
	f.calls = append(f.calls, in)
	if f.err != nil {
		return coord.WorkItemRecord{}, f.err
	}
	return f.rec, nil
}

type fakeDispatch struct {
	calls []coord.RequestDispatchInput
	err   error
}

func (f *fakeDispatch) RequestDispatch(_ context.Context, in coord.RequestDispatchInput) (coord.WorkItemDispatchResult, error) {
	f.calls = append(f.calls, in)
	if f.err != nil {
		return coord.WorkItemDispatchResult{}, f.err
	}
	return coord.WorkItemDispatchResult{WorkItemID: in.WorkItemID, FromState: "backlog", ToState: "todo", RequestedAgent: in.AgentID}, nil
}

type fakeRefs struct{ uid, team string }

func (f fakeRefs) ResolveProject(_ context.Context, _ string) (ResolvedProject, error) {
	return ResolvedProject{UID: f.uid, TeamUID: f.team}, nil
}

type fakeLedger struct {
	seen     map[string]bool
	bound    map[string]string
	released map[string]bool
	claimErr error
}

func newFakeLedger() *fakeLedger {
	return &fakeLedger{seen: map[string]bool{}, bound: map[string]string{}, released: map[string]bool{}}
}

func ledgerKey(msg uuid.UUID, agent string) string { return msg.String() + "|" + agent }

func (l *fakeLedger) Claim(_ context.Context, d discussion.MentionDispatch) (bool, error) {
	if l.claimErr != nil {
		return false, l.claimErr
	}
	k := ledgerKey(d.MessageID, d.AgentName)
	if l.seen[k] {
		return false, nil // already dispatched — the idempotency no-op
	}
	l.seen[k] = true
	return true, nil
}

func (l *fakeLedger) Bind(_ context.Context, msg uuid.UUID, agent, wid string) error {
	l.bound[ledgerKey(msg, agent)] = wid
	return nil
}

func (l *fakeLedger) Release(_ context.Context, msg uuid.UUID, agent string) error {
	k := ledgerKey(msg, agent)
	delete(l.seen, k)
	l.released[k] = true
	return nil
}

func (l *fakeLedger) HopForDispatchedRun(_ context.Context, _ string) (int, bool, error) {
	return 0, false, nil
}

func newTestDispatcher(fc *fakeCreate, fd *fakeDispatch, fl *fakeLedger, marks *[]string) *dispatcher {
	return &dispatcher{
		create:   fc,
		dispatch: fd,
		refs:     fakeRefs{uid: "proj-uid", team: "team-uid"},
		ledger:   fl,
		markSource: func(_ context.Context, id string) error {
			*marks = append(*marks, id)
			return nil
		},
	}
}

func sampleDispatch() discussion.MentionDispatch {
	return discussion.MentionDispatch{
		ProjectID:            "squad-a/proj",
		ThreadID:             uuid.New(),
		MessageID:            uuid.New(),
		TeamID:               uuid.New(),
		AgentName:            "Robo-Coder",
		HopDepth:             1,
		TriggeredByPrincipal: "user:henrik",
	}
}

// --- (a) an @-mention mints exactly one Run for the named agent --------------

func TestMentionDispatchMintsOnce(t *testing.T) {
	fc := &fakeCreate{rec: coord.WorkItemRecord{ID: "11111111-1111-1111-1111-111111111111", State: "backlog"}}
	fd := &fakeDispatch{}
	fl := newFakeLedger()
	var marks []string
	md := newTestDispatcher(fc, fd, fl, &marks)

	d := sampleDispatch()
	if err := md.DispatchMention(context.Background(), d); err != nil {
		t.Fatalf("DispatchMention: %v", err)
	}

	if len(fc.calls) != 1 {
		t.Fatalf("CreateWorkItem called %d times, want 1", len(fc.calls))
	}
	in := fc.calls[0]
	if in.ProjectID != "proj-uid" || in.TeamID != "team-uid" {
		t.Fatalf("create resolved to project/team %q/%q, want proj-uid/team-uid", in.ProjectID, in.TeamID)
	}
	if in.Principal != dispatchPrincipal {
		t.Fatalf("create principal = %q, want %q (trusted server principal, not the agent)", in.Principal, dispatchPrincipal)
	}
	if in.Body == "" || in.Title == "" {
		t.Fatal("create must carry a title + thread-context body")
	}

	if len(marks) != 1 || marks[0] != fc.rec.ID {
		t.Fatalf("markSource calls = %v, want exactly [%s] (board-hide before dispatch)", marks, fc.rec.ID)
	}

	if len(fd.calls) != 1 {
		t.Fatalf("RequestDispatch called %d times, want 1", len(fd.calls))
	}
	rd := fd.calls[0]
	if rd.WorkItemID != fc.rec.ID {
		t.Fatalf("dispatch work item = %q, want the minted %q", rd.WorkItemID, fc.rec.ID)
	}
	if rd.AgentID != d.AgentName {
		t.Fatalf("dispatch agent = %q, want the mentioned %q", rd.AgentID, d.AgentName)
	}
	if rd.Initiator != "human" {
		t.Fatalf("human-triggered dispatch initiator = %q, want human", rd.Initiator)
	}
	if fl.bound[ledgerKey(d.MessageID, d.AgentName)] != fc.rec.ID {
		t.Fatal("ledger did not bind the minted work item id")
	}
}

// idempotency: a redelivery of the SAME (message, agent) never mints a second Run.
func TestMentionDispatchIdempotent(t *testing.T) {
	fc := &fakeCreate{rec: coord.WorkItemRecord{ID: "22222222-2222-2222-2222-222222222222", State: "backlog"}}
	fd := &fakeDispatch{}
	fl := newFakeLedger()
	var marks []string
	md := newTestDispatcher(fc, fd, fl, &marks)

	d := sampleDispatch()
	for i := 0; i < 3; i++ {
		if err := md.DispatchMention(context.Background(), d); err != nil {
			t.Fatalf("DispatchMention #%d: %v", i, err)
		}
	}
	if len(fc.calls) != 1 || len(fd.calls) != 1 {
		t.Fatalf("after 3 identical deliveries: creates=%d dispatches=%d, want 1/1 (idempotent on (message,agent))", len(fc.calls), len(fd.calls))
	}
}

// a different agent on the same message is a distinct dispatch (fan-out to N mentioned agents).
func TestMentionDispatchDistinctAgentsSameMessage(t *testing.T) {
	fc := &fakeCreate{rec: coord.WorkItemRecord{ID: "33333333-3333-3333-3333-333333333333", State: "backlog"}}
	fd := &fakeDispatch{}
	fl := newFakeLedger()
	var marks []string
	md := newTestDispatcher(fc, fd, fl, &marks)

	d := sampleDispatch()
	d2 := d
	d2.AgentName = "Reviewer"
	if err := md.DispatchMention(context.Background(), d); err != nil {
		t.Fatalf("DispatchMention a: %v", err)
	}
	if err := md.DispatchMention(context.Background(), d2); err != nil {
		t.Fatalf("DispatchMention b: %v", err)
	}
	if len(fd.calls) != 2 {
		t.Fatalf("two distinct mentioned agents ⇒ %d dispatches, want 2", len(fd.calls))
	}
}

// an agent-authored trigger dispatches with agent provenance.
func TestMentionDispatchInitiatorAgent(t *testing.T) {
	fc := &fakeCreate{rec: coord.WorkItemRecord{ID: "44444444-4444-4444-4444-444444444444", State: "backlog"}}
	fd := &fakeDispatch{}
	fl := newFakeLedger()
	var marks []string
	md := newTestDispatcher(fc, fd, fl, &marks)

	d := sampleDispatch()
	triggerer := "agent-pm"
	d.TriggeredByAgentID = &triggerer
	d.HopDepth = 2
	if err := md.DispatchMention(context.Background(), d); err != nil {
		t.Fatalf("DispatchMention: %v", err)
	}
	if fd.calls[0].Initiator != "agent" {
		t.Fatalf("agent-triggered dispatch initiator = %q, want agent", fd.calls[0].Initiator)
	}
}

// a mint failure after the claim releases it so a later retry can redo the dispatch cleanly.
func TestMentionDispatchReleasesClaimOnDispatchFailure(t *testing.T) {
	fc := &fakeCreate{rec: coord.WorkItemRecord{ID: "55555555-5555-5555-5555-555555555555", State: "backlog"}}
	fd := &fakeDispatch{err: errors.New("intake down")}
	fl := newFakeLedger()
	var marks []string
	md := newTestDispatcher(fc, fd, fl, &marks)

	d := sampleDispatch()
	if err := md.DispatchMention(context.Background(), d); err == nil {
		t.Fatal("expected DispatchMention to surface the dispatch failure")
	}
	if !fl.released[ledgerKey(d.MessageID, d.AgentName)] {
		t.Fatal("claim was not released after a mint failure — a retry would be wrongly deduped")
	}
}
