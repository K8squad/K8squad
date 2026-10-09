// ISI-5588 (ISI-5569 WS-D, ruling ADR-0027 §3) — the party-mode facilitator round loop.
//
// This is the decision + settle-completeness layer the event-driven advancer runs on. The advancer is
// NOT a long-lived blocking run (ADR-0027 §3.1 rejected that: it fights the single-shot grain and
// re-introduces the ADR-0020 restart leak). Instead each round is one short, terminal facilitator run
// plus the durable discussion.party_session row (WS-B), and a level-triggered advancer re-mints the
// facilitator each round when the prior round's voice-runs SETTLE (ADR-0027 §3.2/§3.3):
//
//	round N: mint facilitator run (Coordinator, Orchestrate=true) → it selects 2–4 voices + dispatches
//	         → voices run → settle markers (ADR-0020 coord.a2a_dispatch.settled_at)
//	         → advancer observes round N settled → AdvanceRound + RecordPaidRuns + DecideAdvance:
//	              rounds remain AND productive → mint round N+1
//	              else                          → mint ONE takeaways run, then CloseSession
//
// Everything here is PURE except RoundVoiceSettlement (the one durable read of §3.3). The pure core —
// voice SELECTION (relevance + rotation, always-include-user-named + complementary), the advance
// DECISION (bounded rounds / budget / convergence → next-round | takeaways | close), and the
// facilitator DIRECTIVE bodies — is exhaustively unit-testable with no DB, exactly like WS-B's budget
// policy. The budget is a SERVER gate: DecideAdvance reuses PartySession.CanStartRound (WS-B) so the
// advancer never mints past the ceiling, and SelectVoices clamps to the budget-allowed count so a
// facilitator can never dispatch more voices than the round permits (ADR-0027 §3.3 — the budget is
// enforced at the mint seam, never trusted to the agent).
package discussion

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ============================================================================
// Voice selection — pure (ADR-0027 §3.2: relevance + rotation + user-named + complementary)
// ============================================================================

// VoiceCandidate is one roster agent the facilitator may select as a voice for a round. The relevance
// and rotation signals are supplied by the caller (the facilitator agent's declared selection and/or
// the thread participation history); SelectVoices applies the ruled policy to them. It is deliberately
// blind to I/O so the whole selection policy is unit-testable.
type VoiceCandidate struct {
	// Name is the canonical roster name of the agent (the dispatch key).
	Name string
	// Eligible is false for an agent that must never be dispatched this round — the facilitator itself
	// (an agent never debates with itself), and any opted-out agent (paused/blocked, ADR-0027 §6). An
	// ineligible candidate is skipped entirely; the facilitator simply has fewer voices.
	Eligible bool
	// UserNamed is true when the human who raised the topic named this agent. Ruling §3.2: user-named
	// voices are ALWAYS included (subject only to the hard budget cap), even if they spoke last round.
	UserNamed bool
	// Relevance is the agent's relevance-to-topic score (higher = more relevant). It orders the
	// complementary pool. Equal scores fall through to the rotation + name tie-breaks for determinism.
	Relevance int
	// LastRound is the most recent round this agent voiced in, or NeverSpoke if it has not spoken in
	// this session. It drives rotation: agents who spoke longest ago (or never) are preferred so the
	// same two voices do not dominate the debate (§3.2).
	LastRound int
}

// NeverSpoke is the LastRound sentinel for a candidate that has not voiced in the session yet. It sorts
// as the most-overdue (highest rotation priority), ahead of anyone who has already spoken.
const NeverSpoke = -1

// VoiceSelection is the outcome of a round's voice selection: the chosen voices (in dispatch order) and
// a non-silent trace of how the cap bit, so the advancer logs truncation rather than swallowing it
// (ADR-0027 §5.1 — dropped/capped counts are never silently dropped).
type VoiceSelection struct {
	// Voices is the selected agent names in a deterministic dispatch order (user-named first, then
	// complementary by relevance+rotation). len(Voices) <= target.
	Voices []string
	// NamedIncluded is how many of Voices were user-named (always-include, §3.2).
	NamedIncluded int
	// Complementary is how many of Voices were relevance/rotation-selected fill (the "1–2 complementary
	// voices" of §3.2).
	Complementary int
	// Dropped is how many ELIGIBLE candidates were not selected because target bound the round. It is
	// the non-silent cap signal (§5.1). Named candidates dropped by the cap are included here too — the
	// budget is hard and beats even the always-include rule.
	Dropped int
}

