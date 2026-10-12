-- 0036_discussion_mention_dispatch_thread_index.sql — ISI-5670 (ISI-5662 WS2): index the per-discussion
-- in-flight-run count that the HARD concurrent-run backstop gates on.
--
-- The backstop (internal/mentiondispatch.DispatchMention) refuses to mint a new run when a discussion
-- thread already has MaxConcurrentRunsPerDiscussion (default 4) runs in flight. It counts them with
-- pgLedger.CountInFlight: discussion.mention_dispatch rows for the thread whose minted work item has no
-- settled a2a marker yet AND whose dispatch is younger than the staleness window —
--
--   WHERE md.thread_id = $1 AND md.created_at >= now() - <staleness>
--
-- mention_dispatch grows across ALL discussions over time, and this count runs on the HOT dispatch path
-- (once per minted run, including every voice of every party round), so the thread_id filter must not
-- seq-scan the whole ledger. 0027 indexed only (work_item_id) — the reply-hop recovery path — so this
-- adds the composite (thread_id, created_at) the count needs: thread_id narrows to the discussion and
-- created_at serves the staleness lower-bound within it.
--
-- ────────────────────────────────────────────────────────────────────────────────────────────────
-- FENCE (ADR-0019 / 0004 header, reasoned in ADR-0027 §4.2): index-only, no column added or changed.
-- discussion.mention_dispatch is facilitation/dispatch bookkeeping, never work-item custody; the 0004
-- fence contract test's forbidden-token list keys on COLUMN names, which this migration does not touch.
--
-- Forward-only, additive; applied once by the apiserver migration runner in filename order.

CREATE INDEX IF NOT EXISTS idx_mention_dispatch_thread_created
    ON discussion.mention_dispatch (thread_id, created_at);
