// ISI-5615 (ISI-5569 WS-D.1, ADR-0027 §3.2) — the operator-side party advancer runnable (P3).
//
// PlanPartyTick (advancer.go) is the pure decision; this file is the I/O shell that runs it on
// every tick. It is a leader-elected mgr.Add runnable (the same shape as HeartbeatSweeper /
// CancelSweeper): a missed tick costs advance latency, never correctness — the state machine
// is purely derived from the durable party_session row and the settle markers.
//
// On each tick:
//  1. ActivePartySessions sweeps the durable session table (no in-memory state).
//  2. For each active session: if a facilitator message is set, read RoundVoiceSettlement.
//  3. PlanPartyTick decides the action.
//  4. Execute: MintRound (coord.CreateWorkItem + RequestDispatch → facilitator run) or CloseSession.
package discussion

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

const (
	// AdvancerInitiator is the §6.5 audit tag for work items minted by the party advancer —
	// "system:party-advancer" provenances each facilitator run as operator-initiated, not human.
	AdvancerInitiator = "system:party-advancer"
)

// AdvancerWorkItemWriter is the narrow create seam the advancer needs from coord (satisfied by
// *coord.WorkItemWriteStore). Narrowed so the advancer is unit-testable with a fake.
type AdvancerWorkItemWriter interface {
	CreateWorkItem(ctx context.Context, in AdvancerCreateInput) (AdvancerWorkItemResult, error)
}

// AdvancerWorkItemResult carries the fields the advancer needs after creating a facilitator work item.
type AdvancerWorkItemResult struct {
	ID string
}

// AdvancerCreateInput mirrors the fields the advancer passes to coord.CreateWorkItem.
type AdvancerCreateInput struct {
	ProjectID string
	TeamID    string
	Title     string
	Body      string
	Principal string
}

// AdvancerWorkItemDispatcher is the narrow dispatch seam (satisfied by *coord.WorkItemDispatchStore).
type AdvancerWorkItemDispatcher interface {
	RequestDispatch(ctx context.Context, in AdvancerDispatchInput) error
}

// AdvancerDispatchInput mirrors coord.RequestDispatchInput for the fields the advancer uses.
type AdvancerDispatchInput struct {
	WorkItemID string
	AgentID    string
	TeamID     string
	Principal  string
	Initiator  string
}

// AdvancerRosterReader resolves the coordinator agent name for a given team (to know which agent to
// dispatch the facilitator run to). Satisfied by the apiserver OrgReader or the Team-CR resolver.
type AdvancerRosterReader interface {
	CoordinatorForTeam(ctx context.Context, teamID string) (agentName string, ok bool, err error)
}

// PartyAdvancer is the leader-elected operator runnable that drives active party sessions forward
// (ADR-0027 §3.2). It holds no per-session in-memory state: each tick re-derives everything from
// the durable party_session row + coord settle markers, so it is restart-safe.
type PartyAdvancer struct {
	Store      *Store
	Writer     AdvancerWorkItemWriter
	Dispatcher AdvancerWorkItemDispatcher
	Roster     AdvancerRosterReader
	Principal  string // e.g. rundrive.OperatorPrincipal ("ksquad-operator")

	// VoiceDispatcher dispatches a single party voice run (ADR-0031 Ruling A §3.1, S-direct). The advancer
	// walks the round's RoundVoices roster one turn at a time, dispatching each through this seam so a voice
	// reacts to the prior voice's freshly-landed reply. It is the SAME discussion.MentionDispatcher the
	// apiserver wires for @-mention run-minting (internal/mentiondispatch), reused verbatim — the operator
	// constructs it over the same coord create/dispatch stores the facilitator mint already rides, so a voice
	// run is minted, board-hidden, ledger-bound (so RoundVoiceSettlement counts it), and body-rendered
	// (PartyContext.RenderVoiceContext) exactly like a human-triggered @-mention. nil disables sequential
	// voice dispatch (the round would never progress past its first turn) — the operator always wires it.
	VoiceDispatcher MentionDispatcher

	Tick     time.Duration // default DefaultPartyAdvancerTick
	SettleTO time.Duration // default DefaultPartyVoiceSettleTimeout (the hard backstop / parallel path)
	// SeqSettleTO is the tightened stuck-voice guard for SEQUENCED sessions (ADR-0031 §3.4, ISI-5640 C4):
	// a session with a RoundVoices roster dispatches one voice in flight at a time, so a dead turn stalls
	// every later turn — this guard advances the cursor sooner than SettleTO. Default
	// DefaultPartySequentialTurnSettleTimeout; the effective per-turn timeout is min(SeqSettleTO, SettleTO)
	// so SettleTO always stays the hard backstop. Ignored for the no-roster (parallel round-sweep) path.
	SeqSettleTO time.Duration
	PostTO      time.Duration // default DefaultPartyFacilitatorPostTimeout
}

