-- 0035_discussion_party_round.sql — ISI-5616 (ISI-5613 Gap 1, ADR-0027 §3.3 / addendum): the durable
-- per-round facilitator-message ledger that lets GetThread surface a stable round number + session id on
-- each party-mode discussion Message.
--
-- WHY THIS EXISTS (what 0034 does NOT give us):
--
-- 0034 (ISI-5615 WS-D.1) added party_session.current_round_message_id — the facilitator @-mention message
-- for the round IN FLIGHT. That column is the advancer's live cursor: MintRound CLEARS it each round and
-- SetRoundFacilitatorMessage re-stamps it, so it only ever holds the CURRENT round's facilitator message.
-- There is no durable history mapping each past round's facilitator message → its round number. So the
-- console (ISI-5613/#827) cannot group a thread's historical voice messages into numbered rounds from the
-- session row alone.
--
-- Re-deriving round numbers at READ time by ordering discussion.mention_dispatch rows would be a heuristic
-- that drifts: a human @-mention posted into the thread mid-debate also lands in mention_dispatch, inflating
-- the ordinal. The faithful fix is to record the (session, round) → facilitator_message link DURABLY at the
-- one seam that knows all three — SetRoundFacilitatorMessage, when the facilitator's round post wins its CAS
-- (party.go). This ledger is that record; GetThread reads it (ISI-5616 read wire).
--
-- A party voice's reply message carries author_run_id; its round is recovered by the SAME join the reply-hop
-- resolver / ThreadForDispatchedRun already use —
--   voice.author_run_id = coord.claim.run_id → work_item_id = mention_dispatch.work_item_id
--     → mention_dispatch.message_id = party_round.facilitator_message_id → party_round.(round, kind).
-- The facilitator's own round message is party_round.facilitator_message_id directly; the party_start opener
-- is party_session.topic_message_id (round 0). See ADR-0027 addendum (NAS) for the full read contract.
--
-- ────────────────────────────────────────────────────────────────────────────────────────────────
-- FENCE (ADR-0019 / 0004 header, reasoned ADR-0027 §4.2 — same carve-out as 0033/0034): party_round is
-- FACILITATION bookkeeping, NOT work-item custody. It carries NO claim/lease/fence_token/holder/assignee/
-- state/status/custody column — facilitator_message_id is a plain reference to an append-only room message,
-- and `kind` is a round-type label ('round'|'takeaways'), never a work-item custody-state. Every
-- facilitator/voice run is still minted + fenced only in coord. FR-B3 exempts the whole discussion schema
-- (precedent: 0027 mention_dispatch, 0033 party_session, 0034 round-facilitator columns).
--
-- Forward-only, additive; applied once by the apiserver migration runner (db/dbmigrate.go) in filename
-- order. Reads/writes that never mention party_round are unaffected.

CREATE TABLE discussion.party_round (
    session_id               uuid        NOT NULL REFERENCES discussion.party_session(id),
    round                    int         NOT NULL,                 -- 1-based; mirrors party_session.round at the mint (the round the facilitator dispatched)
    facilitator_message_id   uuid        NOT NULL REFERENCES discussion.message(id), -- the facilitator @-mention post that dispatched this round's voices
    kind                     text        NOT NULL DEFAULT 'round'  -- 'round' = a voice round; 'takeaways' reserved for a future wrap-up link
        CHECK (kind IN ('round', 'takeaways')),
    created_at               timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT party_round_round_positive CHECK (round > 0),
    -- One facilitator message per (session, round): the ledger INSERT rides SetRoundFacilitatorMessage's
    -- first-writer-wins CAS, and this PK is the belt-and-suspenders second guard (a replayed/concurrent
    -- stamp ON CONFLICT DO NOTHING no-ops here).
    PRIMARY KEY (session_id, round)
);

-- The voice→round read join keys on the facilitator message id (= mention_dispatch.message_id). Index it.
CREATE INDEX idx_party_round_facilitator_message ON discussion.party_round (facilitator_message_id);
