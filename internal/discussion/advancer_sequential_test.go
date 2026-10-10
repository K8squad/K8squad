// ISI-5638 (ISI-5624 C1, ruling ADR-0031 Ruling A §3.1) — unit coverage for the coordinator-SEQUENCED
// party advancer: one voice in flight at a time, each reacting to the prior turn, same paid-run budget.
//
// These are pure tests (no DB, no coord) over the shipped decision layer plus the cross-talk assembly:
//   - PlanPartyTick walks the round's RoundVoices roster one turn at a time, dispatching turn k only after
//     turn k-1 settles (one-in-flight), and advances the round only after the LAST turn settles.
//   - A dispatched voice's rendered context includes the EARLIER in-round turns (react-to-prior), not only
//     the prior round's.
//   - The paid-run budget of a 4-voice × 3-round debate is IDENTICAL to ADR-0027's parallel fan-out:
//     sequencing adds latency, not runs (one facilitator mint per ROUND, not per turn — the S-collapse 2×
//     failure mode is explicitly ruled out).
package discussion

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// seqSession builds an active session whose round-1 facilitator has already posted a `voices`-long roster
// (CurrentRoundMessageID set), ready for the advancer to sequence.
func seqSession(round, maxRounds, maxVoices, budget, used int, voices ...string) PartySession {
	s := activeSession(round, maxRounds, maxVoices, budget, used)
	s.CurrentRoundMessageID = ptr(uuid.New())
	s.RoundVoices = voices
	return s
}

// TestPlanPartyTick_SequentialCursorAdvancesOnSettle proves the turn cursor: with a 3-voice roster, the
// advancer dispatches voice 0 first, WAITS while it is in flight, dispatches voice 1 only once voice 0 has
// settled, and so on — at most one voice in flight at a time (ADR-0031 §3.1).
func TestPlanPartyTick_SequentialCursorAdvancesOnSettle(t *testing.T) {
	s := seqSession(1, 3, 4, 20, 1, "winston", "sally", "amelia")
	now, post := time.Now(), DefaultPartyFacilitatorPostTimeout

	// (a) No voice dispatched yet → dispatch voice 0 (the first turn).
	if got := PlanPartyTick(s, RoundSettlement{Dispatched: 0}, now, post); got.Action != PartyTickDispatchVoice || got.VoiceIndex != 0 {
		t.Fatalf("turn 0: want DispatchVoice idx=0, got action=%v idx=%d", got.Action, got.VoiceIndex)
	}

	// (b) Voice 0 dispatched but still in flight (not settled) → WAIT (one-in-flight, no fan-out).
	if got := PlanPartyTick(s, RoundSettlement{Dispatched: 1, Settled: 0}, now, post); got.Action != PartyTickWait {
		t.Fatalf("turn 0 in flight: want Wait, got %v (%s)", got.Action, got.Note)
	}

	// (c) Voice 0 settled → dispatch voice 1 (reacting to voice 0's just-landed turn).
	if got := PlanPartyTick(s, RoundSettlement{Dispatched: 1, Settled: 1}, now, post); got.Action != PartyTickDispatchVoice || got.VoiceIndex != 1 {
		t.Fatalf("turn 1: want DispatchVoice idx=1, got action=%v idx=%d", got.Action, got.VoiceIndex)
	}

	// (d) A stuck voice that TIMED OUT still counts terminal, so the cursor advances (no wedge).
	if got := PlanPartyTick(s, RoundSettlement{Dispatched: 2, Settled: 1, TimedOut: 1}, now, post); got.Action != PartyTickDispatchVoice || got.VoiceIndex != 2 {
		t.Fatalf("turn 2 after timeout: want DispatchVoice idx=2, got action=%v idx=%d", got.Action, got.VoiceIndex)
	}

	// (e) Last voice in flight → WAIT (all roster voices dispatched, but the round is not complete yet).
	if got := PlanPartyTick(s, RoundSettlement{Dispatched: 3, Settled: 2}, now, post); got.Action != PartyTickWait {
		t.Fatalf("last turn in flight: want Wait, got %v (%s)", got.Action, got.Note)
	}

	// (f) All 3 roster voices dispatched AND settled → the round advances (productive → next round).
	got := PlanPartyTick(s, RoundSettlement{Dispatched: 3, Settled: 3}, now, post)
	if got.Action != PartyTickMintRound || got.FromRound != 1 {
		t.Fatalf("round complete: want MintRound fromRound=1, got action=%v fromRound=%d", got.Action, got.FromRound)
	}
}

