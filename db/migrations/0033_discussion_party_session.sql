-- 0033_discussion_party_session.sql — ISI-5585 (ISI-5569 WS-B, ruling ADR-0027 §4.3):
-- the per-thread "party mode" facilitation-session state + hard cost budgets.
--
-- Party mode (ISI-5569) is a facilitated, bounded, multi-round team debate in a discussion room.
-- Every agent turn in k8squad is a separate PAID Run (unlike BMAD's single cheap context), so the
-- design center is COST CONTROL: a party *session* is an explicit opt-in act with hard round/voice/
-- paid-run budgets, never today's bare-party broadcast (ADR-0027 §5; the ISI-5264 carry-forward risk
-- that "a plain thanks must never start a paid debate").
--
-- The event-driven facilitator advancer (ADR-0027 §3, WS-D) is level-triggered off the ADR-0020
-- run-settle marker and re-mints the Coordinator-as-facilitator per round. It needs ONE durable,
-- mutable per-thread aggregate to carry the round counter + budget tally across restarts — the
-- append-only discussion.thread/message (0004) structurally cannot hold a counter, and coord is the
-- wrong home (it would imply a custody/claim record this is not). This migration adds that aggregate
-- plus the opt-in message kind. Two additive, forward-only, backfill-safe changes:
--
--   (1) kind='party_start' joins the allowed message kinds — the explicit opt-in affordance (§5.2).
--   (2) discussion.party_session — the mutable per-thread facilitation-session row (§4.3).
--
-- ────────────────────────────────────────────────────────────────────────────────────────────────
-- FENCE CARVE-OUT (ADR-0019 / 0004 header, deliberate — reasoned in ADR-0027 §4.2):
-- discussion.party_session is FACILITATION-SESSION bookkeeping, NOT work-item custody. The ADR-0019
-- fence invariant is that the room cannot be a coordination record for work-item custody — no
-- claim/lease/fence_token/holder/assignee/state/status column, no custody transfer. This table
-- honours that by construction: it carries NO fence_token/holder/assignee/lease/custody column; its
-- lifecycle column is named `phase` (NOT `state`/`status`) — EXACTLY the 0031 decision_request
-- discipline, precisely so no token from the 0004 fence contract test's forbidden list
-- (claim/lease/fence_token/state/holder/assignee/status/checked_out_by/holder_principal/custody)
-- enters the discussion schema. `phase` is the SESSION lifecycle (active|closed|converged|
-- budget_exhausted), not any work item's custody-state; no work item's custody is expressed here.
-- Every facilitator/voice run is still minted through coord.CreateWorkItem/RequestDispatch and fenced
-- there, exactly as today. Precedent: discussion.mention_dispatch (0027) is already a mutable,
-- non-message bookkeeping table in the discussion schema with no custody column — party_session is the
-- same category, one aggregation level up (a per-thread session vs. a per-(message,agent) dispatch).
-- FR-B3 exempts the whole discussion schema.
--
-- Forward-only, additive; applied once by the apiserver migration runner in filename order.

-- 1. Widen the kind CHECK (last set in 0031) so a party_start message is representable. party_start
--    is the explicit opt-in that opens a session (§5.2); a bare kind='text', audience='party' message
--    retains EXACTLY its ISI-5265 behaviour (bounded one-shot broadcast, no session, no rounds).
ALTER TABLE discussion.message DROP CONSTRAINT kind_must_be_text_or_extension;
ALTER TABLE discussion.message
    ADD CONSTRAINT kind_must_be_text_or_extension CHECK (
        kind = 'text' OR kind IN ('structured', 'task', 'decision', 'vote', 'proposal',
                                   'decision_request', 'party_start')
    );

-- 2. The per-thread party-session aggregate (ADR-0027 §4.3). Keyed by thread_id; 1 active per thread
--    (a thread cannot run two concurrent debates — the partial UNIQUE below). Created at opt-in on the
--    kind='party_start' message; advanced in place by the WS-D advancer on each round's run-settle.
--
--    Budgets (max_rounds / max_voices_per_round / paid_run_budget) are stored on the row, defaulted
--    from config at create, and are the ONLY loop bound inside a session (the hop-cap is the wrong
--    bound for a deliberate multi-round debate — ADR-0027 §4.4). The advancer refuses to mint once
--    paid_runs_used would exceed paid_run_budget and sets status='budget_exhausted' — a HARD ceiling,
--    not advisory; dropped/capped counts are logged non-silently (§5.1).
--
--    started_by / topic_message_id are SERVER-STAMPED (the opener principal + the opt-in message);
--    agents cannot open a session (humans / the opt-in affordance only — §5.2), which keeps the
--    anti-N² guarantee intact at the entry point.
--
--    The lifecycle column is `phase` (NOT `status`) — the 0031 decision_request discipline — so the
--    0004 fence contract test's forbidden-token list stays out of the discussion schema (§4.2).
CREATE TABLE discussion.party_session (
    id                   uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    thread_id            uuid        NOT NULL REFERENCES discussion.thread(id),
    project_id           text        NOT NULL,                 -- room key "namespace/name" (ISI-3982)
    team_id              uuid        NOT NULL,                 -- tenancy root (ADR-0027 §7.3.3)
    started_by           text        NOT NULL,                 -- SERVER-STAMPED opener principal (§5.2)
    topic_message_id     uuid        NOT NULL REFERENCES discussion.message(id),  -- the opt-in start message
    round                int         NOT NULL DEFAULT 0,       -- current round; advanced on run-settle
    max_rounds           int         NOT NULL,                 -- budget (default 3; §5.1)
    max_voices_per_round int         NOT NULL,                 -- budget (2–4, default 3; §5.1)
    paid_run_budget      int         NOT NULL,                 -- per-session paid-run ceiling (§5.1)
    paid_runs_used       int         NOT NULL DEFAULT 0,       -- running tally (facilitator + voices)
    phase                text        NOT NULL DEFAULT 'active' -- SESSION lifecycle, NOT work-item custody
        CHECK (phase IN ('active', 'closed', 'converged', 'budget_exhausted')),
    opened_at            timestamptz NOT NULL DEFAULT now(),
    closed_at            timestamptz     NULL,
    -- Budget sanity: a non-negative counter and positive, coherent caps (a 0-round/0-voice session
    -- would be a no-op debate; the paid-run ceiling must cover at least one facilitator mint).
    CONSTRAINT party_session_round_nonneg        CHECK (round >= 0),
    CONSTRAINT party_session_paid_runs_nonneg    CHECK (paid_runs_used >= 0),
    CONSTRAINT party_session_budgets_positive    CHECK (max_rounds > 0 AND max_voices_per_round > 0 AND paid_run_budget > 0)
);

-- At most ONE active session per thread: a second party_start while a debate is live is an idempotent
-- no-op that returns the existing session, never a second paid debate (§5.2 opt-in integrity). The
-- partial UNIQUE is the enforcement — StartPartySession INSERTs ON CONFLICT DO NOTHING against it.
CREATE UNIQUE INDEX uq_party_session_active_thread
    ON discussion.party_session (thread_id) WHERE phase = 'active';

-- Advancer lookup: the WS-D advancer sweeps active sessions and their round by thread. Partial on the
-- hot path (active sessions are a tiny, bounded set; closed sessions never match the reaper tick).
CREATE INDEX idx_party_session_active ON discussion.party_session (thread_id) WHERE phase = 'active';
