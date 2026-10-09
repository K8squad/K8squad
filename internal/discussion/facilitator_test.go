package discussion

// Party-mode facilitator round-loop coverage (ISI-5588, WS-D, ADR-0027 §3). The selection policy, the
// advance decision, the settlement completeness predicates, and the directive bodies are all PURE (no
// I/O), so these ride the default unit lane with no database — exactly like the sibling WS-B party
// budget tests. The one DB method (RoundVoiceSettlement, the §3.3 settle-completeness join) is a
// migration/integration-lane concern.

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Voice selection — relevance + rotation + user-named + complementary (§3.2)
// ---------------------------------------------------------------------------

func voiceNames(sel VoiceSelection) string { return strings.Join(sel.Voices, ",") }

func TestSelectVoices_AlwaysIncludesUserNamed(t *testing.T) {
	cands := []VoiceCandidate{
		{Name: "alice", Eligible: true, UserNamed: true, Relevance: 1, LastRound: NeverSpoke},
		{Name: "bob", Eligible: true, UserNamed: false, Relevance: 99, LastRound: NeverSpoke},
		{Name: "carol", Eligible: true, UserNamed: false, Relevance: 50, LastRound: NeverSpoke},
	}
	// target=2: the user-named alice is included even though bob/carol are far more relevant.
	sel := SelectVoices(cands, 2, 1)
	if sel.Voices[0] != "alice" {
		t.Fatalf("user-named agent must be selected first, got %v", sel.Voices)
	}
	if sel.NamedIncluded != 1 {
		t.Fatalf("NamedIncluded = %d, want 1", sel.NamedIncluded)
	}
	if sel.Voices[1] != "bob" { // highest-relevance complementary fills the second slot
		t.Fatalf("complementary slot should take highest-relevance bob, got %v", sel.Voices)
	}
	if sel.Complementary != 1 {
		t.Fatalf("Complementary = %d, want 1", sel.Complementary)
	}
}

func TestSelectVoices_ComplementaryByRelevance(t *testing.T) {
	cands := []VoiceCandidate{
		{Name: "low", Eligible: true, Relevance: 1, LastRound: NeverSpoke},
		{Name: "high", Eligible: true, Relevance: 100, LastRound: NeverSpoke},
		{Name: "mid", Eligible: true, Relevance: 50, LastRound: NeverSpoke},
	}
	sel := SelectVoices(cands, 2, 1)
	if got := voiceNames(sel); got != "high,mid" {
		t.Fatalf("complementary order by relevance desc = %q, want high,mid", got)
	}
	if sel.Dropped != 1 {
		t.Fatalf("Dropped = %d, want 1 (low dropped by cap)", sel.Dropped)
	}
}

func TestSelectVoices_RotationPrefersFreshVoices(t *testing.T) {
	// Round 3. bob spoke in the immediately-preceding round (2) → he is deprioritised even though he is
	// the most relevant; a fresh voice fills the slot instead so the same two don't dominate.
	cands := []VoiceCandidate{
		{Name: "bob", Eligible: true, Relevance: 100, LastRound: 2},  // spoke last round
		{Name: "fresh", Eligible: true, Relevance: 10, LastRound: 1}, // spoke two rounds ago
		{Name: "newer", Eligible: true, Relevance: 5, LastRound: NeverSpoke},
	}
	sel := SelectVoices(cands, 2, 3)
	for _, v := range sel.Voices {
		if v == "bob" {
			t.Fatalf("rotation should defer prior-round speaker bob when fresh voices exist, got %v", sel.Voices)
		}
	}
	// Both fresh voices chosen; among them the never-spoken one is most overdue but relevance leads, so
	// fresh(10) ranks above newer(5).
	if got := voiceNames(sel); got != "fresh,newer" {
		t.Fatalf("fresh pool order = %q, want fresh,newer", got)
	}
}

