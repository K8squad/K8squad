-- 0034_discussion_party_round_facilitator.sql — ISI-5615 (ISI-5569 WS-D.1, ruling ADR-0027 §3.2/§3.3):
-- the per-round facilitator-message link the event-driven advancer needs to be restart-safe.
--
-- WS-B (0033) gave party mode a durable per-thread session row (round counter + budgets). WS-D's pure
-- decision + settle-completeness layer (internal/discussion/facilitator.go: SelectVoices / DecideAdvance
-- / RoundVoiceSettlement) landed next. This migration adds the one piece of durable state the live,
-- level-triggered advancer (ADR-0027 §3.2) needs so it holds NO in-memory round state (§3.1 — the whole
-- reason the long-lived facilitator run was rejected): a link from the session's CURRENT round to the
-- facilitator message that dispatched that round's voices.
--
-- Why it is needed (ADR-0027 §3.3 / §4.3): a round's dispatched voices are the discussion.mention_dispatch
-- rows keyed under the round's FACILITATOR message (the coordinator's @-mention post for that round). The
-- advancer reads round-N completeness via RoundVoiceSettlement(facilitator_message_id) — so it must know,
-- durably, which message is the current round's facilitator message. The facilitator run posts that message
-- asynchronously AFTER the advancer mints it, so the id cannot be known at mint time; the mint-gate dispatch
-- hook stamps it here when the facilitator's round post commits. On an operator restart the advancer resumes
-- the loop purely from this row + the durable ADR-0020 settle markers, exactly like the ADR-0020 reaper.
--
-- Two additive, forward-only, nullable columns (no backfill — pre-existing rows are either closed or were
-- never advanced by the live loop; NULL is the honest "no facilitator message for the current round yet"):
--
--   (1) current_round_message_id — the facilitator @-mention message for the round in flight. NULL between
--       minting a round's facilitator and that facilitator posting its dispatch (the advancer waits on NULL).
--   (2) round_started_at — when the current round's facilitator was minted, so the advancer can bound the
--       one residual liveness gap (a facilitator run that dies before posting its dispatch) with a timeout,
--       the same shape as the §3.3 settle_timeout stuck-voice guard.
--
-- ────────────────────────────────────────────────────────────────────────────────────────────────
-- FENCE (ADR-0019 / 0004 header, reasoned in ADR-0027 §4.2): both columns are facilitation-session
-- bookkeeping, NOT work-item custody. current_round_message_id REFERENCES discussion.message (a room
-- message, append-only); round_started_at is a timestamp. Neither is a claim/lease/fence_token/holder/
-- assignee/state/status/custody column — the 0004 fence contract test's forbidden-token list is untouched
-- (exact column-name match; neither name is on it). No work-item custody is expressed here; every
-- facilitator/voice run is still minted + fenced only in coord. FR-B3 exempts the whole discussion schema.
--
-- Forward-only, additive; applied once by the apiserver migration runner in filename order.

ALTER TABLE discussion.party_session
    ADD COLUMN current_round_message_id uuid NULL REFERENCES discussion.message(id),
    ADD COLUMN round_started_at         timestamptz NULL;
