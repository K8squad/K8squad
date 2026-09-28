-- 0027_dispatch_on_mention_run_minting.sql — ISI-5116 (parent ISI-5108, ruling ADR-0024b / ISI-5117):
-- the run-minting + reply-plumbing half of dispatch-on-mention.
--
-- The trigger half (ISI-5108, PR #675) parses @-mentions in a room post and emits a MentionDispatch
-- per matched agent through the MentionDispatcher seam. This migration adds the two durable pieces the
-- apiserver-side dispatcher needs to turn each MentionDispatch into a real agent Run:
--
--   (1) coord.work_item.source  — the board-hide discriminator (ADR-0024b §4.2).
--   (2) discussion.mention_dispatch — the (message_id, agent_name) idempotency ledger (§4.5).
--
-- Both are additive, forward-only, backfill-safe. Reads/writes that never mention them are unaffected.
--
-- ────────────────────────────────────────────────────────────────────────────────────────────────
-- (1) coord.work_item.source — board-hidden thread-run marker (ADR-0024b §4)
-- ────────────────────────────────────────────────────────────────────────────────────────────────
-- A2 (the Architect's ruling) mints the mentioned agent's drive item through the SAME
-- coord.CreateWorkItem + coord.RequestDispatch seams the human board uses — a server-side, trusted
-- "discussion dispatch engine" principal, a peer of Intake. To keep "create-ticket stays human-gated"
-- coherent, those per-mention items MUST NOT appear on any human board surface (they scale with chat
-- volume and have no parent). Board hiding is therefore a filter concern on a discriminator column.
--
-- The column defaults to 'board', so EVERY existing INSERT path (the human create at
-- pkg/coord/workitemwrite.go, the agent-author path, review-automation, the dispatch chain) records
-- 'board' with NO code change — coord.CreateWorkItem/RequestDispatch stay reused verbatim (no new
-- exported pkg/coord symbol ⇒ FR-B3 allowlist untripped, ADR-0024b §4.3). The discussion dispatcher
-- flips its own minted item to 'discussion' via a direct UPDATE at the apiserver edge.
--
-- Filtered ALLOWLIST-style (WHERE source = 'board') on every human board-list surface so that already-
-- guarded queries stay correct as new sources appear (ADR-0024b §4.2). The authoritative fail-closed
-- guarantee is a contract test (mirror of frb3_no_chat_contract_test.go), not this column.
--
-- Intake (pkg/controller/rundrive/intake.go, WHERE state='todo') is deliberately NOT filtered on
-- source: it MUST mint the Run for a discussion-sourced item — that is the whole point of A2.

ALTER TABLE coord.work_item
    ADD COLUMN source text NOT NULL DEFAULT 'board';   -- 'board' = human/agent board authoring; 'discussion' = dispatch-on-mention thread-run

ALTER TABLE coord.work_item
    ADD CONSTRAINT work_item_source_chk
        CHECK (source IN ('board', 'discussion'));

-- Partial index on the board allowlist: the board-list queries all filter WHERE source='board', so
-- index the common case. Small and cheap; discussion items are excluded from the index entirely.
CREATE INDEX idx_work_item_board_source ON coord.work_item (project_id, updated_at DESC)
    WHERE source = 'board';

-- ────────────────────────────────────────────────────────────────────────────────────────────────
-- (2) discussion.mention_dispatch — the idempotency ledger (ADR-0024b §4.5)
-- ────────────────────────────────────────────────────────────────────────────────────────────────
-- The MentionDispatcher seam MUST be idempotent on (message_id, agent_name) so an at-least-once caller
-- never double-runs. coord.DispatchOnce is spine-fenced (it requires a live claim's fence token) and
-- is not reusable at the apiserver edge, so the ledger is a plain first-writer-wins marker keyed on the
-- pair: the dispatcher claims (INSERT … ON CONFLICT DO NOTHING) before minting, binds the created
-- work_item_id after CreateWorkItem, and releases the claim if minting fails so a retry can redo.
--
-- The ledger also carries hop_depth + work_item_id so the reply path can look up, from a run's identity
-- alone, the hop the run was dispatched at and stamp its post-back honestly (loop guard, §4.4) without
-- trusting the agent to echo the number. It lives in the `discussion` schema — the sanctioned, custody-
-- free Per-Project Discussion Room surface (FR-B3 exempts the whole schema; this is a dispatch ledger,
-- not an agent-to-agent message store: no body, no state, no custody column).

CREATE TABLE discussion.mention_dispatch (
    message_id   uuid        NOT NULL,                 -- the triggering discussion.message
    agent_name   text        NOT NULL,                 -- the mentioned agent (canonical roster casing)
    project_id   text        NOT NULL,                 -- room key — "namespace/name" slug (ISI-3982)
    thread_id    uuid        NOT NULL,                 -- the thread the dispatched run reads + replies into
    hop_depth    int         NOT NULL,                 -- the hop the dispatched run runs AT (human turn ⇒ 1)
    work_item_id uuid,                                  -- the minted drive item; NULL until CreateWorkItem binds it
    created_at   timestamptz NOT NULL DEFAULT now(),
    -- Idempotency: one dispatch per (message, agent). A concurrent at-least-once retry hits this PK and
    -- is a no-op (ON CONFLICT DO NOTHING), so the same @-mention never mints two Runs for one agent.
    PRIMARY KEY (message_id, agent_name)
);

-- The reply-path hop lookup joins from a run back to its drive item to its dispatch row:
--   coord.claim(run_id) → work_item_id → discussion.mention_dispatch(work_item_id).hop_depth
CREATE INDEX idx_mention_dispatch_work_item ON discussion.mention_dispatch (work_item_id)
    WHERE work_item_id IS NOT NULL;