// SelectVoices applies the ADR-0027 §3.2 facilitator selection policy to the round's candidates, bound
// to `target` voices (the caller passes the budget-allowed count from PartySession.VoicesAllowedThisRound
// so the selection can never exceed the per-round / paid-run budget — the server cap, not the agent's
// word). `currentRound` is the round being selected FOR; it is used only to compute rotation staleness.
//
// Policy, in order:
//  1. Drop ineligible candidates (facilitator-self + opted-out).
//  2. Always include user-named voices first (§3.2), ranked relevance desc → most-overdue → name, so a
//     cap that bites named voices drops the least-relevant / most-recently-heard ones deterministically.
//  3. Fill the remaining slots with COMPLEMENTARY voices, applying ROTATION: prefer candidates who did
//     NOT speak in the immediately-preceding round, so the same two voices do not dominate; only fall
//     back to prior-round speakers when there is no fresher eligible voice left to fill the round.
//  4. Within each pool, order by relevance desc → most-overdue (smallest LastRound, NeverSpoke first) →
//     name asc, for a stable, test-pinnable result.
//
// target <= 0 selects nobody (a degenerate round the advancer closes).
func SelectVoices(candidates []VoiceCandidate, target, currentRound int) VoiceSelection {
	eligible := make([]VoiceCandidate, 0, len(candidates))
	for _, c := range candidates {
		if c.Eligible && c.Name != "" {
			eligible = append(eligible, c)
		}
	}
	if target <= 0 {
		return VoiceSelection{Dropped: len(eligible)}
	}

	// less orders a pool by the ruled relevance → rotation → name tie-break (step 4).
	less := func(a, b VoiceCandidate) bool {
		if a.Relevance != b.Relevance {
			return a.Relevance > b.Relevance // higher relevance first
		}
		sa, sb := staleness(a.LastRound, currentRound), staleness(b.LastRound, currentRound)
		if sa != sb {
			return sa > sb // spoke longer ago (more overdue) first
		}
		return a.Name < b.Name // deterministic final tie-break
	}

	var named, fresh, recent []VoiceCandidate
	priorRound := currentRound - 1
	for _, c := range eligible {
		switch {
		case c.UserNamed:
			named = append(named, c)
		case c.LastRound == priorRound && priorRound >= 1:
			// Spoke in the immediately-preceding round → deprioritised for rotation (§3.2).
			recent = append(recent, c)
		default:
			fresh = append(fresh, c)
		}
	}
	sort.SliceStable(named, func(i, j int) bool { return less(named[i], named[j]) })
	sort.SliceStable(fresh, func(i, j int) bool { return less(fresh[i], fresh[j]) })
	sort.SliceStable(recent, func(i, j int) bool { return less(recent[i], recent[j]) })

	sel := VoiceSelection{}
	take := func(pool []VoiceCandidate, isNamed bool) {
		for _, c := range pool {
			if len(sel.Voices) >= target {
				return
			}
			sel.Voices = append(sel.Voices, c.Name)
			if isNamed {
				sel.NamedIncluded++
			} else {
				sel.Complementary++
			}
		}
	}
	take(named, true)   // always-include user-named first
	take(fresh, false)  // rotation: fresh complementary voices
	take(recent, false) // only reuse prior-round speakers if the round is still short
	sel.Dropped = len(eligible) - len(sel.Voices)
	return sel
}

// staleness is the rotation priority of a candidate: how many rounds ago it last spoke. A never-spoken
// candidate (NeverSpoke) is the most overdue of all. Current/future LastRound values (should not occur)
// clamp to 0 so they are the least overdue.
func staleness(lastRound, currentRound int) int {
	if lastRound == NeverSpoke {
		return currentRound + 1 // strictly more overdue than anyone who has spoken
	}
	s := currentRound - lastRound
	if s < 0 {
		return 0
	}
	return s
}

// ============================================================================
// Round settlement completeness — pure type + the one durable read (ADR-0027 §3.3)
// ============================================================================

