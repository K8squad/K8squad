// ISI-5585 (ISI-5569 WS-B, ruling ADR-0027) — the party-mode session model + hard cost budgets.
//
// Party mode (ISI-5569) is a facilitated, bounded, multi-round team debate in a discussion room. The
// one force that separates it from BMAD: in k8squad every agent turn is a separate PAID Run, so cost
// control is the design center. This file is the WS-B data + policy layer the downstream workstreams
// consume:
//
//   - WS-C (cross-talk context) and WS-D (the facilitator round loop / advancer) both need the
//     per-thread session row to exist — this file creates and reads it.
//   - WS-D's event-driven advancer (ADR-0027 §3) consults the BUDGET POLICY here (CanStartRound /
//     VoicesAllowedThisRound) and mutates the session (AdvanceRound / RecordPaidRuns / Close) as each
//     round settles. The budget is a SERVER gate, not an agent-trusted instruction (§3.3): the
//     advancer refuses to mint once the ceiling would be crossed and flips the session to
//     budget_exhausted — a HARD ceiling, dropped/capped counts logged non-silently (§5.1).
//
// Opt-in integrity (§5.2): a party SESSION is opened ONLY by StartPartySession on a server-stamped,
// human-authored kind='party_start' message. A bare kind='text', audience='party' post keeps its
// ISI-5265 one-shot broadcast behaviour (no session, no rounds) — "a plain thanks must never start a
// paid debate" (the ISI-5264 carry-forward risk). party_start is NOT in normalizeKind's generic set,
// so a generic PostMessage cannot mint a party_start that bypasses session creation; the ONLY writer
// is StartPartySession (same discipline as PostDecisionRequest).
//
// Fence (ADR-0019, reasoned in ADR-0027 §4.2): party_session is facilitation-session bookkeeping, not
// work-item custody. Its `phase` is the SESSION lifecycle, never a custody-state; no custody is
// expressed or transferred here. Every facilitator/voice run is still minted + fenced in coord.
package discussion

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ============================================================================
// Kind, phasees, default budgets
// ============================================================================

const (
	// KindPartyStart marks the explicit opt-in message that opens a party session (ADR-0027 §5.2).
	KindPartyStart = "party_start"

	// Party-session lifecycle (the session state machine). These are SESSION states, never a work
	// item's custody-state (ADR-0027 §4.2 fence carve-out).
	PartyPhaseActive          = "active"           // the debate is live; the advancer may mint rounds
	PartyPhaseClosed          = "closed"           // ended normally (max rounds reached / takeaways done)
	PartyPhaseConverged       = "converged"        // the facilitator stopped early (no new disagreement)
	PartyPhaseBudgetExhausted = "budget_exhausted" // the hard paid-run ceiling was reached (§5.1)
)

const (
	// DefaultMaxPartyRounds is the hard cap on facilitator rounds (ADR-0027 §5.1). Round max_rounds is
	// the takeaways/close round.
	DefaultMaxPartyRounds = 3

	// Voice-per-round bounds (ADR-0027 §5.1): the facilitator selects 2–4 relevant voices per round,
	// defaulting to 3. These bound the per-round fan-out BEFORE the absolute paid-run ceiling.
	MinVoicesPerRound     = 2
	MaxVoicesPerRound     = 4
	DefaultVoicesPerRound = 3
)

// ============================================================================
// Errors
// ============================================================================

var (
	// ErrPartyStartForbidden — only a human may open a party session (ADR-0027 §5.2). An agent cannot
	// start a paid debate; that keeps the anti-N² guarantee intact at the entry point.
	ErrPartyStartForbidden = errors.New("discussion: only a human may start a party session")

	// ErrNoActivePartySession — no live session for the thread (the advancer's lookup miss, or a gate
	// consulted on a non-party thread). Not an error the caller must surface — the dispatch rules just
	// stay exactly as today (ADR-0027 §4.4).
	ErrNoActivePartySession = errors.New("discussion: no active party session for thread")

	// ErrPartySessionNotActive — a mutation (advance/record/close) targeted a session no longer active
	// (already closed/converged/budget_exhausted). A CAS loser, not a crash.
	ErrPartySessionNotActive = errors.New("discussion: party session is not active")

	// ErrInvalidPartyBudget — a supplied budget override is incoherent (non-positive cap).
	ErrInvalidPartyBudget = errors.New("discussion: party budget caps must be positive")
)

