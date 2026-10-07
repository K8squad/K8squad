-- 0031_discussion_decision_request.sql — ISI-5536 (ISI-5531 E2): the decision_request interaction.
--
-- ADR-0026 §4 (feature ISI-5531). A decision_request is the net-new "an agent suggests structured
-- options a human answers" surface. It copies the proposal pattern (0025) wholesale: an inert,
-- append-only discussion.message carries the question; a lifecycle SIDE TABLE carries the mutable
-- decision state the append-only message row cannot. Two additive changes:
--
--   1. `kind='decision_request'` joins the allowed message kinds (0025's CHECK is widened).
--   2. discussion.decision_request — the decision-lifecycle side table for the state machine
--      `open → answered | rejected | expired | superseded`.
--
-- FENCE CARVE-OUT (ADR-0019 / R13, deliberate — identical to 0025): discussion.message stays
-- custody-free by construction (its 0004 append-only trigger permits only the invalidated_at
-- soft-retract), so the decision lifecycle CANNOT live in the message row. This side table is a
-- DECISION record, not a custody record: it has no claim/lease/fence_token/holder/assignee column,
-- names no custodian, and cannot transfer custody. Answering a decision_request fans out ONLY
-- through the existing human-gated dispatch seam (WorkItemDispatcher.RequestDispatch, ADR-0026 §5);
-- custody of a work item still moves ONLY in the fenced coord claim tables. The column is named
-- `phase` (not `state`) precisely so no custody token from the fence's forbidden list enters the
-- discussion schema.
--
-- Forward-only, additive; applied once by the apiserver migration runner in filename order.

-- 1. Widen the kind CHECK (0025) so a decision_request message is representable.
ALTER TABLE discussion.message DROP CONSTRAINT kind_must_be_text_or_extension;
ALTER TABLE discussion.message
    ADD CONSTRAINT kind_must_be_text_or_extension CHECK (
        kind = 'text' OR kind IN ('structured', 'task', 'decision', 'vote', 'proposal', 'decision_request')
    );

-- 2. Decision-lifecycle side table. One row per kind='decision_request' message, inserted at post
--    time (phase='open') and advanced only by the answer/reject/expire shells. PK = the request
--    message, so a message has at most one lifecycle. The join through message→thread→(project_id,
--    team_id) is the tenancy predicate on every read/write of this table.
--
--    idempotency_key (ADR-0026 §4.1): "decision:{ticketId}:{slug}:{runId}" — the UNIQUE guarantees
--    one card per logical ask, so an agent retry re-posting the same request returns the existing
--    card (idempotent create) rather than minting a duplicate row.
--
--    bound_revision_id (ADR-0026 §4.2/§5): the target.revisionId the card is pinned to, lifted into
--    its own column so the supersede CAS ("auto-expire when the bound revision moves") is a cheap
--    indexed predicate, not a jsonb extraction. The full target{type,ref,revisionId} is kept in
--    `target` jsonb for the card renderer.
--    work_item_id (ADR-0026 §3.3/§5): the coord work item the card decides on, carried so (a) the
--    Inbox union keys each decision by its ticket to run-join for ordering (BE-7) and (b) the answer
--    shell re-dispatches that ticket to resume the raising agent (BE-6). It is a PLAIN uuid with NO
--    cross-schema FK into coord.work_item on purpose: the fence (0004/ADR-0019) keeps the discussion
--    schema from referencing — and thus coupling custody to — the coordination tables. NULL is legal
--    (a project-wide ask bound to no ticket): such a card simply has no continuation target.
CREATE TABLE discussion.decision_request (
    message_id        uuid        PRIMARY KEY REFERENCES discussion.message(id),
    team_id           uuid        NOT NULL,   -- the owning thread's team, denormalized ONLY to scope
                                              -- the idempotency UNIQUE to the tenant (see below)
    phase             text        NOT NULL DEFAULT 'open'
        CHECK (phase IN ('open', 'answered', 'rejected', 'expired', 'superseded')),
    idempotency_key   text        NOT NULL,
    work_item_id      uuid            NULL,   -- coord ticket the card decides on (NO FK — fence, ADR-0019)
    target            jsonb           NULL,   -- {type,ref,revisionId} the card binds to (ADR §4.1)
    bound_revision_id text            NULL,   -- target.revisionId lifted out for the supersede CAS
    continuation      text        NOT NULL DEFAULT 'resume_agent_on_answer',
    answer            jsonb           NULL,   -- typed answer (ADR §4.3): selectedOptionIds/freeText/…
    reject_reason     text            NULL,   -- required when rejectRequiresReason (handler-enforced)
    answered_by       text            NULL,   -- principal of the answering/rejecting human (SERVER-STAMPED)
    answered_at       timestamptz     NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

-- One card per logical ask, SCOPED TO THE TENANT: an agent retry re-posting the same idempotency_key
-- returns the existing row instead of a second card (ADR-0026 §4.2 idempotent create). The uniqueness
-- is (team_id, idempotency_key), NOT idempotency_key alone: a GLOBAL unique would let one team's key
-- block another team's create (the insert conflicts, but the tenancy-scoped idempotent re-read cannot
-- see the other team's row → a spurious 404/create-block across tenants). Scoping to team_id matches
-- the re-read's tenancy predicate so the conflict path always resolves to the caller's own card.
CREATE UNIQUE INDEX idx_decision_request_idempotency
    ON discussion.decision_request (team_id, idempotency_key);

-- Answer/reject CAS scans and the Inbox "open decision_requests" arm (ADR-0026 §3.2) both live in
-- phase='open'; decided lookups ride the PK.
CREATE INDEX idx_decision_request_open ON discussion.decision_request (phase) WHERE phase = 'open';
