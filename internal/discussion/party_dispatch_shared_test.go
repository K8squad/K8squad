package discussion

// ISI-5688 — the shared party-facilitator dispatch hook (DispatchPartyMentionsFrom) must stamp the round
// roster off EITHER room write edge, driven by the caller-supplied authorIsCoordinator signal rather than a
// roster Coordinator flag (the MCP edge's name-only roster has none). These cases ride the unit lane: no
// database, the party-session seam is a fake so the hook's decision + SetRoundFacilitatorMessage call are
// asserted directly.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// fakePartyStore is an in-memory PartyDispatchStore: it returns a canned active session and records every
// SetRoundFacilitatorMessage call so a test can assert the round roster was (or was not) stamped.
type fakePartyStore struct {
	sess     *PartySession
	getErr   error
	won      bool
	setErr   error
	setCalls []stampCall
}

type stampCall struct {
	id     uuid.UUID
	round  int
	msgID  uuid.UUID
	voices []string
}

func (f *fakePartyStore) ActivePartySession(_ context.Context, _ uuid.UUID) (*PartySession, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.sess, nil
}

func (f *fakePartyStore) SetRoundFacilitatorMessage(_ context.Context, id uuid.UUID, forRound int, messageID uuid.UUID, voices []string) (*PartySession, bool, error) {
	f.setCalls = append(f.setCalls, stampCall{id: id, round: forRound, msgID: messageID, voices: voices})
	return f.sess, f.won, f.setErr
}

func activeTestSession() *PartySession {
	return &PartySession{
		ID:    uuid.New(),
		Round: 1,
		Phase: PartyPhaseActive,
		Budget: PartyBudget{
			MaxRounds:         3,
			MaxVoicesPerRound: 4,
			PaidRunBudget:     10,
		},
		PaidRunsUsed: 1,
	}
}

// partyDispatchRoster is a coordinator (sam) plus two dispatchable voices (uma, john). The REST edge would carry the
// Coordinator flag here; the MCP edge would not — the hook does not read it, it takes authorIsCoordinator.
func partyDispatchRoster() []TeamAgent {
	return []TeamAgent{
		{Name: "sam", Coordinator: true},
		{Name: "uma"},
		{Name: "john"},
	}
}

func TestDispatchPartyMentionsFromStampsRosterWhenCoordinator(t *testing.T) {
	store := &fakePartyStore{sess: activeTestSession(), won: true}
	disp := &fakeDispatcher{}
	msg := agentMsg("Round 1: @uma then @john", "party", "sam", nil)

	handled := DispatchPartyMentionsFrom(context.Background(), disp, store, "squad-a/proj",
		AuthorContext{Principal: "ksquad-intake", AgentID: strptr("sam")}, msg, partyDispatchRoster(), true)

	if !handled {
		t.Fatalf("expected the party hook to handle a coordinator facilitator post")
	}
	if len(store.setCalls) != 1 {
		t.Fatalf("SetRoundFacilitatorMessage called %d times, want 1", len(store.setCalls))
	}
	got := store.setCalls[0]
	if got.msgID != msg.ID || got.round != 1 {
		t.Fatalf("stamped round=%d msg=%s, want round=1 msg=%s", got.round, got.msgID, msg.ID)
	}
	if len(got.voices) != 2 || got.voices[0] != "uma" || got.voices[1] != "john" {
		t.Fatalf("stamped voices %v, want [uma john] in @-mention order", got.voices)
	}
	// The voices are NOT dispatched here — the advancer sequences them (ADR-0031 Ruling A).
	if len(disp.calls) != 0 {
		t.Fatalf("hook dispatched %d voices, want 0 (advancer sequences)", len(disp.calls))
	}
}

func TestDispatchPartyMentionsFromFallsThroughWhenNotCoordinator(t *testing.T) {
	store := &fakePartyStore{sess: activeTestSession(), won: true}
	msg := agentMsg("@uma @john", "party", "uma", nil) // a voice, not the coordinator

	handled := DispatchPartyMentionsFrom(context.Background(), &fakeDispatcher{}, store, "squad-a/proj",
		AuthorContext{Principal: "ksquad-intake", AgentID: strptr("uma")}, msg, partyDispatchRoster(), false)

	if handled {
		t.Fatalf("expected the hook to fall through for a non-coordinator post")
	}
	if len(store.setCalls) != 0 {
		t.Fatalf("SetRoundFacilitatorMessage called %d times for a non-coordinator, want 0", len(store.setCalls))
	}
}

func TestDispatchPartyMentionsFromFallsThroughWithNoActiveSession(t *testing.T) {
	store := &fakePartyStore{getErr: errors.New("no active party session")}
	msg := agentMsg("@uma @john", "party", "sam", nil)

	handled := DispatchPartyMentionsFrom(context.Background(), &fakeDispatcher{}, store, "squad-a/proj",
		AuthorContext{Principal: "ksquad-intake", AgentID: strptr("sam")}, msg, partyDispatchRoster(), true)

	if handled {
		t.Fatalf("expected the hook to fall through when there is no active party session")
	}
	if len(store.setCalls) != 0 {
		t.Fatalf("SetRoundFacilitatorMessage called %d times with no session, want 0", len(store.setCalls))
	}
}