// ============================================================================
// Budget policy — pure, exhaustively unit-testable (no I/O)
// ============================================================================

// PartyBudget is the hard cost budget for a session (ADR-0027 §5.1). Defaulted from config at create,
// stored on the session row, and the ONLY loop bound inside a session (the hop-cap is the wrong bound
// for a deliberate multi-round debate — §4.4).
type PartyBudget struct {
	MaxRounds         int // hard cap on facilitator rounds
	MaxVoicesPerRound int // per-round voice fan-out cap (2–4)
	PaidRunBudget     int // absolute ceiling on total paid runs (facilitator + voices) per session
}

// DefaultPartyBudget is the config-defaulted budget. The paid-run ceiling is DERIVED so it always
// covers the worst case the round/voice caps permit — one facilitator mint per round plus up to
// max_voices_per_round voices per round (ADR-0027 §5.1: ~= rounds×voices + rounds facilitator runs).
func DefaultPartyBudget() PartyBudget {
	return PartyBudget{
		MaxRounds:         DefaultMaxPartyRounds,
		MaxVoicesPerRound: DefaultVoicesPerRound,
	}.withDerivedCeiling()
}

// normalize fills zero fields from the default and clamps max_voices_per_round into the 2–4 band, then
// derives the paid-run ceiling when the caller left it unset. A fully-specified override is validated
// (non-positive caps are rejected) and its explicit ceiling is honoured as the hard ceiling.
func (b PartyBudget) normalize() (PartyBudget, error) {
	if b.MaxRounds == 0 {
		b.MaxRounds = DefaultMaxPartyRounds
	}
	if b.MaxVoicesPerRound == 0 {
		b.MaxVoicesPerRound = DefaultVoicesPerRound
	}
	if b.MaxRounds < 0 || b.MaxVoicesPerRound < 0 || b.PaidRunBudget < 0 {
		return PartyBudget{}, ErrInvalidPartyBudget
	}
	// Clamp the per-round voice cap into the ruled 2–4 band so a config typo can neither silence a
	// debate (0/1 voice) nor blow the per-round fan-out past the ADR band.
	if b.MaxVoicesPerRound < MinVoicesPerRound {
		b.MaxVoicesPerRound = MinVoicesPerRound
	}
	if b.MaxVoicesPerRound > MaxVoicesPerRound {
		b.MaxVoicesPerRound = MaxVoicesPerRound
	}
	if b.PaidRunBudget == 0 {
		b = b.withDerivedCeiling()
	}
	return b, nil
}

// withDerivedCeiling computes the hard paid-run ceiling from the round/voice caps (ADR-0027 §5.1):
// each round mints one facilitator run plus up to max_voices_per_round voice runs.
func (b PartyBudget) withDerivedCeiling() PartyBudget {
	b.PaidRunBudget = b.MaxRounds*b.MaxVoicesPerRound + b.MaxRounds
	return b
}

// PartySession is the per-thread facilitation-session aggregate (discussion.party_session). It mirrors
// the §4.3 columns exactly. The budget-policy methods below are PURE (no I/O) so WS-D's advancer can
// decide whether to mint a round/voices without a round-trip, and the gating is exhaustively testable.
type PartySession struct {
	ID             uuid.UUID   `json:"id"`
	ThreadID       uuid.UUID   `json:"threadId"`
	ProjectID      string      `json:"projectId"`
	TeamID         uuid.UUID   `json:"teamId"`
	StartedBy      string      `json:"startedBy"`
	TopicMessageID uuid.UUID   `json:"topicMessageId"`
	Round          int         `json:"round"`
	Budget         PartyBudget `json:"budget"`
	PaidRunsUsed   int         `json:"paidRunsUsed"`
	Phase          string      `json:"phase"`
	OpenedAt       time.Time   `json:"openedAt"`
	ClosedAt       *time.Time  `json:"closedAt,omitempty"`
}

// IsActive reports whether the session is live (the advancer may mint).
func (s PartySession) IsActive() bool { return s.Phase == PartyPhaseActive }

// RemainingPaidRuns is the paid-run headroom left under the hard ceiling (never negative).
func (s PartySession) RemainingPaidRuns() int {
	if s.PaidRunsUsed >= s.Budget.PaidRunBudget {
		return 0
	}
	return s.Budget.PaidRunBudget - s.PaidRunsUsed
}

