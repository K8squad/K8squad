package discussion

// Reply-path loop-guard coverage (ISI-5116). The run-minting half wires a trusted, ledger-derived hop
// stamp onto an agent-authored reply so an agent→agent chain stays bounded end-to-end WITHOUT trusting
// the agent to echo the number. These ride the default unit lane (no DB) — the ledger is faked, exactly
// as the sibling dispatch tests fake the dispatcher.

import (
	"context"
	"testing"
)

// fakeHopResolver stands in for the apiserver dispatch ledger: it returns a fixed (hop, ok) so the
// stamp decision is exercised without Postgres.
type fakeHopResolver struct {
	hop int
	ok  bool
	err error
}

func (f fakeHopResolver) HopForDispatchedRun(_ context.Context, _ string) (int, bool, error) {
	return f.hop, f.ok, f.err
}

func agentRunAuth(agentID, runID string) AuthorContext {
	a := AuthorContext{Principal: "run:" + runID}
	if agentID != "" {
		id := agentID
		a.AgentID = &id
	}
	if runID != "" {
		rid := runID
		a.RunID = &rid
	}
	return a
}

// stampReplyHop stamps the resolved hop for an agent-authored reply from within a dispatched run.
func TestStampReplyHopStampsAgentRun(t *testing.T) {
	h := &Handler{}
	h.SetReplyHopResolver(fakeHopResolver{hop: 1, ok: true})

	got := h.stampReplyHop(context.Background(), agentRunAuth("agent-coder", "run-1"), nil)
	if got == nil {
		t.Fatal("expected a stamped payload, got nil")
	}
	if hop := DispatchHopOf(got); hop != 1 {
		t.Fatalf("stamped hop = %d, want 1", hop)
	}
}

// A human post (no agent id) is never stamped — a human turn is always hop 0, so the resolver must not
// even be consulted (here it would falsely return hop 1 if it were).
func TestStampReplyHopHumanPassThrough(t *testing.T) {
	h := &Handler{}
	h.SetReplyHopResolver(fakeHopResolver{hop: 1, ok: true})

	if got := h.stampReplyHop(context.Background(), AuthorContext{Principal: "user:me"}, nil); got != nil {
		t.Fatalf("human post payload was modified (%s), want pass-through nil", *got)
	}
}

// A run that is not a dispatch-on-mention thread-run (resolver ok=false) leaves the payload untouched —
// a normal agent post stays hop 0.
func TestStampReplyHopNonThreadRunPassThrough(t *testing.T) {
	h := &Handler{}
	h.SetReplyHopResolver(fakeHopResolver{ok: false})

	if got := h.stampReplyHop(context.Background(), agentRunAuth("agent-x", "run-x"), nil); got != nil {
		t.Fatalf("non-thread-run post was stamped (%s), want pass-through nil", *got)
	}
}

// A resolver error degrades to no stamp (best-effort) — the reply is already valid.
func TestStampReplyHopResolverErrorPassThrough(t *testing.T) {
	h := &Handler{}
	h.SetReplyHopResolver(fakeHopResolver{hop: 2, ok: true, err: context.DeadlineExceeded})

	if got := h.stampReplyHop(context.Background(), agentRunAuth("agent-x", "run-x"), nil); got != nil {
		t.Fatalf("errored resolver still stamped (%s), want pass-through nil", *got)
	}
}

// (b) end-to-end reply half: a run dispatched at hop 1 replies with an @-mention → the trusted stamp
// makes that reply hop 1 → resolveMentionTargets dispatches the next agent at hop 2 (still allowed). A
// run dispatched at hop 2 replies with an @-mention → stamped hop 2 → the next hop would be 3, which is
// refused. The chain is bounded without any cooperation from the agent's own payload.
func TestReplyHopBoundsAgentChainEndToEnd(t *testing.T) {
	roster := []TeamAgent{
		{Name: "reviewer", Status: "working"},
		{Name: "coder", Status: "working"},
	}

	// hop-1 run replies "@reviewer" → dispatch at hop 2.
	h1 := &Handler{}
	h1.SetReplyHopResolver(fakeHopResolver{hop: 1, ok: true})
	payload1 := h1.stampReplyHop(context.Background(), agentRunAuth("coder", "run-hop1"), nil)
	reply1 := agentMsg("@reviewer please take a look", "party", "coder", payload1)
	targets, _ := resolveMentionTargets(reply1, roster)
	if len(targets) != 1 || targets[0].AgentName != "reviewer" {
		t.Fatalf("hop-1 reply dispatched %v, want exactly [reviewer]", names(targets))
	}
	if targets[0].HopDepth != 2 {
		t.Fatalf("hop-1 reply's dispatch runs at hop %d, want 2", targets[0].HopDepth)
	}

	// hop-2 run replies "@coder" → would be hop 3 → refused (the paid-loop backstop).
	h2 := &Handler{}
	h2.SetReplyHopResolver(fakeHopResolver{hop: 2, ok: true})
	payload2 := h2.stampReplyHop(context.Background(), agentRunAuth("reviewer", "run-hop2"), nil)
	reply2 := agentMsg("@coder back to you", "party", "reviewer", payload2)
	bounded, _ := resolveMentionTargets(reply2, roster)
	if len(bounded) != 0 {
		t.Fatalf("hop-2 reply dispatched %v, want the chain bounded (nobody at hop 3)", names(bounded))
	}
}