// TestPlanPartyTick_SequentialDoesNotAdvanceMidRoster proves the fix for the core hazard: a round whose
// in-flight turns have all settled but still has UNDISPATCHED roster voices must NOT be mistaken for a
// complete round. Before ISI-5638 settle.Complete() alone drove the advance; now the roster gates it.
func TestPlanPartyTick_SequentialDoesNotAdvanceMidRoster(t *testing.T) {
	s := seqSession(1, 3, 4, 20, 1, "a", "b", "c", "d")
	// 2 of 4 dispatched, both settled → settle.Complete() is true, but 2 voices have never run.
	settle := RoundSettlement{Dispatched: 2, Settled: 2}
	if !settle.Complete() {
		t.Fatal("precondition: all dispatched-so-far settled → Complete() true")
	}
	got := PlanPartyTick(s, settle, time.Now(), DefaultPartyFacilitatorPostTimeout)
	if got.Action != PartyTickDispatchVoice || got.VoiceIndex != 2 {
		t.Fatalf("mid-roster: want DispatchVoice idx=2 (NOT advance), got action=%v idx=%d", got.Action, got.VoiceIndex)
	}
}

// TestPlanPartyTick_NoRosterIsRoundGranular proves backward compatibility: a session with no RoundVoices
// (pre-0035, or a degenerate zero-voice round) collapses to the ADR-0027 round-granular behaviour — the
// sequential arm never fires because Dispatched is always ≥ len(RoundVoices)=0.
func TestPlanPartyTick_NoRosterIsRoundGranular(t *testing.T) {
	s := activeSession(1, 3, 3, 12, 4)
	s.CurrentRoundMessageID = ptr(uuid.New()) // facilitator posted, but no roster stamped (pre-0035)
	settle := RoundSettlement{Dispatched: 3, Settled: 3}
	got := PlanPartyTick(s, settle, time.Now(), DefaultPartyFacilitatorPostTimeout)
	if got.Action != PartyTickMintRound {
		t.Fatalf("no roster: want round-granular MintRound, got %v", got.Action)
	}
}

// TestSequentialVoiceContextIncludesPriorInRoundTurn proves react-to-prior at the turn grain (ADR-0031
// §3.1): the context assembled for voice N includes voice N-1's turn FROM THE SAME ROUND — exactly the
// gap ADR-0031 §2.2 named (before, a within-round voice could only react to the PRIOR round). This is the
// assembly the advancer's dispatchVoice runs before dispatching each turn.
func TestSequentialVoiceContextIncludesPriorInRoundTurn(t *testing.T) {
	winston, sally := "winston", "sally"
	// The round so far: Winston (turn 0) has posted; Sally (turn 1) is about to be dispatched.
	roundMsgs := []Message{
		{AuthorAgentID: &winston, Body: "We should shard by tenant — it bounds blast radius."},
	}
	peers := AssemblePeerTurns(roundMsgs, sally, nil)
	if len(peers) != 1 || peers[0].Persona.Name != winston {
		t.Fatalf("expected Sally to see Winston's in-round turn, got %+v", peers)
	}

	ctx := &PartyContext{Round: 2, Persona: PersonaBlurb{Name: sally}, PeersThisRound: peers}
	body := ctx.RenderVoiceContext()
	if !strings.Contains(body, "What Others Said This Round") {
		t.Fatal("rendered context missing the cross-talk block header")
	}
	if !strings.Contains(body, "shard by tenant") {
		t.Fatalf("Sally's context must quote Winston's just-landed in-round turn, got:\n%s", body)
	}
	// And a voice never has its OWN turn fed back to it.
	ownPeers := AssemblePeerTurns(append(roundMsgs, Message{AuthorAgentID: &sally, Body: "I disagree."}), sally, nil)
	if len(ownPeers) != 1 || ownPeers[0].Persona.Name != winston {
		t.Fatalf("a voice must not see its own turn as a peer, got %+v", ownPeers)
	}
}