// Start implements manager.Runnable. It runs until ctx is done.
func (a *PartyAdvancer) Start(ctx context.Context) error {
	tick := a.Tick
	if tick <= 0 {
		tick = DefaultPartyAdvancerTick
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			a.tick(ctx)
		}
	}
}

func (a *PartyAdvancer) tick(ctx context.Context) {
	sessions, err := a.Store.ActivePartySessions(ctx)
	if err != nil {
		slog.WarnContext(ctx, "party advancer: ActivePartySessions failed", "err", err)
		return
	}
	settleTO := a.SettleTO
	if settleTO <= 0 {
		settleTO = DefaultPartyVoiceSettleTimeout
	}
	seqSettleTO := a.SeqSettleTO
	if seqSettleTO <= 0 {
		seqSettleTO = DefaultPartySequentialTurnSettleTimeout
	}
	postTO := a.PostTO
	if postTO <= 0 {
		postTO = DefaultPartyFacilitatorPostTimeout
	}
	for _, sess := range sessions {
		a.advanceSession(ctx, sess, settleTO, seqSettleTO, postTO)
	}
}

// effectivePartySettleTimeout picks the stuck-voice guard for one session's settlement read (ADR-0031
// §3.4, ISI-5640 C4). For a SEQUENCED session (RoundVoices roster present) a dead turn stalls every later
// turn, so the tightened per-turn guard seqSettleTO applies — but bounded by the backstop, so the
// effective value is min(seqSettleTO, backstop): lowering only the backstop below seqSettleTO still wins
// (the backstop is always the hard ceiling), and a non-positive seqSettleTO falls back to the backstop. A
// no-roster (parallel round-sweep) session always uses the full backstop. Pure, so the tiering is
// exhaustively unit-testable without a DB.
func effectivePartySettleTimeout(hasRoster bool, backstop, seqSettleTO time.Duration) time.Duration {
	if hasRoster && seqSettleTO > 0 && seqSettleTO < backstop {
		return seqSettleTO
	}
	return backstop
}

