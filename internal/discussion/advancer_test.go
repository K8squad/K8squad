// ISI-5615 (ISI-5569 WS-D.1) — exhaustive branch coverage for the pure party advancer decision.
package discussion

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// activeSession builds an active party session with the given round + budget state for plan tests.
func activeSession(round, maxRounds, maxVoices, budget, used int) PartySession {
	return PartySession{
		Phase:        PartyPhaseActive,
		Round:        round,
		Budget:       PartyBudget{MaxRounds: maxRounds, MaxVoicesPerRound: maxVoices, PaidRunBudget: budget},
		PaidRunsUsed: used,
	}
}

func ptr[T any](v T) *T { return &v }

func TestPlanPartyTick_NotActiveIsInert(t *testing.T) {
	for _, phase := range []string{PartyPhaseClosed, PartyPhaseConverged, PartyPhaseBudgetExhausted} {
		s := activeSession(1, 3, 3, 12, 4)
		s.Phase = phase
		got := PlanPartyTick(s, RoundSettlement{}, time.Now(), DefaultPartyFacilitatorPostTimeout)
		if got.Action != PartyTickWait {
			t.Fatalf("phase=%s: want Wait, got %v", phase, got.Action)
		}
	}
}

func TestPlanPartyTick_FreshSessionKicksRoundOne(t *testing.T) {
	s := activeSession(0, 3, 3, 12, 0) // never started
	got := PlanPartyTick(s, RoundSettlement{}, time.Now(), DefaultPartyFacilitatorPostTimeout)
	if got.Action != PartyTickMintRound || got.FromRound != 0 {
		t.Fatalf("want MintRound FromRound=0, got action=%v fromRound=%d", got.Action, got.FromRound)
	}
}

func TestPlanPartyTick_AwaitingFacilitatorPost(t *testing.T) {
	s := activeSession(1, 3, 3, 12, 1) // facilitator minted (round=1), no dispatch message yet
	s.RoundStartedAt = ptr(time.Now().Add(-time.Minute))
	got := PlanPartyTick(s, RoundSettlement{}, time.Now(), DefaultPartyFacilitatorPostTimeout)
	if got.Action != PartyTickWait {
		t.Fatalf("want Wait while awaiting facilitator post, got %v", got.Action)
	}
}

func TestPlanPartyTick_FacilitatorPostTimeoutCloses(t *testing.T) {
	s := activeSession(1, 3, 3, 12, 1)
	s.RoundStartedAt = ptr(time.Now().Add(-30 * time.Minute)) // long overdue
	got := PlanPartyTick(s, RoundSettlement{}, time.Now(), DefaultPartyFacilitatorPostTimeout)
	if got.Action != PartyTickClose || got.Reason != PartyPhaseClosed {
		t.Fatalf("want Close/closed on facilitator-post timeout, got action=%v reason=%q", got.Action, got.Reason)
	}
}

func TestPlanPartyTick_NoTimeoutWhenRoundStartedAtNil(t *testing.T) {
	s := activeSession(1, 3, 3, 12, 1) // RoundStartedAt nil → the timeout cannot fire
	got := PlanPartyTick(s, RoundSettlement{}, time.Now(), DefaultPartyFacilitatorPostTimeout)
	if got.Action != PartyTickWait {
		t.Fatalf("want Wait when RoundStartedAt is nil, got %v", got.Action)
	}
}

func TestPlanPartyTick_AwaitingVoiceSettlement(t *testing.T) {
	s := activeSession(1, 3, 3, 12, 4)
	s.CurrentRoundMessageID = ptr(uuid.New())
	settle := RoundSettlement{Dispatched: 3, Settled: 1} // not complete
	got := PlanPartyTick(s, settle, time.Now(), DefaultPartyFacilitatorPostTimeout)
	if got.Action != PartyTickWait {
		t.Fatalf("want Wait while voices unsettled, got %v", got.Action)
	}
}