// CanStartRound reports whether the advancer may mint the NEXT facilitator round (ADR-0027 §3.2/§5.1).
// It is the hard gate that replaces the hop-cap inside a session: the session must be active, have a
// round left under max_rounds, and have paid-run headroom for at least the facilitator mint. `reason`
// names the terminal state to set when the gate is closed (budget_exhausted vs. closed), so the
// advancer never mints past the ceiling and the stop is logged non-silently.
func (s PartySession) CanStartRound() (ok bool, reason string) {
	if !s.IsActive() {
		return false, s.Phase
	}
	if s.RemainingPaidRuns() < 1 {
		return false, PartyPhaseBudgetExhausted
	}
	if s.Round >= s.Budget.MaxRounds {
		return false, PartyPhaseClosed
	}
	return true, ""
}

// VoicesAllowedThisRound clamps a facilitator's REQUESTED voice count to the per-round cap AND the
// remaining paid-run headroom, reserving one run for the facilitator mint itself (ADR-0027 §3.3: the
// budget is enforced at the mint seam, never trusted to the agent). It returns how many voices may be
// dispatched and how many were `capped` (dropped), so the caller logs a non-silent truncation (§5.1).
// Negative/zero requests dispatch nobody.
func (s PartySession) VoicesAllowedThisRound(requested int) (allowed, capped int) {
	if requested <= 0 {
		return 0, 0
	}
	allowed = requested
	if allowed > s.Budget.MaxVoicesPerRound {
		allowed = s.Budget.MaxVoicesPerRound
	}
	// Reserve the facilitator run already counted for this round's mint; voices draw from what is left.
	headroom := s.RemainingPaidRuns()
	if headroom < 0 {
		headroom = 0
	}
	if allowed > headroom {
		allowed = headroom
	}
	if allowed < 0 {
		allowed = 0
	}
	return allowed, requested - allowed
}

// ============================================================================
// Opt-in gating — pure (ADR-0027 §5.2)
// ============================================================================

// PartyStartAllowed is the opt-in guard: a party session may be opened ONLY by a human (an agent
// cannot start a paid debate — the anti-N² guarantee at the entry point). Agent-vs-human is DERIVED
// from the server-stamped provenance (auth.AgentID), never a body flag.
func PartyStartAllowed(auth AuthorContext) error {
	if auth.AgentID != nil {
		return ErrPartyStartForbidden
	}
	return nil
}

// ============================================================================
// Store — party-session lifecycle
// ============================================================================

const partySessionSelect = `
	SELECT id, thread_id, project_id, team_id, started_by, topic_message_id,
	       round, max_rounds, max_voices_per_round, paid_run_budget, paid_runs_used,
	       phase, opened_at, closed_at
	FROM discussion.party_session`

// scanPartySession hydrates one PartySession from a partySessionSelect row.
func scanPartySession(sc interface{ Scan(...any) error }) (*PartySession, error) {
	var s PartySession
	var closedAt sql.NullTime
	if err := sc.Scan(
		&s.ID, &s.ThreadID, &s.ProjectID, &s.TeamID, &s.StartedBy, &s.TopicMessageID,
		&s.Round, &s.Budget.MaxRounds, &s.Budget.MaxVoicesPerRound, &s.Budget.PaidRunBudget,
		&s.PaidRunsUsed, &s.Phase, &s.OpenedAt, &closedAt,
	); err != nil {
		return nil, err
	}
	if closedAt.Valid {
		t := closedAt.Time
		s.ClosedAt = &t
	}
	return &s, nil
}