func TestSelectVoices_FallsBackToRecentWhenRoundStillShort(t *testing.T) {
	// Round 3, target 2, but only one fresh voice exists → the prior-round speaker is reused to fill the
	// round rather than leaving it short.
	cands := []VoiceCandidate{
		{Name: "recent", Eligible: true, Relevance: 100, LastRound: 2},
		{Name: "fresh", Eligible: true, Relevance: 1, LastRound: NeverSpoke},
	}
	sel := SelectVoices(cands, 2, 3)
	if got := voiceNames(sel); got != "fresh,recent" {
		t.Fatalf("should fall back to prior-round speaker to fill the round, got %q want fresh,recent", got)
	}
	if sel.Dropped != 0 {
		t.Fatalf("Dropped = %d, want 0", sel.Dropped)
	}
}

func TestSelectVoices_SkipsIneligible(t *testing.T) {
	cands := []VoiceCandidate{
		{Name: "paused", Eligible: false, Relevance: 100, LastRound: NeverSpoke},
		{Name: "self", Eligible: false, UserNamed: true, Relevance: 100, LastRound: NeverSpoke},
		{Name: "ok", Eligible: true, Relevance: 1, LastRound: NeverSpoke},
	}
	sel := SelectVoices(cands, 3, 1)
	if got := voiceNames(sel); got != "ok" {
		t.Fatalf("ineligible candidates (incl. user-named-but-ineligible) must be skipped, got %q", got)
	}
	if sel.Dropped != 0 {
		t.Fatalf("Dropped = %d, want 0 (ineligibles are not droppable candidates)", sel.Dropped)
	}
}

func TestSelectVoices_CapBitesNamedWhenOverBudget(t *testing.T) {
	// 3 user-named but the budget allows only 2 — the hard cap beats always-include; the least-relevant
	// named voice is dropped, and it is counted non-silently.
	cands := []VoiceCandidate{
		{Name: "a", Eligible: true, UserNamed: true, Relevance: 10, LastRound: NeverSpoke},
		{Name: "b", Eligible: true, UserNamed: true, Relevance: 30, LastRound: NeverSpoke},
		{Name: "c", Eligible: true, UserNamed: true, Relevance: 20, LastRound: NeverSpoke},
	}
	sel := SelectVoices(cands, 2, 1)
	if got := voiceNames(sel); got != "b,c" {
		t.Fatalf("over-budget named selection = %q, want b,c (by relevance desc)", got)
	}
	if sel.NamedIncluded != 2 || sel.Complementary != 0 {
		t.Fatalf("named=%d comp=%d, want 2/0", sel.NamedIncluded, sel.Complementary)
	}
	if sel.Dropped != 1 {
		t.Fatalf("Dropped = %d, want 1 (hard budget cap drops a named voice non-silently)", sel.Dropped)
	}
}

func TestSelectVoices_TargetZeroSelectsNobody(t *testing.T) {
	cands := []VoiceCandidate{{Name: "a", Eligible: true, Relevance: 1, LastRound: NeverSpoke}}
	sel := SelectVoices(cands, 0, 1)
	if len(sel.Voices) != 0 || sel.Dropped != 1 {
		t.Fatalf("target<=0 should select nobody and drop all eligibles, got %+v", sel)
	}
}