func (a *PartyAdvancer) advanceSession(ctx context.Context, sess *PartySession, settleTO, seqSettleTO, postTO time.Duration) {
	var settle RoundSettlement
	if sess.CurrentRoundMessageID != nil {
		// ISI-5640 (ADR-0031 §3.4) — a session with a RoundVoices roster is dispatched one turn in flight at
		// a time, so a dead turn stalls every later turn; use the tightened per-turn guard for it, bounded by
		// the hard backstop. A no-roster session (pre-0035 parallel round-sweep) keeps the full backstop — its
		// voices run concurrently, so the original bound is right.
		effectiveSettleTO := effectivePartySettleTimeout(len(sess.RoundVoices) > 0, settleTO, seqSettleTO)
		s, err := a.Store.RoundVoiceSettlement(ctx, *sess.CurrentRoundMessageID, effectiveSettleTO)
		if err != nil {
			slog.WarnContext(ctx, "party advancer: RoundVoiceSettlement failed",
				"sessionID", sess.ID, "round", sess.Round, "err", err)
			return
		}
		settle = s

		// ISI-5615 — charge the just-settled round's voice runs against the paid-run ceiling BEFORE the
		// advance decision (ADR-0027 §3.2 "AdvanceRound + RecordPaidRuns + DecideAdvance", §5.1 hard stop).
		// DecideAdvance (inside PlanPartyTick) is documented to run on the session AS IT WILL BE after the
		// round is recorded, so without this charge the ceiling only ever sees the +1/round facilitator mint
		// and the voice runs go uncounted — the budget never actually stops a runaway debate. RecordPaidRuns
		// may itself flip the session to budget_exhausted (hard ceiling hit mid-round); we re-read the
		// returned row so the decision sees the real budget. The charge is applied once per round: the
		// settled round's current_round_message_id is cleared when the round advances (MintRound) or the
		// session goes terminal (CloseSession / the budget_exhausted flip here), so the next tick no longer
		// re-reads this round's settlement.
		//
		// ISI-5638 (ADR-0031 §3.1): under sequential dispatch the round is NOT complete until EVERY roster
		// voice has been dispatched. An in-flight round where only turn k of N has landed also satisfies
		// settle.Complete() (all DISPATCHED-so-far reached terminal), but the next turn still has to be
		// dispatched — so the charge+advance must hold until Dispatched reaches the roster size. Charging
		// once per round (all N voices together, here) keeps the paid-run budget identical to ADR-0027's
		// parallel fan-out (sequencing adds latency, not runs). A session with no roster (pre-0035) has
		// len(RoundVoices)=0, so Dispatched ≥ 0 always holds and this charges exactly as it shipped.
		roundFullyDispatched := settle.Dispatched >= len(sess.RoundVoices)
		if settle.Complete() && settle.Dispatched > 0 && roundFullyDispatched {
			charged, err := a.Store.RecordPaidRuns(ctx, sess.ID, settle.Dispatched)
			if err != nil {
				if errors.Is(err, ErrPartySessionNotActive) {
					// Closed out from under us (a concurrent close, or RecordPaidRuns already charged and
					// exhausted the budget on a prior tick) — nothing left to advance.
					return
				}
				slog.WarnContext(ctx, "party advancer: RecordPaidRuns failed",
					"sessionID", sess.ID, "round", sess.Round, "voices", settle.Dispatched, "err", err)
				return
			}
			if charged.IsActive() != sess.IsActive() || charged.Phase != sess.Phase {
				slog.InfoContext(ctx, "party advancer: paid-run ceiling reached mid-round",
					"sessionID", sess.ID, "round", sess.Round, "phase", charged.Phase,
					"paidRunsUsed", charged.PaidRunsUsed, "paidRunBudget", charged.Budget.PaidRunBudget)
			}
			sess = charged
		}
	}

	plan := PlanPartyTick(*sess, settle, time.Now(), postTO)
	slog.DebugContext(ctx, "party advancer: tick",
		"sessionID", sess.ID, "round", sess.Round, "action", plan.Action, "note", plan.Note)

	switch plan.Action {
	case PartyTickWait:
		// nothing to do
	case PartyTickDispatchVoice:
		a.dispatchVoice(ctx, sess, plan)
	case PartyTickMintRound:
		a.mintRound(ctx, sess, plan)
	case PartyTickTakeaways:
		a.mintTakeaways(ctx, sess, plan)
	case PartyTickClose:
		if _, err := a.Store.CloseSession(ctx, sess.ID, plan.Reason); err != nil {
			slog.WarnContext(ctx, "party advancer: CloseSession failed",
				"sessionID", sess.ID, "reason", plan.Reason, "err", err)
		}
	}
}