// StartPartySession is the opt-in entry point (ADR-0027 §5.2): it appends a server-stamped,
// human-authored kind='party_start' message and opens the per-thread session row in one transaction.
// Idempotent on the active-session invariant — if a debate is already live on the thread, the insert
// loses the uq_party_session_active_thread conflict and the EXISTING active session is returned (no
// second paid debate, no second start message committed). The returned message is the topic/start
// message the WS-D advancer mints round 1 from.
//
// Budgets are defaulted from config (DefaultPartyBudget) unless a coherent override is supplied; the
// stored ceiling is the hard cap the advancer enforces. started_by + author provenance are stamped
// from auth (AC3: never the body).
func (s *Store) StartPartySession(ctx context.Context, projectID string, teamID, threadID uuid.UUID, auth AuthorContext, body string, budget *PartyBudget) (*PartySession, *Message, error) {
	if body == "" {
		return nil, nil, ErrEmptyBody
	}
	if err := PartyStartAllowed(auth); err != nil {
		return nil, nil, err
	}
	b := DefaultPartyBudget()
	if budget != nil {
		nb, err := budget.normalize()
		if err != nil {
			return nil, nil, err
		}
		b = nb
	}
	if err := s.assertThreadInScope(ctx, projectID, teamID, threadID); err != nil {
		return nil, nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback() }()

	m := Message{
		ThreadID:        threadID,
		AuthorPrincipal: auth.Principal,
		AuthorAgentID:   auth.AgentID,
		AuthorRunID:     auth.RunID,
		Body:            body,
		Audience:        "party", // a party debate is room-visible by construction
		Kind:            KindPartyStart,
	}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO discussion.message
		    (thread_id, author_principal, author_agent_id, author_run_id, body, audience, kind)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, created_at`,
		threadID, auth.Principal, auth.agentID(), auth.runID(), body, m.Audience, m.Kind,
	).Scan(&m.ID, &m.CreatedAt)
	if err != nil {
		return nil, nil, fmt.Errorf("post party_start message: %w", err)
	}

	var sess *PartySession
	// ON CONFLICT on a PARTIAL unique index is inferred by the index predicate (thread_id WHERE
	// phase='active'), which Postgres matches by the index-inference clause below — not a named
	// constraint. A live debate on the thread loses this conflict and inserts nothing (n=0).
	tag, err := tx.ExecContext(ctx, `
		INSERT INTO discussion.party_session
		    (thread_id, project_id, team_id, started_by, topic_message_id,
		     max_rounds, max_voices_per_round, paid_run_budget)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (thread_id) WHERE phase = 'active' DO NOTHING`,
		threadID, projectID, teamID, auth.Principal, m.ID,
		b.MaxRounds, b.MaxVoicesPerRound, b.PaidRunBudget)
	if err != nil {
		return nil, nil, fmt.Errorf("open party session: %w", err)
	}
	if n, _ := tag.RowsAffected(); n == 0 {
		// A debate is already live on this thread — idempotent opt-in. Discard the just-inserted
		// start message (roll back) and return the existing active session (§5.2: no second debate).
		_ = tx.Rollback()
		existing, gerr := s.ActivePartySession(ctx, threadID)
		if gerr != nil {
			return nil, nil, gerr
		}
		return existing, nil, nil
	}
	sess, err = s.activePartySessionTx(ctx, tx, threadID)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return sess, &m, nil
}

// ActivePartySession returns the thread's live session, or ErrNoActivePartySession if none. This is
// the advancer's / dispatch gate's single indexed read (ADR-0027 §4.4): active + within budget → the
// facilitator may dispatch; no active session → the ordinary dispatch rules apply verbatim.
func (s *Store) ActivePartySession(ctx context.Context, threadID uuid.UUID) (*PartySession, error) {
	sess, err := scanPartySession(s.db.QueryRowContext(ctx,
		partySessionSelect+` WHERE thread_id = $1 AND phase = 'active'`, threadID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoActivePartySession
	}
	return sess, err
}

// activePartySessionTx is the in-transaction variant used by StartPartySession to read back the row it
// just opened (so the create and the read see the same snapshot).
func (s *Store) activePartySessionTx(ctx context.Context, tx *sql.Tx, threadID uuid.UUID) (*PartySession, error) {
	sess, err := scanPartySession(tx.QueryRowContext(ctx,
		partySessionSelect+` WHERE thread_id = $1 AND phase = 'active'`, threadID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoActivePartySession
	}
	return sess, err
}

// GetPartySession reads a session by id (any phase) — the WS-D advancer's re-read after a mutation and
// the ISI-5617 get-by-id read path. Unlike ActivePartySession it does NOT filter on phase, so a closed /
// converged / budget_exhausted session is still reachable after a debate ends (the terminal takeaways the
// console renders — ISI-5613 Gap 2). Returns ErrNoActivePartySession (→ 404) when no row has that id.
func (s *Store) GetPartySession(ctx context.Context, id uuid.UUID) (*PartySession, error) {
	sess, err := scanPartySession(s.db.QueryRowContext(ctx, partySessionSelect+` WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoActivePartySession
	}
	return sess, err
}

