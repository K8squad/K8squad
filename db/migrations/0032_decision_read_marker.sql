-- 0032_decision_read_marker.sql — ISI-5535 (E1 of ISI-5531, ADR-0026 §6): the per-user read-marker
-- store for the "Needs Human Decision" Inbox.
--
-- The Inbox aggregates, per human, every open proposal + in_review work item (+ decision_request in
-- E2) across projects. A row is UNREAD when the human has not seen it since its last run — grep
-- confirmed no notifications/read-marker subsystem exists to reuse (ADR-0026 D5), so this is the
-- minimum: one tiny keyed table, NOT a notifications framework.
--
-- Named `decision_read_marker` (NOT a chat-shaped name): FR-B3 (pkg/coord frb3_no_chat_contract_test)
-- rejects any non-discussion-schema table whose bare name reads as an agent-to-agent channel. This is
-- a HUMAN-facing read-state marker, so it carries a neutral custody-free name and its store lives in
-- the apiserver (not pkg/coord's custody surface).
--
--   * item_key is the ADR-0026 §3.3 union-member key — `inReview:{workItemId}` /
--     `proposal:{messageId}` / `decision:{messageId}` — a STABLE id per logical decision, so a
--     marker survives agent retries (one logical decision = one key = one marker).
--   * user_principal is the authenticated identity (discussion.AuthorContext.Principal), the same
--     server-stamped principal the discussion/coord reads fence on. Markers are strictly per-user:
--     one human marking a shared in_review item seen never clears it for a teammate.
--   * seen_at is UPSERTed to now() by POST /api/squad/inbox/seen. UNREAD is DERIVED in the handler
--     (ADR-0026 §3.3/§6): the item has no marker, OR seen_at < the item's orderKey (a NEW run on an
--     already-seen item re-surfaces it — the "needs me again" signal). Nothing about unread is
--     stored here beyond the last-seen instant.
--
-- This is a DECISION/attention record, not a custody record — it holds no claim/lease/fence_token,
-- names no custodian, moves no work-item custody. Forward-only, additive; no backfill (absent marker
-- ⇒ unread, the correct first-run default).

CREATE TABLE coord.decision_read_marker (
    user_principal text        NOT NULL,                 -- WHO — the authenticated principal (per-user)
    item_key       text        NOT NULL,                 -- ADR-0026 §3.3 union-member key (inReview:/proposal:/decision:)
    seen_at        timestamptz NOT NULL DEFAULT now(),   -- last time this user marked this item seen
    PRIMARY KEY (user_principal, item_key)
);

-- The badge/read derivation reads every marker for one user in a single scan; the PK's leading
-- column already serves that prefix lookup, so no extra index is needed.
