-- 0042_comment_kind.sql — ISI-7094 (parent ISI-7088): two-phase agent comments.
--
-- Henrik (2026-11-05): agent comments on k8Squad tickets are unreadable — every run
-- sub-step (the operator-side ProgressMirror's `[run …][tool:…][status]` rows, plus the
-- per-run LLM digests) lands as a TOP-LEVEL coord.comment, burying the one human-readable
-- line. The firehose was never meant to be a coordination comment (ADR-040 keeps the
-- high-volume shim trace OFF the coord tables), but the M1.6 "progress visible" mirror
-- (ISI-4235) routed it onto coord.comment anyway. This migration adds the structural
-- distinction so the console can render a summary first-class and tuck progress away.
--
-- Two additive, forward-only, backfill-safe columns on coord.comment:
--
--   kind   — the two-phase class: 'progress' | 'summary' | 'human'.
--            * progress : run sub-steps (tool results, status transitions, LLM digests,
--                         narration). STORED but excluded from the default top-level
--                         comment query (readComments); surfaced only under the console's
--                         per-run "Activity (N events)" expander (ISI-7095).
--            * summary  : exactly ONE per completed run — the agent-authored structured
--                         recap, or the platform-synthesized minimal summary when the
--                         agent omits one (the invariant holds structurally, at settle,
--                         never on agent goodwill — see pkg/coord/settle.go).
--            * human    : a human-authored console comment (or any legacy/plain comment).
--                         The DEFAULT, so every pre-0042 row and every unlabelled write
--                         reads back as a first-class top-level comment exactly as before.
--
--   run_id — the Run a progress/summary row belongs to (NULL for a human comment and for
--            legacy progress rows we cannot re-key). Lets the console GROUP progress rows
--            per run under the Activity expander, and lets settle answer "did THIS run
--            already write a summary?" to enforce the exactly-one invariant.
--
-- Backfill (the ISI-7088 reclassification): existing comments whose body starts with the
-- ProgressMirror `[run` prefix are the flood — reclassify them to 'progress' so they drop
-- out of the default thread immediately. We cannot recover run_id from the body (the
-- prefix carries only the 8-char short id), so historical progress rows keep run_id NULL;
-- the console lumps NULL-run progress into a single legacy Activity group.
--
-- coord.comment is append-only (the comment_append_only BEFORE UPDATE trigger, 0001). A
-- one-time controlled reclassification is the sanctioned exception, so we DISABLE the
-- trigger for just the backfill UPDATE and re-ENABLE it in the same transaction — the
-- append-only guarantee is unchanged for every runtime write before and after.

ALTER TABLE coord.comment
    ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'human'
        CHECK (kind IN ('progress', 'summary', 'human')),
    ADD COLUMN IF NOT EXISTS run_id uuid;

-- One-time reclassification of the existing [run-prefixed flood → progress. The
-- append-only trigger is disabled ONLY for this statement, inside the migration txn.
ALTER TABLE coord.comment DISABLE TRIGGER comment_append_only;
UPDATE coord.comment
   SET kind = 'progress'
 WHERE kind = 'human'
   AND body LIKE '[run %';
ALTER TABLE coord.comment ENABLE TRIGGER comment_append_only;

-- Query-shape indexes for the two read paths the console (ISI-7095) and observability
-- (ISI-7096) split on. The original idx_comment_work_item (work_item_id, created_at)
-- stays for the broad read; these serve the kind-filtered top-level query and the
-- per-run Activity grouping.
CREATE INDEX IF NOT EXISTS idx_comment_kind
    ON coord.comment (work_item_id, kind, created_at);
CREATE INDEX IF NOT EXISTS idx_comment_run
    ON coord.comment (work_item_id, run_id, created_at)
 WHERE run_id IS NOT NULL;