// ListPartySessionsByThread returns a thread's party sessions newest-first (ISI-5617, ISI-5613 Gap 2).
// With includeClosed=false it mirrors ActivePartySession's filter (only the live session, at most one by
// the partial-unique invariant), so it is a safe superset of the existing active read. With
// includeClosed=true it returns every session the thread has ever hosted — including the TERMINAL row the
// console needs for post-close takeaways (phase reason, final round count, paid-run tally) once
// …/active 404s. Ordered opened_at DESC so the most recent debate is first. An empty result is a nil
// slice (the handler renders it as []), never an error — a thread with no debate is not a miss.
func (s *Store) ListPartySessionsByThread(ctx context.Context, threadID uuid.UUID, includeClosed bool) ([]PartySession, error) {
	q := partySessionSelect + ` WHERE thread_id = $1`
	if !includeClosed {
		q += ` AND phase = 'active'`
	}
	q += ` ORDER BY opened_at DESC`
	rows, err := s.db.QueryContext(ctx, q, threadID)
	if err != nil {
		return nil, fmt.Errorf("list party sessions by thread: %w", err)
	}
	defer rows.Close()
	var out []PartySession
	for rows.Next() {
		sess, err := scanPartySession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sess)
	}
	return out, rows.Err()
}

// AdvanceRound bumps the round counter by one on a still-active session (ADR-0027 §3.2: the advancer
// increments the round as each round settles). It is a CAS on phase='active' AND the observed round,
// so two concurrent settle events cannot double-advance. Returns ErrPartySessionNotActive if the
// session closed or another advance already moved the round.
func (s *Store) AdvanceRound(ctx context.Context, id uuid.UUID, fromRound int) (*PartySession, error) {
	tag, err := s.db.ExecContext(ctx, `
		UPDATE discussion.party_session
		   SET round = round + 1
		 WHERE id = $1 AND phase = 'active' AND round = $2`, id, fromRound)
	if err != nil {
		return nil, err
	}
	if n, _ := tag.RowsAffected(); n == 0 {
		return nil, ErrPartySessionNotActive
	}
	return s.GetPartySession(ctx, id)
}

// RecordPaidRuns adds n to the running paid-run tally and, if the hard ceiling is now reached, flips
// the session to budget_exhausted in the SAME statement (ADR-0027 §5.1: the ceiling is a hard stop,
// not advisory). n must be ≥ 0. It CAS-guards on phase='active' so a closed session is never
// re-counted. Returns the updated session; the caller logs the budget_exhausted transition non-silently.
func (s *Store) RecordPaidRuns(ctx context.Context, id uuid.UUID, n int) (*PartySession, error) {
	if n < 0 {
		return nil, fmt.Errorf("discussion: paid-run delta must be non-negative, got %d", n)
	}
	tag, err := s.db.ExecContext(ctx, `
		UPDATE discussion.party_session
		   SET paid_runs_used = paid_runs_used + $2,
		       phase = CASE WHEN paid_runs_used + $2 >= paid_run_budget
		                     THEN 'budget_exhausted' ELSE phase END,
		       closed_at = CASE WHEN paid_runs_used + $2 >= paid_run_budget
		                        THEN now() ELSE closed_at END
		 WHERE id = $1 AND phase = 'active'`, id, n)
	if err != nil {
		return nil, err
	}
	if rows, _ := tag.RowsAffected(); rows == 0 {
		return nil, ErrPartySessionNotActive
	}
	return s.GetPartySession(ctx, id)
}

// CloseSession ends a live session with a terminal phase (closed | converged | budget_exhausted) and
// stamps closed_at (ADR-0027 §3.2 end-of-session / §5.1 budget stop). CAS on phase='active' so a
// double-close is a no-op (ErrPartySessionNotActive). An unknown phase is rejected before the write.
func (s *Store) CloseSession(ctx context.Context, id uuid.UUID, phase string) (*PartySession, error) {
	switch phase {
	case PartyPhaseClosed, PartyPhaseConverged, PartyPhaseBudgetExhausted:
	default:
		return nil, fmt.Errorf("discussion: invalid terminal party phase %q", phase)
	}
	tag, err := s.db.ExecContext(ctx, `
		UPDATE discussion.party_session
		   SET phase = $2, closed_at = now()
		 WHERE id = $1 AND phase = 'active'`, id, phase)
	if err != nil {
		return nil, err
	}
	if n, _ := tag.RowsAffected(); n == 0 {
		return nil, ErrPartySessionNotActive
	}
	return s.GetPartySession(ctx, id)
}
