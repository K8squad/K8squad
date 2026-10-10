// ISI-5640 (ISI-5624 C4, ruling ADR-0031 §3.4) — unit coverage for the settle-timeout tiering that keeps
// one wedged turn from stalling a whole SERIALIZED debate. Under sequencing only one voice is in flight at
// a time and every later turn waits behind it, so a dead turn must time out sooner than the parallel
// round-sweep backstop — but the backstop stays the hard ceiling. These are pure tests (no DB) over
// effectivePartySettleTimeout, the function advanceSession uses to choose the per-session settlement guard.
package discussion

import (
	"testing"
	"time"
)

func TestEffectivePartySettleTimeout(t *testing.T) {
	const (
		backstop = DefaultPartyVoiceSettleTimeout          // 10m
		seq      = DefaultPartySequentialTurnSettleTimeout // 5m
	)

	cases := []struct {
		name      string
		hasRoster bool
		backstop  time.Duration
		seq       time.Duration
		want      time.Duration
	}{
		{
			// Sequenced session (roster present) → the tightened per-turn guard wins so a dead turn
			// advances the cursor at 5m instead of holding every later turn for the full 10m.
			name: "sequenced uses the tightened guard", hasRoster: true,
			backstop: backstop, seq: seq, want: seq,
		},
		{
			// No roster (pre-0035 parallel round-sweep): voices run concurrently, so the full backstop is
			// the right bound — the tightening never applies.
			name: "no roster keeps the backstop", hasRoster: false,
			backstop: backstop, seq: seq, want: backstop,
		},
		{
			// The backstop is the HARD ceiling: a backstop lowered below the sequential guard wins, so you
			// can never widen a turn's guard past the backstop by raising seq.
			name: "backstop below seq is the ceiling", hasRoster: true,
			backstop: 3 * time.Minute, seq: seq, want: 3 * time.Minute,
		},
		{
			// A non-positive sequential guard falls back to the backstop (defensive — the runner defaults a
			// zero field before calling, but the function must be total).
			name: "zero seq falls back to backstop", hasRoster: true,
			backstop: backstop, seq: 0, want: backstop,
		},
		{
			// seq == backstop is not "<", so no tightening — identical to the parallel bound.
			name: "seq equal to backstop does not tighten", hasRoster: true,
			backstop: backstop, seq: backstop, want: backstop,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectivePartySettleTimeout(tc.hasRoster, tc.backstop, tc.seq); got != tc.want {
				t.Fatalf("effectivePartySettleTimeout(%v, %v, %v) = %v, want %v",
					tc.hasRoster, tc.backstop, tc.seq, got, tc.want)
			}
		})
	}
}

// TestSequentialTurnTimeoutDefaultIsTighterThanBackstop pins the ADR-0031 §3.4 relationship the whole C4
// change rests on: the serialized per-turn guard is strictly tighter than the round-sweep hard backstop,
// so a wedged turn advances the cursor before it stalls the entire remaining debate.
func TestSequentialTurnTimeoutDefaultIsTighterThanBackstop(t *testing.T) {
	if DefaultPartySequentialTurnSettleTimeout >= DefaultPartyVoiceSettleTimeout {
		t.Fatalf("sequential turn timeout %v must be < backstop %v (ADR-0031 §3.4)",
			DefaultPartySequentialTurnSettleTimeout, DefaultPartyVoiceSettleTimeout)
	}
}

// TestSequentialStuckTurnAdvancesCursorNotWedge ties the tightened guard back to the advancer's decision:
// once a wedged turn is counted TIMED OUT (which the tighter guard reaches sooner), PlanPartyTick treats it
// as terminal and dispatches the NEXT voice rather than waiting forever — so one dead turn cannot wedge the
// serialized session. (The guard only changes WHEN TimedOut flips; this asserts the decision that flip
// drives, with the shorter timeout already elapsed.)
func TestSequentialStuckTurnAdvancesCursorNotWedge(t *testing.T) {
	s := seqSession(1, 3, 4, 20, 1, "winston", "sally", "amelia")
	now, post := time.Now(), DefaultPartyFacilitatorPostTimeout

	// Voice 0 settled, voice 1 dispatched then wedged (no marker) — under the tightened guard it is counted
	// TimedOut at 5m instead of 10m. With TimedOut=1 the cursor advances to voice 2: the debate keeps moving.
	got := PlanPartyTick(s, RoundSettlement{Dispatched: 2, Settled: 1, TimedOut: 1}, now, post)
	if got.Action != PartyTickDispatchVoice || got.VoiceIndex != 2 {
		t.Fatalf("wedged turn should advance the cursor: want DispatchVoice idx=2, got action=%v idx=%d (%s)",
			got.Action, got.VoiceIndex, got.Note)
	}

	// Before the (now shorter) timeout elapses the turn is still in flight → WAIT (one-in-flight preserved).
	if got := PlanPartyTick(s, RoundSettlement{Dispatched: 2, Settled: 1, TimedOut: 0}, now, post); got.Action != PartyTickWait {
		t.Fatalf("turn still in flight pre-timeout: want Wait, got %v (%s)", got.Action, got.Note)
	}
}