func TestSelectVoices_Deterministic(t *testing.T) {
	// Equal relevance + equal staleness → name tie-break, stable across runs.
	cands := []VoiceCandidate{
		{Name: "charlie", Eligible: true, Relevance: 5, LastRound: NeverSpoke},
		{Name: "alpha", Eligible: true, Relevance: 5, LastRound: NeverSpoke},
		{Name: "bravo", Eligible: true, Relevance: 5, LastRound: NeverSpoke},
	}
	for i := 0; i < 5; i++ {
		if got := voiceNames(SelectVoices(cands, 3, 1)); got != "alpha,bravo,charlie" {
			t.Fatalf("iter %d: tie-break not deterministic, got %q", i, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Round settlement completeness (§3.3)
// ---------------------------------------------------------------------------

func TestRoundSettlement_Complete(t *testing.T) {
	cases := []struct {
		rs   RoundSettlement
		want bool
	}{
		{RoundSettlement{Dispatched: 0}, true},                           // nobody dispatched → trivially done
		{RoundSettlement{Dispatched: 3, Settled: 3}, true},               // all settled
		{RoundSettlement{Dispatched: 3, Settled: 1, TimedOut: 2}, true},  // settle + timeout = dispatched
		{RoundSettlement{Dispatched: 3, Settled: 2}, false},              // one still running
		{RoundSettlement{Dispatched: 3, Settled: 1, TimedOut: 1}, false}, // one still running
	}
	for i, c := range cases {
		if got := c.rs.Complete(); got != c.want {
			t.Errorf("case %d: Complete(%+v) = %v, want %v", i, c.rs, got, c.want)
		}
	}
}

func TestRoundSettlement_AllSilent(t *testing.T) {
	if !(RoundSettlement{Dispatched: 2, TimedOut: 2}).AllSilent() {
		t.Error("a round where every voice timed out with no marker is all-silent")
	}
	if (RoundSettlement{Dispatched: 2, Settled: 1, TimedOut: 1}).AllSilent() {
		t.Error("a round with any settled output is not all-silent")
	}
	if (RoundSettlement{Dispatched: 0}).AllSilent() {
		t.Error("an empty round is not all-silent")
	}
}

// ---------------------------------------------------------------------------
// Advance decision — the §3.2 loop over the §5.1 hard budget
// ---------------------------------------------------------------------------

// sess builds an active session with the given round + budget + paid-runs-used for decision tests.
func sess(round, maxRounds, maxVoices, paidBudget, paidUsed int, phase string) PartySession {
	return PartySession{
		Round:        round,
		Budget:       PartyBudget{MaxRounds: maxRounds, MaxVoicesPerRound: maxVoices, PaidRunBudget: paidBudget},
		PaidRunsUsed: paidUsed,
		Phase:        phase,
	}
}

func TestDecideAdvance_WaitsUntilRoundComplete(t *testing.T) {
	d := DecideAdvance(sess(1, 3, 3, 16, 4, PartyPhaseActive), false /*complete*/, false)
	if d.Action != AdvanceWait {
		t.Fatalf("incomplete round must wait, got %+v", d)
	}
}

func TestDecideAdvance_NextRoundWhenBudgetAndRoundsRemain(t *testing.T) {
	// round 1 of 3 done, plenty of paid-run headroom, not converged → run round 2.
	d := DecideAdvance(sess(1, 3, 3, 16, 4, PartyPhaseActive), true, false)
	if d.Action != AdvanceNextRound || d.Reason != "" {
		t.Fatalf("got %+v, want AdvanceNextRound/''", d)
	}
}

func TestDecideAdvance_TakeawaysAtMaxRounds(t *testing.T) {
	// round == max_rounds: no more debate rounds, but headroom for the takeaways mint → takeaways/closed.
	d := DecideAdvance(sess(3, 3, 3, 16, 10, PartyPhaseActive), true, false)
	if d.Action != AdvanceTakeaways || d.Reason != PartyPhaseClosed {
		t.Fatalf("got %+v, want AdvanceTakeaways/closed", d)
	}
}

func TestDecideAdvance_TakeawaysOnConvergence(t *testing.T) {
	// rounds + budget remain, but the facilitator declared convergence → stop early with a takeaways run.
	d := DecideAdvance(sess(1, 3, 3, 16, 4, PartyPhaseActive), true, true /*converged*/)
	if d.Action != AdvanceTakeaways || d.Reason != PartyPhaseConverged {
		t.Fatalf("got %+v, want AdvanceTakeaways/converged", d)
	}
}

func TestDecideAdvance_CloseWhenNoHeadroomForTakeaways(t *testing.T) {
	// paid budget fully used → not even a takeaways mint is affordable → hard budget_exhausted close.
	d := DecideAdvance(sess(2, 3, 3, 16, 16, PartyPhaseActive), true, false)
	if d.Action != AdvanceClose || d.Reason != PartyPhaseBudgetExhausted {
		t.Fatalf("got %+v, want AdvanceClose/budget_exhausted", d)
	}
}

func TestDecideAdvance_ConvergenceStillBudgetClosesWhenBroke(t *testing.T) {
	// converged AND broke → the hard budget stop wins the reason (no affordable takeaways).
	d := DecideAdvance(sess(1, 3, 3, 16, 16, PartyPhaseActive), true, true)
	if d.Action != AdvanceClose || d.Reason != PartyPhaseBudgetExhausted {
		t.Fatalf("got %+v, want AdvanceClose/budget_exhausted", d)
	}
}

func TestDecideAdvance_TerminalSessionClosesOut(t *testing.T) {
	// RecordPaidRuns already flipped the session to budget_exhausted before the decision → close out.
	d := DecideAdvance(sess(2, 3, 3, 16, 16, PartyPhaseBudgetExhausted), true, false)
	if d.Action != AdvanceClose || d.Reason != PartyPhaseBudgetExhausted {
		t.Fatalf("got %+v, want AdvanceClose/budget_exhausted", d)
	}
}

// ---------------------------------------------------------------------------
// Directive bodies (§3.2) — content the re-minted facilitator run pulls
// ---------------------------------------------------------------------------

func fround() FacilitatorRound {
	return FacilitatorRound{
		ProjectID:      "acme/web",
		ThreadID:       uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		TopicMessageID: uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Round:          2,
		MaxRounds:      3,
		VoiceCap:       3,
		UserNamed:      []string{"alice", "bob"},
		Topic:          "should we adopt the new substrate?",
	}
}

func TestFacilitatorRoundDirective_Content(t *testing.T) {
	body := FacilitatorRoundDirective(fround())
	for _, want := range []string{
		"round 2 of 3",
		"should we adopt the new substrate?",
		"at most 3 relevant agents",
		"hard-caps the round at 3 voices",
		"Rotate",
		"@alice, @bob",
		"work_item_create + work_item_assign",
		"Do NOT paraphrase",
		"PASS in one sentence",
		"[party] facilitator round=2/3 voiceCap=3",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("round directive missing %q\n---\n%s", want, body)
		}
	}
}

func TestFacilitatorRoundDirective_NoNamedVoices(t *testing.T) {
	d := fround()
	d.UserNamed = nil
	body := FacilitatorRoundDirective(d)
	if strings.Contains(body, "ALWAYS include") {
		t.Errorf("no user-named agents → must not render the always-include line\n%s", body)
	}
	if !strings.Contains(body, "Pick on relevance") {
		t.Errorf("no user-named agents → must render the relevance-only line\n%s", body)
	}
}

func TestTakeawaysDirective_ReasonFraming(t *testing.T) {
	cases := map[string]string{
		PartyPhaseConverged:       "converged",
		PartyPhaseBudgetExhausted: "cut for cost",
		PartyPhaseClosed:          "round limit",
	}
	for reason, want := range cases {
		body := TakeawaysDirective(fround(), reason)
		if !strings.Contains(body, want) {
			t.Errorf("takeaways(%s) missing framing %q\n%s", reason, want, body)
		}
		for _, must := range []string{"TAKEAWAYS", "DISAGREEMENT", "NOT invent a consensus", "do NOT dispatch any more voices"} {
			if !strings.Contains(body, must) {
				t.Errorf("takeaways(%s) missing %q", reason, must)
			}
		}
		if !strings.Contains(body, "reason="+reason) {
			t.Errorf("takeaways(%s) trailer missing reason tag", reason)
		}
	}
}