// RoundSettlement is how a round's dispatched voice-runs have progressed toward terminal. The advancer
// advances ON RUN-SETTLE, not on reply-message-landing (ADR-0027 §3.3): a voice may legitimately pass
// (terminate without posting) or fail, so counting reply messages would wedge the session forever. Every
// dispatched voice-run reaches terminal exactly once (single-shot, ADR-0024c §4), so run-settle is the
// complete, restart-safe signal. TimedOut counts voices whose follow never wrote a settle marker but
// whose dispatch is older than the settle_timeout — the §3.3 stuck-voice guard, counted-settled so one
// stuck voice can never wedge the debate.
type RoundSettlement struct {
	Dispatched int // voice-runs the round dispatched
	Settled    int // voice-runs with a durable terminal settle marker (coord.a2a_dispatch.settled_at)
	TimedOut   int // unsettled voice-runs older than settle_timeout, counted-settled (§3.3)
}

// Complete reports whether the round is finished — every dispatched voice settled or timed out. A round
// that dispatched nobody is trivially complete (the advancer closes it). This is the gate the advancer
// checks before advancing the round counter.
func (rs RoundSettlement) Complete() bool {
	if rs.Dispatched <= 0 {
		return true
	}
	return rs.Settled+rs.TimedOut >= rs.Dispatched
}

// AllSilent reports whether a COMPLETE round produced no settled output at all — every dispatched voice
// timed out with no marker. The advancer may feed this to DecideAdvance as a convergence signal: a round
// where nobody actually voiced is a dead debate, nothing is gained by minting another (§3.2 early-stop).
func (rs RoundSettlement) AllSilent() bool {
	return rs.Dispatched > 0 && rs.Settled == 0 && rs.TimedOut == rs.Dispatched
}

// RoundVoiceSettlement reads how far a round's dispatched voice-runs have settled (ADR-0027 §3.3). A
// round's voices are the discussion.mention_dispatch rows minted under the round's facilitator message
// (§4.3 — the ledger IS the per-round dispatched-voice record); a voice counts settled when its minted
// work item has a settled a2a follow (coord.a2a_dispatch.settled_at, ADR-0020, MAX over retry laps) OR
// its dispatch row is older than settleTimeout (the §3.3 stuck-voice guard). This is the single durable,
// restart-safe read the level-triggered advancer polls — it holds no in-memory round state (§3.1). It is
// a cross-schema READ only (discussion ledger → coord settle marker, the ADR-0020 S3 join direction); no
// custody is read or moved, so the ADR-0019 fence is untouched (§4.2).
func (s *Store) RoundVoiceSettlement(ctx context.Context, facilitatorMessageID uuid.UUID, settleTimeout time.Duration) (RoundSettlement, error) {
	secs := settleTimeout.Seconds()
	if secs < 0 {
		secs = 0
	}
	var rs RoundSettlement
	err := s.db.QueryRowContext(ctx, `
		SELECT
		    count(*)                                                                   AS dispatched,
		    count(*) FILTER (WHERE ad.settled_at IS NOT NULL)                          AS settled,
		    count(*) FILTER (WHERE ad.settled_at IS NULL
		                       AND md.created_at < now() - make_interval(secs => $2))  AS timed_out
		FROM discussion.mention_dispatch md
		LEFT JOIN LATERAL (
		    SELECT max(settled_at) AS settled_at
		    FROM coord.a2a_dispatch
		    WHERE work_item_id = md.work_item_id
		) ad ON md.work_item_id IS NOT NULL
		WHERE md.message_id = $1`,
		facilitatorMessageID, secs,
	).Scan(&rs.Dispatched, &rs.Settled, &rs.TimedOut)
	if err != nil {
		return RoundSettlement{}, fmt.Errorf("read round voice settlement: %w", err)
	}
	return rs, nil
}

// ============================================================================
// Advance decision — pure (ADR-0027 §3.2 loop + §5.1 hard budget)
// ============================================================================

// AdvanceAction is what the advancer does when a round settles.
type AdvanceAction int

const (
	// AdvanceWait — the round's voice-runs have not all settled yet; do nothing (the next settle event
	// re-triggers the advancer). The default zero value, so a mis-constructed decision is inert.
	AdvanceWait AdvanceAction = iota
	// AdvanceNextRound — rounds remain, budget allows, and the debate is still productive: mint the next
	// facilitator run (the Coordinator selects + dispatches round N+1's voices).
	AdvanceNextRound
	// AdvanceTakeaways — the debate is ending normally (max rounds reached or converged) and there is
	// still paid-run headroom for ONE final facilitator run: mint the end-of-session takeaways run, then
	// close the session with Reason.
	AdvanceTakeaways
	// AdvanceClose — the session ends with NO further mint (the paid-run ceiling is reached — not even a
	// takeaways run is affordable). Close the session with Reason.
	AdvanceClose
)