func TestPlanPartyTick_ProductiveRoundAdvances(t *testing.T) {
	s := activeSession(1, 3, 3, 12, 4) // round 1 of 3, plenty of budget
	s.CurrentRoundMessageID = ptr(uuid.New())
	settle := RoundSettlement{Dispatched: 3, Settled: 3} // complete, productive
	got := PlanPartyTick(s, settle, time.Now(), DefaultPartyFacilitatorPostTimeout)
	if got.Action != PartyTickMintRound || got.FromRound != 1 {
		t.Fatalf("want MintRound FromRound=1, got action=%v fromRound=%d", got.Action, got.FromRound)
	}
}

func TestPlanPartyTick_MaxRoundsReachedGoesToTakeaways(t *testing.T) {
	s := activeSession(3, 3, 3, 12, 10) // last round complete, headroom for a takeaways run
	s.CurrentRoundMessageID = ptr(uuid.New())
	settle := RoundSettlement{Dispatched: 3, Settled: 3}
	got := PlanPartyTick(s, settle, time.Now(), DefaultPartyFacilitatorPostTimeout)
	if got.Action != PartyTickTakeaways || got.Reason != PartyPhaseClosed {
		t.Fatalf("want Takeaways/closed at max rounds, got action=%v reason=%q", got.Action, got.Reason)
	}
}

func TestPlanPartyTick_ConvergedGoesToTakeaways(t *testing.T) {
	s := activeSession(1, 3, 3, 12, 4) // rounds + budget remain, but the round was all-silent → converged
	s.CurrentRoundMessageID = ptr(uuid.New())
	settle := RoundSettlement{Dispatched: 3, Settled: 0, TimedOut: 3} // complete + all-silent
	if !settle.AllSilent() {
		t.Fatal("precondition: settlement should be all-silent")
	}
	got := PlanPartyTick(s, settle, time.Now(), DefaultPartyFacilitatorPostTimeout)
	if got.Action != PartyTickTakeaways || got.Reason != PartyPhaseConverged {
		t.Fatalf("want Takeaways/converged on all-silent round, got action=%v reason=%q", got.Action, got.Reason)
	}
}

func TestPlanPartyTick_BudgetExhaustedClosesColdNoTakeaways(t *testing.T) {
	s := activeSession(3, 3, 3, 12, 12) // ceiling reached — not even a takeaways run is affordable
	s.CurrentRoundMessageID = ptr(uuid.New())
	settle := RoundSettlement{Dispatched: 3, Settled: 3}
	got := PlanPartyTick(s, settle, time.Now(), DefaultPartyFacilitatorPostTimeout)
	if got.Action != PartyTickClose || got.Reason != PartyPhaseBudgetExhausted {
		t.Fatalf("want Close/budget_exhausted at the hard ceiling, got action=%v reason=%q", got.Action, got.Reason)
	}
}

func TestPlanPartyTick_BudgetHardStopBeatsConvergence(t *testing.T) {
	// A converged round with no paid-run headroom still closes cold as budget_exhausted — the hard ceiling
	// beats a courtesy takeaways run (ADR-0027 §5.1).
	s := activeSession(2, 3, 3, 12, 12)
	s.CurrentRoundMessageID = ptr(uuid.New())
	settle := RoundSettlement{Dispatched: 2, Settled: 0, TimedOut: 2} // all-silent → converged
	got := PlanPartyTick(s, settle, time.Now(), DefaultPartyFacilitatorPostTimeout)
	if got.Action != PartyTickClose || got.Reason != PartyPhaseBudgetExhausted {
		t.Fatalf("want Close/budget_exhausted (hard stop beats converge), got action=%v reason=%q", got.Action, got.Reason)
	}
}

func TestPlanPartyTick_ZeroDispatchRoundIsComplete(t *testing.T) {
	// A round whose facilitator dispatched nobody (VoicesAllowedThisRound capped to 0) is trivially
	// complete; with rounds + budget remaining the advancer still advances rather than wedging.
	s := activeSession(1, 3, 3, 12, 2)
	s.CurrentRoundMessageID = ptr(uuid.New())
	settle := RoundSettlement{Dispatched: 0}
	if !settle.Complete() {
		t.Fatal("precondition: a zero-dispatch round is complete")
	}
	got := PlanPartyTick(s, settle, time.Now(), DefaultPartyFacilitatorPostTimeout)
	if got.Action != PartyTickMintRound {
		t.Fatalf("want MintRound after a zero-dispatch productive round, got %v", got.Action)
	}
}
