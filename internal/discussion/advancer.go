// ISI-5615 (ISI-5569 WS-D.1, ruling ADR-0027 §3.2/§3.3) — the party-mode advancer's pure decision.
//
// WS-D (facilitator.go) shipped the pure pieces an advancer runs ON — voice SELECTION, the advance
// DECISION (DecideAdvance), round-settlement COMPLETENESS (RoundVoiceSettlement), and the facilitator
// DIRECTIVE bodies. This file is the pure state machine that SEQUENCES them into one per-session tick, and
// party.go adds the durable CAS mutations (MintRound / SetRoundFacilitatorMessage / RecordPaidRuns /
// CloseSession) it drives. The live operator runnable (ISI-5615 WS-D.1 operator wiring) is then a thin I/O
// shell: each tick it reads the active sessions + their round settlement, calls PlanPartyTick, and executes
// the returned plan through the coord mint seam + the party Store — holding NO in-memory round state, so it
// resumes a debate purely from the durable party_session row + the ADR-0020 settle markers after a restart
// (§3.1 — the whole reason the long-lived facilitator run was rejected).
//
// Keeping the decision pure (no DB, no mint seam) makes the loop's every branch exhaustively unit-testable
// without Postgres or a coordinator — exactly as WS-B's budget policy and WS-D's DecideAdvance are. The
// operator shell carries only the irreducible I/O: ActivePartySessions, RoundVoiceSettlement, and the
// mint/close execution.
package discussion

import "time"

const (
	// DefaultPartyAdvancerTick is how often the level-triggered advancer re-derives the loop from durable
	// state (ADR-0027 §3.2 — the ADR-0020 reaper cadence; matches rundrive Intake/Heartbeat at 10s). It is a
	// floor on advance latency, not a correctness knob: settle markers and the party_session row are durable,
	// so a missed or delayed tick only slows the debate, never corrupts it.
	DefaultPartyAdvancerTick = 10 * time.Second

	// DefaultPartyVoiceSettleTimeout bounds the §3.3 residual: a voice-run whose a2a follow died on a
	// mid-round restart never writes its settle marker. After this long the advancer counts that voice
	// settled-with-no-output so one stuck voice can never wedge the debate (2× expected run wall-clock).
	DefaultPartyVoiceSettleTimeout = 10 * time.Minute

	// DefaultPartyFacilitatorPostTimeout bounds the symmetric gap this layer adds: a facilitator run that is
	// minted but dies before posting its round dispatch (so current_round_message_id never gets stamped).
	// After this long with no facilitator message the advancer closes the debate rather than wait forever.
	DefaultPartyFacilitatorPostTimeout = 10 * time.Minute
)

// PartyTickAction is what the advancer should do for one session this tick.
type PartyTickAction int

const (
	// PartyTickWait — nothing to do: the round's facilitator has not posted yet, or its voices have not all
	// settled. The next tick re-evaluates from durable state. The zero value, so a mis-built plan is inert.
	PartyTickWait PartyTickAction = iota
	// PartyTickMintRound — mint the facilitator run for the next round (MintRound CAS on FromRound, then
	// dispatch the coordinator-as-facilitator with FacilitatorRoundDirective). Used both to kick round 1 of a
	// fresh session (FromRound 0) and to advance after a productive round (FromRound = the settled round).
	PartyTickMintRound
	// PartyTickTakeaways — the debate is ending normally with paid-run headroom for one final run: mint the
	// takeaways facilitator run (TakeawaysDirective) then CloseSession with Reason.
	PartyTickTakeaways
	// PartyTickClose — end the session with Reason and NO further mint (budget exhausted, or the facilitator
	// never posted its dispatch within the post-timeout).
	PartyTickClose
)

