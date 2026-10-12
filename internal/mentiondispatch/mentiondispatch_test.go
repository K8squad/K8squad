package mentiondispatch

// Run-minting coverage for dispatch-on-mention (ISI-5116; relocated from internal/apiserver in ISI-5125).
// The dispatcher is exercised against fakes — no Postgres — so these ride the default unit lane. The
// DB-backed guarantees (board-hide filter, (message,agent) idempotency at the SQL layer) are covered by
// the chaos-tagged contract tests.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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

	// ISI-5670: the in-flight count the per-discussion backstop gates on. countErr forces the fail-closed
	// path; countCalls records how CountInFlight was invoked so a test can assert the staleness window.
	inFlight   int
	countErr   error
	countCalls []countCall
}

type countCall struct {
	thread    uuid.UUID
	staleness time.Duration
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

func (l *fakeLedger) CountInFlight(_ context.Context, thread uuid.UUID, staleness time.Duration) (int, error) {
	l.countCalls = append(l.countCalls, countCall{thread: thread, staleness: staleness})
	if l.countErr != nil {
		return 0, l.countErr
	}
	return l.inFlight, nil
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

// ISI-5283 / ISI-5267 WS-2: an orchestration dispatch mints ONE drive item for the coordinator whose
// body carries the structured directive — the mentioned agents and the verbatim request — pointing it
// at its LIVE ISI-5220 orchestration verbs.
func TestMentionDispatchOrchestrationDirective(t *testing.T) {
	fc := &fakeCreate{rec: coord.WorkItemRecord{ID: "66666666-6666-6666-6666-666666666666", State: "backlog"}}
	fd := &fakeDispatch{}
	fl := newFakeLedger()
	var marks []string
	md := newTestDispatcher(fc, fd, fl, &marks)

	d := sampleDispatch()
	d.AgentName = "coord"
	d.Orchestrate = true
	d.OrchestratedAgents = []string{"john", "jane"}
	d.RequestBody = "@john @jane ship the login page"
	if err := md.DispatchMention(context.Background(), d); err != nil {
		t.Fatalf("DispatchMention: %v", err)
	}

	if len(fc.calls) != 1 || len(fd.calls) != 1 {
		t.Fatalf("orchestration must mint exactly one Run: creates=%d dispatches=%d", len(fc.calls), len(fd.calls))
	}
	if fd.calls[0].AgentID != "coord" {
		t.Fatalf("dispatch agent = %q, want the coordinator 'coord'", fd.calls[0].AgentID)
	}
	body := fc.calls[0].Body
	for _, want := range []string{"Team Coordinator", "@john", "@jane", "STRUCTURE", "work_item_create", "work_item_assign", d.RequestBody} {
		if !strings.Contains(body, want) {
			t.Fatalf("orchestration body missing %q\n--- body ---\n%s", want, body)
		}
	}
}

// ISI-5586 WS-C: a party-mode voice dispatch renders the cross-talk context + persona prompt into the
// work-item body (persona self-framing, rolling summary, "What Others Said This Round", disagree/pass
// guidelines) AND keeps the read-thread / reply-in-room / hop footer.
func TestMentionDispatchPartyVoiceBody(t *testing.T) {
	fc := &fakeCreate{rec: coord.WorkItemRecord{ID: "77777777-7777-7777-7777-777777777777", State: "backlog"}}
	fd := &fakeDispatch{}
	fl := newFakeLedger()
	var marks []string
	md := newTestDispatcher(fc, fd, fl, &marks)

	d := sampleDispatch()
	d.AgentName = "ada"
	d.Party = &discussion.PartyContext{
		Round:          2,
		Topic:          "which storage engine",
		Persona:        discussion.PersonaBlurb{Name: "Ada", Role: "Engineer", Icon: "⚙"},
		RollingSummary: "Round 1: Winston argued Postgres.",
		PeersThisRound: []discussion.PartyPeerTurn{
			{Persona: discussion.PersonaBlurb{Name: "Winston", Role: "Architect"}, Body: "Commit to Postgres."},
		},
	}
	if err := md.DispatchMention(context.Background(), d); err != nil {
		t.Fatalf("DispatchMention: %v", err)
	}
	if len(fc.calls) != 1 || len(fd.calls) != 1 {
		t.Fatalf("party voice must mint exactly one Run: creates=%d dispatches=%d", len(fc.calls), len(fd.calls))
	}
	body := fc.calls[0].Body
	for _, want := range []string{
		"Ada (Engineer)",                                      // persona self-framing
		"What Others Said This Round",                         // cross-talk header
		"Winston (Architect): Commit to Postgres.",            // peer turn verbatim
		"Round 1: Winston argued Postgres.",                   // rolling summary
		discussion.DisagreeMandate,                            // persona mandate
		discussion.PassPermission,                             // permission to pass
		"POST /api/projects/squad-a/proj/discussion/threads/", // reply-in-room footer
		"party round=2",                                       // provenance footer
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("party voice body missing %q\n--- body ---\n%s", want, body)
		}
	}
	// a party voice must NOT fall through to the generic single-agent instruction.
	if strings.Contains(body, "You were @-mentioned in the discussion room") {
		t.Fatal("party voice body should not use the generic single-agent instruction")
	}
}

// --- ISI-5670: the HARD per-discussion concurrent-run backstop --------------

// capDispatcher builds a dispatcher with the per-discussion concurrent-run cap armed (the field New sets
// from discussion.MaxConcurrentRunsPerDiscussion()); newTestDispatcher leaves it zero = disabled.
func capDispatcher(fc *fakeCreate, fd *fakeDispatch, fl *fakeLedger, marks *[]string, cap int) *dispatcher {
	d := newTestDispatcher(fc, fd, fl, marks)
	d.maxInFlight = cap
	d.inFlightStaleness = discussion.DefaultPartyVoiceSettleTimeout
	return d
}

// Under the ceiling, a dispatch mints exactly as before and consults the in-flight count first.
func TestDispatchUnderConcurrencyCapMints(t *testing.T) {
	fc := &fakeCreate{rec: coord.WorkItemRecord{ID: "33333333-3333-3333-3333-333333333333", State: "backlog"}}
	fd := &fakeDispatch{}
	fl := newFakeLedger()
	fl.inFlight = 3 // below the cap of 4
	var marks []string
	md := capDispatcher(fc, fd, fl, &marks, 4)

	d := sampleDispatch()
	if err := md.DispatchMention(context.Background(), d); err != nil {
		t.Fatalf("DispatchMention: %v", err)
	}
	if len(fc.calls) != 1 || len(fd.calls) != 1 {
		t.Fatalf("under-cap dispatch must mint one Run: creates=%d dispatches=%d", len(fc.calls), len(fd.calls))
	}
	if len(fl.countCalls) != 1 {
		t.Fatalf("CountInFlight called %d times, want 1 (the backstop gate)", len(fl.countCalls))
	}
	if fl.countCalls[0].thread != d.ThreadID {
		t.Fatalf("CountInFlight thread = %s, want the dispatch thread %s", fl.countCalls[0].thread, d.ThreadID)
	}
	if fl.countCalls[0].staleness != discussion.DefaultPartyVoiceSettleTimeout {
		t.Fatalf("CountInFlight staleness = %s, want the settle-timeout window %s",
			fl.countCalls[0].staleness, discussion.DefaultPartyVoiceSettleTimeout)
	}
}

// AT the ceiling, the dispatch is SHED: no claim, no work item, no RequestDispatch — the hard backstop.
func TestDispatchAtConcurrencyCapSheds(t *testing.T) {
	fc := &fakeCreate{rec: coord.WorkItemRecord{ID: "44444444-4444-4444-4444-444444444444", State: "backlog"}}
	fd := &fakeDispatch{}
	fl := newFakeLedger()
	fl.inFlight = 4 // exactly at the cap of 4
	var marks []string
	md := capDispatcher(fc, fd, fl, &marks, 4)

	d := sampleDispatch()
	if err := md.DispatchMention(context.Background(), d); err != nil {
		t.Fatalf("a shed dispatch is a no-op, not an error: %v", err)
	}
	if len(fc.calls) != 0 || len(fd.calls) != 0 {
		t.Fatalf("at-cap dispatch must mint NOTHING: creates=%d dispatches=%d", len(fc.calls), len(fd.calls))
	}
	if len(fl.seen) != 0 {
		t.Fatalf("at-cap dispatch must not claim a ledger row (checked before Claim): seen=%v", fl.seen)
	}
}

// Over the ceiling (a count racing above the cap) sheds the same way — the comparison is >=.
func TestDispatchOverConcurrencyCapSheds(t *testing.T) {
	fc := &fakeCreate{rec: coord.WorkItemRecord{ID: "55555555-5555-5555-5555-555555555555", State: "backlog"}}
	fd := &fakeDispatch{}
	fl := newFakeLedger()
	fl.inFlight = 9
	var marks []string
	md := capDispatcher(fc, fd, fl, &marks, 4)

	if err := md.DispatchMention(context.Background(), sampleDispatch()); err != nil {
		t.Fatalf("shed dispatch: %v", err)
	}
	if len(fc.calls) != 0 || len(fd.calls) != 0 {
		t.Fatalf("over-cap dispatch must mint nothing: creates=%d dispatches=%d", len(fc.calls), len(fd.calls))
	}
}

// A count error fails CLOSED: the backstop refuses to mint when it cannot prove headroom.
func TestDispatchConcurrencyCapFailsClosedOnCountError(t *testing.T) {
	fc := &fakeCreate{rec: coord.WorkItemRecord{ID: "66666666-6666-6666-6666-666666666666", State: "backlog"}}
	fd := &fakeDispatch{}
	fl := newFakeLedger()
	fl.countErr = errors.New("boom")
	var marks []string
	md := capDispatcher(fc, fd, fl, &marks, 4)

	if err := md.DispatchMention(context.Background(), sampleDispatch()); err == nil {
		t.Fatal("a count error must fail closed (return an error), not mint")
	}
	if len(fc.calls) != 0 || len(fd.calls) != 0 {
		t.Fatalf("fail-closed must mint nothing: creates=%d dispatches=%d", len(fc.calls), len(fd.calls))
	}
}

// With the cap disabled (zero value, as in the pre-ISI-5670 constructor path), the count is never even
// consulted and dispatch is unbounded — the cap is strictly additive behaviour.
func TestDispatchConcurrencyCapDisabledNeverCounts(t *testing.T) {
	fc := &fakeCreate{rec: coord.WorkItemRecord{ID: "77777777-7777-7777-7777-777777777777", State: "backlog"}}
	fd := &fakeDispatch{}
	fl := newFakeLedger()
	fl.inFlight = 100
	var marks []string
	md := newTestDispatcher(fc, fd, fl, &marks) // maxInFlight == 0

	if err := md.DispatchMention(context.Background(), sampleDispatch()); err != nil {
		t.Fatalf("DispatchMention: %v", err)
	}
	if len(fc.calls) != 1 || len(fd.calls) != 1 {
		t.Fatalf("cap-disabled dispatch must mint: creates=%d dispatches=%d", len(fc.calls), len(fd.calls))
	}
	if len(fl.countCalls) != 0 {
		t.Fatalf("cap-disabled dispatch must never call CountInFlight, got %d calls", len(fl.countCalls))
	}
}