// TestSequentialPaidRunBudgetUnchanged drives a full 4-voice × 3-round debate through the pure decision
// layer with an in-memory accounting model mirroring the advancer's charges (MintRound +1 per facilitator;
// RecordPaidRuns +Dispatched once per completed round), and asserts the total paid runs equal ADR-0027's
// parallel fan-out: 3 round-facilitators + 12 voices + 1 takeaways = 16. Crucially it asserts ONE
// facilitator mint PER ROUND — not one per turn — which is the line between S-direct (budget unchanged) and
// S-collapse (~2× runs). Sequencing adds latency, not paid runs (ADR-0031 §3.1).
func TestSequentialPaidRunBudgetUnchanged(t *testing.T) {
	const rounds, voicesPerRound = 3, 4
	// Budget sized to cover the worst case + a takeaways run (derived ceiling is rounds×voices+rounds=15;
	// +1 for the optional takeaways mint → 16), so the debate ends 'closed', not 'budget_exhausted'.
	s := PartySession{
		Phase:  PartyPhaseActive,
		Round:  0,
		Budget: PartyBudget{MaxRounds: rounds, MaxVoicesPerRound: voicesPerRound, PaidRunBudget: 16},
	}
	roster := []string{"a", "b", "c", "d"}
	now, post := time.Now(), DefaultPartyFacilitatorPostTimeout

	var settle RoundSettlement
	facilitatorMints, voiceDispatches := 0, 0
	perRoundCharges := map[int]int{}

	for step := 0; step < 1000; step++ {
		// Mirror advanceSession's charge: once every roster voice has settled, charge the round's voices.
		if s.CurrentRoundMessageID != nil {
			roundFullyDispatched := settle.Dispatched >= len(s.RoundVoices)
			if settle.Complete() && settle.Dispatched > 0 && roundFullyDispatched {
				s.PaidRunsUsed += settle.Dispatched
				perRoundCharges[s.Round] += settle.Dispatched
				if s.PaidRunsUsed >= s.Budget.PaidRunBudget {
					s.Phase = PartyPhaseBudgetExhausted
				}
			}
		}

		plan := PlanPartyTick(s, settle, now, post)
		switch plan.Action {
		case PartyTickWait:
			// The only Wait the driver must unblock is "facilitator minted but has not posted its roster".
			if s.CurrentRoundMessageID == nil && s.Round > 0 {
				s.CurrentRoundMessageID = ptr(uuid.New())
				s.RoundVoices = roster
				settle = RoundSettlement{}
				continue
			}
			t.Fatalf("step %d: unexpected Wait (%s) round=%d settle=%+v", step, plan.Note, s.Round, settle)
		case PartyTickDispatchVoice:
			if plan.VoiceIndex != settle.Dispatched {
				t.Fatalf("step %d: cursor %d != Dispatched %d", step, plan.VoiceIndex, settle.Dispatched)
			}
			// Dispatch the turn, then let it settle (the next tick observes the settle).
			settle.Dispatched++
			settle.Settled++
			voiceDispatches++
		case PartyTickMintRound:
			// MintRound: bump the round, charge the facilitator mint, clear the round state.
			s.Round++
			s.PaidRunsUsed++
			s.CurrentRoundMessageID = nil
			s.RoundVoices = nil
			settle = RoundSettlement{}
			facilitatorMints++
		case PartyTickTakeaways:
			// The takeaways run is one final facilitator mint, then the session closes.
			s.Round++
			s.PaidRunsUsed++
			facilitatorMints++
			s.Phase = plan.Reason
		case PartyTickClose:
			s.Phase = plan.Reason
		}
		if !s.IsActive() {
			break
		}
	}

	if s.IsActive() {
		t.Fatal("debate never terminated within the step budget")
	}
	if s.Phase != PartyPhaseClosed {
		t.Fatalf("debate should end 'closed' (full 3 rounds + takeaways under budget), got %q", s.Phase)
	}
	// ADR-0031 §3.1 budget equality with ADR-0027's parallel fan-out:
	if voiceDispatches != rounds*voicesPerRound {
		t.Errorf("voice dispatches = %d, want %d (4 per round × 3 rounds)", voiceDispatches, rounds*voicesPerRound)
	}
	if facilitatorMints != rounds+1 {
		t.Errorf("facilitator mints = %d, want %d (one per round + takeaways, NOT one per turn)", facilitatorMints, rounds+1)
	}
	if s.PaidRunsUsed != 16 {
		t.Errorf("total paid runs = %d, want 16 (3 facilitators + 12 voices + 1 takeaways)", s.PaidRunsUsed)
	}
	// The anti-S-collapse guard: each round charged exactly its roster of voices once, never per-turn mints.
	for r := 1; r <= rounds; r++ {
		if perRoundCharges[r] != voicesPerRound {
			t.Errorf("round %d charged %d voices, want %d", r, perRoundCharges[r], voicesPerRound)
		}
	}
}