// AdvanceDecision is the advancer's ruling for a settled round: the action plus, for the terminal
// actions, the session phase to close with (closed | converged | budget_exhausted).
type AdvanceDecision struct {
	Action AdvanceAction
	Reason string // terminal phase for AdvanceTakeaways/AdvanceClose; "" for Wait/NextRound
}

// DecideAdvance is the pure loop decision (ADR-0027 §3.2/§5.1). `s` is the session AS IT WILL BE after
// the just-settled round has been recorded — i.e. RecordPaidRuns (which may itself flip the phase to
// budget_exhausted) and AdvanceRound (the round counter bumped for the completed round) have been
// applied before calling this. `complete` is RoundSettlement.Complete() for that round; `converged` is
// the facilitator's early-stop signal (declared convergence or an all-silent round, §3.2).
//
// It reuses PartySession.CanStartRound (WS-B) as the single budget/round gate so the advancer can never
// mint past the ceiling, and layers the takeaways-vs-hard-stop distinction on top: a normal end still
// gets one final takeaways run if the ceiling allows, but a budget-exhausted session stops cold.
func DecideAdvance(s PartySession, complete, converged bool) AdvanceDecision {
	if !complete {
		return AdvanceDecision{Action: AdvanceWait}
	}
	// RecordPaidRuns may already have flipped the session terminal (hard ceiling hit mid-round). A
	// non-active session is over — close it out with whatever terminal phase it carries, no mint.
	if !s.IsActive() {
		return AdvanceDecision{Action: AdvanceClose, Reason: s.Phase}
	}

	canRound, closeReason := s.CanStartRound()
	if canRound && !converged {
		return AdvanceDecision{Action: AdvanceNextRound}
	}

	// Stopping. Pick the terminal reason: convergence wins; otherwise the budget gate's reason (closed
	// when rounds are exhausted, budget_exhausted when paid runs are) — defaulting to a clean close.
	reason := PartyPhaseClosed
	switch {
	case converged:
		reason = PartyPhaseConverged
	case closeReason != "":
		reason = closeReason
	}

	// A takeaways run is itself one paid facilitator mint. If even that is unaffordable, stop cold with
	// a hard budget_exhausted close (ADR-0027 §5.1 — the ceiling is a hard stop, not advisory).
	if s.RemainingPaidRuns() < 1 {
		return AdvanceDecision{Action: AdvanceClose, Reason: PartyPhaseBudgetExhausted}
	}
	return AdvanceDecision{Action: AdvanceTakeaways, Reason: reason}
}

// ============================================================================
// Facilitator directive bodies — pure (ADR-0027 §3.2; cross-talk persona context is WS-C)
// ============================================================================

// FacilitatorRound is the data a facilitator-run directive is rendered from. It is the party-mode
// counterpart of mentiondispatch's orchestrationRunBody — the NL body the re-minted Coordinator run
// pulls at dispatch time. The per-VOICE cross-talk context (persona blurb + "What Others Said This
// Round" + rolling summary) is assembled by WS-C into each dispatched voice's body; THIS directive is
// the FACILITATOR's round brief: select + dispatch, under the server budget cap.
type FacilitatorRound struct {
	ProjectID      string    // room key "namespace/name"
	ThreadID       uuid.UUID // the party thread to read + reply into
	TopicMessageID uuid.UUID // the opt-in party_start message that raised the topic
	Round          int       // the round this facilitator run drives (1-based)
	MaxRounds      int       // the hard round budget (for the brief's "round N of M")
	VoiceCap       int       // the server-enforced max voices this round (VoicesAllowedThisRound)
	UserNamed      []string  // agents the human named → the facilitator must always include these
	Topic          string    // the raised topic, verbatim, for the brief
}

