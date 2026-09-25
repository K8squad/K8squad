-- 0025_discussion_proposal.sql — ISI-4928: action-proposal messages + decision-lifecycle side table
--
-- Plan ISI-4919 §4.4/§6 (board-approved OQ1: propose→authorize happens in the room, superseding
-- v1 AC5). Two additive changes:
--
--   1. `kind='proposal'` joins the allowed message kinds (0024's CHECK is widened).
--   2. discussion.proposal — the decision-lifecycle side table for the proposal state machine
--      `proposed → confirmed | dismissed | executed`.
--
-- FENCE CARVE-OUT (ADR-0019 / R13, deliberate): discussion.message stays custody-free by
-- construction — its append-only trigger (0004) still permits ONLY the invalidated_at soft-retract,
-- so the proposal lifecycle CANNOT live in the message row. This side table is a DECISION record,
-- not a custody record: it has no claim/lease/fence_token/holder/assignee column, it names no
-- custodian, and it cannot transfer custody. Execution still happens ONLY through the existing
-- human-gated authoring APIs (POST /api/projects/{pid}/work-items, POST /api/work-items/{id}/dispatch);
-- custody of a work item still moves ONLY in the fenced coord claim tables (§6.2/§6.3). The column
-- is named `phase` (not `state`) precisely so no custody token from the fence's forbidden list
-- enters the discussion schema.
--
-- Forward-only, additive; applied once by the apiserver migration runner in filename order.

-- 1. Widen the kind CHECK (0024) so a proposal message is representable.
ALTER TABLE discussion.message DROP CONSTRAINT kind_must_be_text_or_extension;
ALTER TABLE discussion.message
    ADD CONSTRAINT kind_must_be_text_or_extension CHECK (
        kind = 'text' OR kind IN ('structured', 'task', 'decision', 'vote', 'proposal')
    );

-- 2. Decision-lifecycle side table. One row per kind='proposal' message, inserted at post time
--    (phase='proposed') and advanced only by the confirm/dismiss shells. PK = the proposal message,
--    so a message has at most one lifecycle. The join through message→thread→(project_id, team_id)
--    is the tenancy predicate on every read/write of this table.
CREATE TABLE discussion.proposal (
    message_id  uuid        PRIMARY KEY REFERENCES discussion.message(id),
    phase       text        NOT NULL DEFAULT 'proposed'
        CHECK (phase IN ('proposed', 'confirmed', 'dismissed', 'executed')),
    decided_by  text            NULL,   -- principal of the confirming/dismissing human (SERVER-STAMPED)
    decided_at  timestamptz     NULL,
    result      jsonb           NULL,   -- fan-out outcome (work item id, dispatch states, run-chip data)
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- Confirm/dismiss CAS scans live in phase='proposed'; executed lookups ride the PK.
CREATE INDEX idx_proposal_phase ON discussion.proposal (phase) WHERE phase = 'proposed';