// dispatchVoice dispatches the ONE next voice of the current round (ADR-0031 Ruling A §3.1, S-direct). The
// round's facilitator has posted its roster (sess.RoundVoices) and every earlier turn has settled, so the
// advancer mints a run for RoundVoices[plan.VoiceIndex], pushing it the cross-talk context of every turn
// taken SO FAR — including earlier IN-round turns — via PartyContext. The next tick sees this voice's
// mention_dispatch row (cursor++), waits for it to settle, then dispatches the one after. This is the
// one-voice-in-flight, react-to-prior turn-taking; all guardrails (ADR-0031 §3.4) hold a fortiori since a
// sequential debate is strictly less concurrent than ADR-0027's.
//
// It reuses the shipped @-mention run-minting path verbatim (VoiceDispatcher == internal/mentiondispatch):
// claim-idempotency on (facilitatorMessageID, voice) makes a repeated tick a no-op, the item is board-hidden
// and ledger-bound, and the body is PartyContext.RenderVoiceContext + the standard read-thread/reply footer.
// Best-effort: the facilitator message is already durable; a dispatch error logs and the next tick retries
// the same cursor (the released claim leaves no mention_dispatch row, so Dispatched does not advance).
func (a *PartyAdvancer) dispatchVoice(ctx context.Context, sess *PartySession, plan PartyTickPlan) {
	if a.VoiceDispatcher == nil {
		slog.WarnContext(ctx, "party advancer: no voice dispatcher wired — cannot sequence voices",
			"sessionID", sess.ID, "round", sess.Round)
		return
	}
	if sess.CurrentRoundMessageID == nil {
		return // defensive: PlanPartyTick only emits DispatchVoice once the facilitator posted
	}
	if plan.VoiceIndex < 0 || plan.VoiceIndex >= len(sess.RoundVoices) {
		return // defensive: cursor out of range (roster changed under us)
	}
	voice := sess.RoundVoices[plan.VoiceIndex]
	facilitatorMsg := *sess.CurrentRoundMessageID

	// Assemble "What Others Said This Round" from the thread as it stands NOW — so this voice reacts to
	// every prior turn, including the earlier in-round turns the previous ticks dispatched (§3.1). A read
	// failure degrades to empty peers (the voice still gets its persona frame + can pull the thread itself).
	var roundMsgs []Message
	if thread, terr := a.Store.GetThread(ctx, sess.ProjectID, sess.TeamID, sess.ThreadID); terr == nil && thread != nil {
		roundMsgs = flattenMessages(thread.Messages)
	} else if terr != nil {
		slog.WarnContext(ctx, "party advancer: GetThread for voice context failed (best-effort)",
			"sessionID", sess.ID, "round", sess.Round, "voice", voice, "err", terr)
	}

	d := MentionDispatch{
		ProjectID:            sess.ProjectID,
		ThreadID:             sess.ThreadID,
		MessageID:            facilitatorMsg, // the round's facilitator message — the settlement key
		TeamID:               sess.TeamID,
		AgentName:            voice,
		HopDepth:             1, // the facilitator posts at hop 0; its dispatched voices run at hop 1
		TriggeredByPrincipal: a.advancerPrincipal(),
		Party: &PartyContext{
			Round:          sess.Round,
			Persona:        PersonaBlurb{Name: voice},
			PeersThisRound: AssemblePeerTurns(roundMsgs, voice, nil),
		},
	}
	if err := a.VoiceDispatcher.DispatchMention(ctx, d); err != nil {
		slog.WarnContext(ctx, "party advancer: voice dispatch failed (best-effort, will retry)",
			"sessionID", sess.ID, "round", sess.Round, "voice", voice, "cursor", plan.VoiceIndex, "err", err)
		return
	}
	slog.InfoContext(ctx, "party advancer: voice dispatched (sequential)",
		"sessionID", sess.ID, "round", sess.Round, "voice", voice,
		"turn", plan.VoiceIndex+1, "ofTurns", len(sess.RoundVoices))
}

// mintRound mints the facilitator run for round plan.FromRound+1 (ADR-0027 §3.2).
func (a *PartyAdvancer) mintRound(ctx context.Context, sess *PartySession, plan PartyTickPlan) {
	// CAS: advance the session row (increments round, clears current_round_message_id).
	updated, err := a.Store.MintRound(ctx, sess.ID, plan.FromRound)
	if err != nil {
		slog.WarnContext(ctx, "party advancer: MintRound CAS failed (another leader won or session closed)",
			"sessionID", sess.ID, "fromRound", plan.FromRound, "err", err)
		return
	}

	// Resolve the coordinator agent for this team.
	coordinator, ok, err := a.Roster.CoordinatorForTeam(ctx, sess.TeamID.String())
	if err != nil || !ok {
		slog.WarnContext(ctx, "party advancer: no coordinator for team — cannot mint facilitator",
			"sessionID", sess.ID, "teamID", sess.TeamID, "err", err)
		return
	}

	// Build the facilitator directive.
	round := updated.Round
	directive := FacilitatorRoundDirective(FacilitatorRound{
		ProjectID:      sess.ProjectID,
		ThreadID:       sess.ThreadID,
		TopicMessageID: sess.TopicMessageID,
		Round:          round,
		MaxRounds:      sess.Budget.MaxRounds,
		VoiceCap:       sess.Budget.MaxVoicesPerRound,
	})

	title := fmt.Sprintf("Party facilitation — round %d of %d", round, sess.Budget.MaxRounds)
	item, createErr := a.Writer.CreateWorkItem(ctx, AdvancerCreateInput{
		ProjectID: sess.ProjectID,
		TeamID:    sess.TeamID.String(),
		Title:     title,
		Body:      directive,
		Principal: a.advancerPrincipal(),
	})
	if createErr != nil {
		slog.WarnContext(ctx, "party advancer: CreateWorkItem (facilitator) failed",
			"sessionID", sess.ID, "round", round, "err", createErr)
		return
	}

	if dispErr := a.Dispatcher.RequestDispatch(ctx, AdvancerDispatchInput{
		WorkItemID: item.ID,
		AgentID:    coordinator,
		TeamID:     sess.TeamID.String(),
		Principal:  a.advancerPrincipal(),
		Initiator:  AdvancerInitiator,
	}); dispErr != nil {
		slog.WarnContext(ctx, "party advancer: RequestDispatch (facilitator) failed",
			"sessionID", sess.ID, "round", round, "itemID", item.ID, "err", dispErr)
		return
	}
	slog.InfoContext(ctx, "party advancer: facilitator run minted",
		"sessionID", sess.ID, "round", round, "coordinator", coordinator, "itemID", item.ID)
}

