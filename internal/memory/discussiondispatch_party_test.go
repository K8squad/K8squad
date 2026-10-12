package memory

// ISI-5688 — the MCP discussion_post edge must fire the party-facilitator roster stamp (parity with the
// apiserver REST handler), driven by the server-authenticated CapabilityCoordinator marker the run token
// carries. Before this, DiscussionDispatch.dispatchMentions ran only the plain DispatchMentionsFrom, so a
// facilitator posting its round directive through the MCP tool never stamped RoundVoices and the party
// round stalled with zero voices. These cases ride the unit lane: the party-session seam is a fake, so the
// stamp decision is asserted directly without a database.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
)

// fakeMemPartyStore is an in-memory discussion.PartyDispatchStore recording whether the round roster was
// stamped (SetRoundFacilitatorMessage) and with which voices.
type fakeMemPartyStore struct {
	sess      *discussion.PartySession
	setCalled int
	setVoices []string
}

func (f *fakeMemPartyStore) ActivePartySession(_ context.Context, _ uuid.UUID) (*discussion.PartySession, error) {
	return f.sess, nil
}

func (f *fakeMemPartyStore) SetRoundFacilitatorMessage(_ context.Context, _ uuid.UUID, _ int, _ uuid.UUID, voices []string) (*discussion.PartySession, bool, error) {
	f.setCalled++
	f.setVoices = voices
	return f.sess, true, nil
}

func activeMemSession() *discussion.PartySession {
	return &discussion.PartySession{
		ID:    uuid.New(),
		Round: 1,
		Phase: discussion.PartyPhaseActive,
		Budget: discussion.PartyBudget{
			MaxRounds:         3,
			MaxVoicesPerRound: 4,
			PaidRunBudget:     10,
		},
		PaidRunsUsed: 1,
	}
}

func facilitatorMsg() *discussion.Message {
	sam := "sam"
	return &discussion.Message{
		ID:            uuid.New(),
		ThreadID:      uuid.New(),
		Body:          "Round 1: @uma then @john",
		Audience:      "party",
		AuthorAgentID: &sam,
	}
}

func TestMCPDispatchStampsPartyRosterWhenCoordinatorCapabilityPresent(t *testing.T) {
	store := &fakeMemPartyStore{sess: activeMemSession()}
	disp := &captureDispatcher{}
	dd := NewDiscussionDispatch(disp, fixedHopResolver{}, staticRoster{names: []string{"sam", "uma", "john"}}, nil).
		WithPartyStore(store)

	sam := "sam"
	auth := discussion.AuthorContext{Principal: "ksquad-intake", TeamID: uuid.New(), AgentID: &sam}
	// authorIsCoordinator=true models the token carrying CapabilityCoordinator.
	dd.dispatchMentions(context.Background(), "squad-a/proj", auth, facilitatorMsg(), true)

	if store.setCalled != 1 {
		t.Fatalf("SetRoundFacilitatorMessage called %d times, want 1 (coordinator facilitator post)", store.setCalled)
	}
	if len(store.setVoices) != 2 || store.setVoices[0] != "uma" || store.setVoices[1] != "john" {
		t.Fatalf("stamped voices %v, want [uma john]", store.setVoices)
	}
	// The party hook handled the post — the normal mention dispatch must NOT also fire.
	if len(disp.calls) != 0 {
		t.Fatalf("normal dispatch fired %d times after party hook handled the post, want 0", len(disp.calls))
	}
}

func TestMCPDispatchSkipsPartyRosterWithoutCoordinatorCapability(t *testing.T) {
	store := &fakeMemPartyStore{sess: activeMemSession()}
	dd := NewDiscussionDispatch(&captureDispatcher{}, fixedHopResolver{}, staticRoster{names: []string{"sam", "uma", "john"}}, nil).
		WithPartyStore(store)

	sam := "sam"
	auth := discussion.AuthorContext{Principal: "ksquad-intake", TeamID: uuid.New(), AgentID: &sam}
	// authorIsCoordinator=false: no coordinator marker ⇒ party path inert, falls through to normal dispatch.
	dd.dispatchMentions(context.Background(), "squad-a/proj", auth, facilitatorMsg(), false)

	if store.setCalled != 0 {
		t.Fatalf("SetRoundFacilitatorMessage called %d times without the coordinator marker, want 0", store.setCalled)
	}
}

func TestSessionHasCoordinatorCapability(t *testing.T) {
	if !SessionHasCoordinatorCapability([]string{"discussion", CoordinatorCapability}) {
		t.Fatalf("expected the coordinator marker to be detected in the capability set")
	}
	if SessionHasCoordinatorCapability([]string{"discussion", "work_item.author"}) {
		t.Fatalf("did not expect a coordinator marker in a non-coordinator capability set")
	}
	if SessionHasCoordinatorCapability(nil) {
		t.Fatalf("empty capability set must not report a coordinator marker")
	}
}