// PartyTickPlan is the advancer's ruling for one session this tick: the action, the round it concerns, the
// CAS guard for a mint, the terminal phase for a close/takeaways, the settlement it observed, and a short
// non-silent note (ADR-0027 §5.1 — truncation/early-stop reasons are logged, never swallowed).
type PartyTickPlan struct {
	Action     PartyTickAction
	FromRound  int             // the round the MintRound CAS guards on (0 kicks round 1)
	Round      int             // the session's current round this plan concerns (for logging)
	Reason     string          // terminal phase for Takeaways/Close (closed | converged | budget_exhausted)
	Settlement RoundSettlement // the round-N voice settlement observed (zero when no facilitator message yet)
	Note       string          // non-silent decision note for the operator log
}

// PlanPartyTick is the pure per-session decision the live advancer executes (ADR-0027 §3.2/§3.3). It takes
// the durable session, the round's voice settlement (zero when the round has no facilitator message yet),
// the current time, and the facilitator-post timeout, and returns exactly one plan. All the state it needs
// is on the row + the settlement — no in-memory history — so it is restart-safe and fully testable.
//
// The sequence, top to bottom:
//  1. A non-active session is inert (defensive; the sweep only returns active rows, but a tick can race a
//     close): wait.
//  2. round == 0 — a fresh opt-in session the advancer has not started yet: mint round 1.
//  3. current_round_message_id == nil — a round's facilitator was minted but has not posted its dispatch
//     yet: wait, unless it has been too long (the facilitator run died before posting) → close.
//  4. the facilitator posted (current_round_message_id set) — gate on the round's voice settlement: not
//     complete → wait; complete → DecideAdvance (next round | takeaways | close), reusing WS-D's budget gate.
func PlanPartyTick(s PartySession, settle RoundSettlement, now time.Time, facilitatorPostTimeout time.Duration) PartyTickPlan {
	if !s.IsActive() {
		return PartyTickPlan{Action: PartyTickWait, Round: s.Round, Note: "session not active"}
	}

	// (2) Fresh session — kick round 1. MintRound CAS guards on round=0.
	if s.Round == 0 {
		return PartyTickPlan{Action: PartyTickMintRound, FromRound: 0, Round: 0, Note: "start round 1"}
	}

	// (3) A round's facilitator is minted but has not posted its @-mention dispatch yet.
	if s.CurrentRoundMessageID == nil {
		if facilitatorPostTimeout > 0 && s.RoundStartedAt != nil &&
			now.Sub(*s.RoundStartedAt) > facilitatorPostTimeout {
			return PartyTickPlan{
				Action: PartyTickClose, Round: s.Round, Reason: PartyPhaseClosed,
				Note: "facilitator did not post its dispatch within the post-timeout",
			}
		}
		return PartyTickPlan{Action: PartyTickWait, Round: s.Round, Note: "awaiting facilitator dispatch post"}
	}

	// (4) The facilitator posted — advance on the round's voice-run settlement (never reply-landing, §3.3).
	if !settle.Complete() {
		return PartyTickPlan{Action: PartyTickWait, Round: s.Round, Settlement: settle, Note: "awaiting voice settlement"}
	}
	dec := DecideAdvance(s, true, settle.AllSilent())
	switch dec.Action {
	case AdvanceNextRound:
		return PartyTickPlan{Action: PartyTickMintRound, FromRound: s.Round, Round: s.Round, Settlement: settle, Note: "round productive — next round"}
	case AdvanceTakeaways:
		return PartyTickPlan{Action: PartyTickTakeaways, Round: s.Round, Reason: dec.Reason, Settlement: settle, Note: "ending — takeaways"}
	case AdvanceClose:
		return PartyTickPlan{Action: PartyTickClose, Round: s.Round, Reason: dec.Reason, Settlement: settle, Note: "ending — no further mint"}
	default: // AdvanceWait — DecideAdvance saw an incomplete round (should not reach here after Complete()).
		return PartyTickPlan{Action: PartyTickWait, Round: s.Round, Settlement: settle, Note: "decide: wait"}
	}
}
