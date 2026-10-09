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

	Tick     time.Duration // default DefaultPartyAdvancerTick
	SettleTO time.Duration // default DefaultPartyVoiceSettleTimeout
	PostTO   time.Duration // default DefaultPartyFacilitatorPostTimeout
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
	postTO := a.PostTO
	if postTO <= 0 {
		postTO = DefaultPartyFacilitatorPostTimeout
	}
	for _, sess := range sessions {
		a.advanceSession(ctx, sess, settleTO, postTO)
	}
}

func (a *PartyAdvancer) advanceSession(ctx context.Context, sess *PartySession, settleTO, postTO time.Duration) {
	var settle RoundSettlement
	if sess.CurrentRoundMessageID != nil {
		s, err := a.Store.RoundVoiceSettlement(ctx, *sess.CurrentRoundMessageID, settleTO)
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
		if settle.Complete() && settle.Dispatched > 0 {
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