// FacilitatorRoundDirective renders the round-N facilitator brief (ADR-0027 §3.2). It instructs the
// Coordinator to read the thread, select up to VoiceCap relevant voices (rotation + always-include the
// user-named + 1–2 complementary), and dispatch them — and states plainly that the server caps the
// round at VoiceCap voices (§3.3: the budget is enforced at the mint seam, the facilitator's count is
// not trusted). It carries the liveliness mandate (§3 distilled from BMAD): never paraphrase the
// agents; agents must disagree where warranted and may pass in one sentence.
func FacilitatorRoundDirective(d FacilitatorRound) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the Team Coordinator facilitating a bounded party discussion in project %q (round %d of %d).\n\n",
		d.ProjectID, d.Round, d.MaxRounds)
	if strings.TrimSpace(d.Topic) != "" {
		fmt.Fprintf(&b, "The topic under discussion is: «%s»\n\n", strings.TrimSpace(d.Topic))
	}
	fmt.Fprintf(&b, "Read the discussion thread (id %s; topic message %s) via discussion_search or the thread GET before you act, so this round reacts to what was already said.\n\n",
		d.ThreadID, d.TopicMessageID)
	b.WriteString("Select the voices for THIS round:\n")
	fmt.Fprintf(&b, "  - Choose at most %d relevant agents. The server hard-caps the round at %d voices — selecting more will be truncated, not honoured.\n", d.VoiceCap, d.VoiceCap)
	b.WriteString("  - Rotate: do not let the same two agents dominate — prefer agents who have not spoken recently.\n")
	if len(d.UserNamed) > 0 {
		fmt.Fprintf(&b, "  - ALWAYS include the agents the human named (%s), plus 1–2 complementary voices.\n", strings.Join(prefixAll("@", d.UserNamed), ", "))
	} else {
		b.WriteString("  - Pick on relevance to the topic, plus 1–2 complementary voices for a real spread of views.\n")
	}
	b.WriteString("\nDispatch the selected voices (work_item_create + work_item_assign, as you do for a multi-mention), one work item per voice, pointing each at this thread.\n\n")
	b.WriteString("Facilitation rules:\n")
	b.WriteString("  - Do NOT paraphrase, merge, or summarise the agents' answers — the humans hear the agents directly, not your digest.\n")
	b.WriteString("  - Tell each voice to disagree where warranted (no hedging to be polite) and to PASS in one sentence rather than manufacture an opinion.\n")
	b.WriteString("  - You may add one short orchestrator note flagging a disagreement or who to bring in next — but never put words in an agent's mouth.\n")
	fmt.Fprintf(&b, "\n[party] facilitator round=%d/%d voiceCap=%d\n", d.Round, d.MaxRounds, d.VoiceCap)
	return b.String()
}

// TakeawaysDirective renders the end-of-session wrap-up brief (ADR-0027 §3.2). `reason` is the terminal
// phase the session is closing with (closed | converged | budget_exhausted), surfaced so the facilitator
// frames the wrap honestly (a budget-cut debate is not a resolved one). It must summarise the discussion
// as TAKEAWAYS — agreements AND open disagreements — without inventing a consensus that was not reached.
func TakeawaysDirective(d FacilitatorRound, reason string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the Team Coordinator closing the party discussion in project %q after %d round(s).\n\n", d.ProjectID, d.Round)
	switch reason {
	case PartyPhaseConverged:
		b.WriteString("The discussion converged — the team stopped adding new disagreement. ")
	case PartyPhaseBudgetExhausted:
		b.WriteString("The discussion hit its paid-run budget and is being closed — note that it was cut for cost, not necessarily resolved. ")
	default:
		b.WriteString("The discussion reached its round limit. ")
	}
	fmt.Fprintf(&b, "Read the full thread (id %s) and post ONE wrap-up message to it.\n\n", d.ThreadID)
	if strings.TrimSpace(d.Topic) != "" {
		fmt.Fprintf(&b, "Topic: «%s»\n\n", strings.TrimSpace(d.Topic))
	}
	b.WriteString("Post TAKEAWAYS:\n")
	b.WriteString("  - The points the team AGREED on.\n")
	b.WriteString("  - The points still in DISAGREEMENT (name who held which view) — do not paper over an unresolved split.\n")
	b.WriteString("  - Any concrete next step the team surfaced.\n")
	b.WriteString("Do NOT invent a consensus that was not reached, and do NOT dispatch any more voices — this is the final message of the session.\n")
	fmt.Fprintf(&b, "\n[party] takeaways reason=%s rounds=%d\n", reason, d.Round)
	return b.String()
}

// prefixAll returns a copy of names with p prepended to each (for "@name" rendering in the brief).
func prefixAll(p string, names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = p + n
	}
	return out
}