// mintTakeaways mints the takeaways facilitator run then closes the session.
func (a *PartyAdvancer) mintTakeaways(ctx context.Context, sess *PartySession, plan PartyTickPlan) {
	updated, err := a.Store.MintRound(ctx, sess.ID, plan.FromRound)
	if err != nil {
		slog.WarnContext(ctx, "party advancer: MintRound (takeaways) CAS failed",
			"sessionID", sess.ID, "fromRound", plan.FromRound, "err", err)
		return
	}

	coordinator, ok, err := a.Roster.CoordinatorForTeam(ctx, sess.TeamID.String())
	if err != nil || !ok {
		slog.WarnContext(ctx, "party advancer: no coordinator for team — skip takeaways",
			"sessionID", sess.ID, "teamID", sess.TeamID, "err", err)
		if _, cerr := a.Store.CloseSession(ctx, sess.ID, plan.Reason); cerr != nil {
			slog.WarnContext(ctx, "party advancer: CloseSession (no coordinator) failed",
				"sessionID", sess.ID, "err", cerr)
		}
		return
	}

	round := updated.Round
	directive := TakeawaysDirective(FacilitatorRound{
		ProjectID:      sess.ProjectID,
		ThreadID:       sess.ThreadID,
		TopicMessageID: sess.TopicMessageID,
		Round:          round,
		MaxRounds:      sess.Budget.MaxRounds,
		VoiceCap:       sess.Budget.MaxVoicesPerRound,
	}, plan.Reason)

	title := fmt.Sprintf("Party takeaways — round %d", round)
	item, createErr := a.Writer.CreateWorkItem(ctx, AdvancerCreateInput{
		ProjectID: sess.ProjectID,
		TeamID:    sess.TeamID.String(),
		Title:     title,
		Body:      directive,
		Principal: a.advancerPrincipal(),
	})
	if createErr != nil {
		slog.WarnContext(ctx, "party advancer: CreateWorkItem (takeaways) failed",
			"sessionID", sess.ID, "round", round, "err", createErr)
		// Still close the session so we don't loop forever.
		if _, cerr := a.Store.CloseSession(ctx, sess.ID, plan.Reason); cerr != nil {
			slog.WarnContext(ctx, "party advancer: CloseSession (post takeaways-create failure) failed",
				"sessionID", sess.ID, "err", cerr)
		}
		return
	}

	if dispErr := a.Dispatcher.RequestDispatch(ctx, AdvancerDispatchInput{
		WorkItemID: item.ID,
		AgentID:    coordinator,
		TeamID:     sess.TeamID.String(),
		Principal:  a.advancerPrincipal(),
		Initiator:  AdvancerInitiator,
	}); dispErr != nil {
		slog.WarnContext(ctx, "party advancer: RequestDispatch (takeaways) failed",
			"sessionID", sess.ID, "itemID", item.ID, "err", dispErr)
	} else {
		slog.InfoContext(ctx, "party advancer: takeaways run minted",
			"sessionID", sess.ID, "round", round, "coordinator", coordinator, "itemID", item.ID)
	}

	if _, cerr := a.Store.CloseSession(ctx, sess.ID, plan.Reason); cerr != nil {
		slog.WarnContext(ctx, "party advancer: CloseSession (post takeaways) failed",
			"sessionID", sess.ID, "reason", plan.Reason, "err", cerr)
	}
}

func (a *PartyAdvancer) advancerPrincipal() string {
	if a.Principal != "" {
		return a.Principal
	}
	return "ksquad-operator"
}
