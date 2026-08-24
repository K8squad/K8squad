-- 0009_run_pause.sql — the PRODUCTION uuid-keyed pause-episode table for
-- Paused(rate_limited) auto-resume (Story 3.7 / ISI-2531, wired by ISI-2883).
--
-- Where 0001..0008 bind the reconcile machine's durable step, effects and
-- artifacts, this binds the §8 tier-2 SCHEDULED RESUME state: one row per work
-- item, rewritten in place (the claim-row discipline), carrying the single
-- durable wake time (resume_at) the resume timer sleeps toward — never a poll
-- loop (arch §8, story 2.11: "a single durable wake at the window reset").
--
--   * work_item_id uuid PK — exactly one live pause episode per work item; the
--     episode is REWRITTEN IN PLACE (a re-signalled still-pending pause refreshes
--     resume_at and keeps the attempt; a re-pause after a resume escalates or
--     resets the attempt — pkg/coord/resume.go planAttempt).
--   * run_id uuid — the Run the episode belongs to (audit/projection join key).
--   * reason — 'rate_limited' today; the same Paused machinery will carry
--     'credential' (7.4) — one family, distinct reasons (arch §8).
--   * retry_after_ms — the provider-supplied window when the shim's 5.10 signal
--     carried one (NULL ⇒ the equal-jitter exponential backoff was used).
--   * attempt — the escalation streak counter (bounded exponential input).
--   * resume_at — THE durable wake: set once at pause time; the timer re-derives
--     from it after a crash (crash-safe: never held only in memory).
--   * resumed_at — NULL while pending; stamped by the exactly-once ResumeDue
--     claim (FOR UPDATE SKIP LOCKED). The partial index covers exactly the
--     pending set the wake derivation reads.
--
-- This is the uuid-keyed production sibling of the int-keyed harness table
-- NewResumeForTest provisions (pkg/coord/resume.go) — same columns, same
-- discipline, real foreign keys. pkg/coord/resumeprod.go binds the statements.
--
-- Self-check: 0009_run_pause_test.sql (structural); behavioural gate:
-- TestProdResume (pkg/coord/resumeprod_chaos_test.go, -tags=chaos).

BEGIN;

CREATE TABLE coord.run_pause (
    work_item_id   uuid        PRIMARY KEY REFERENCES coord.work_item(id) ON DELETE CASCADE,
    run_id         uuid        NOT NULL,
    reason         text        NOT NULL,
    retry_after_ms bigint,
    attempt        int         NOT NULL,
    resume_at      timestamptz NOT NULL,
    paused_at      timestamptz NOT NULL,
    resumed_at     timestamptz
);

-- The pending-wake scan: NextWake reads the earliest pending resume_at, the
-- ResumeDue claim stamps resumed_at under SKIP LOCKED. Partial so the index
-- stays the size of the PAUSED set, not the all-time episode history.
CREATE INDEX idx_run_pause_pending ON coord.run_pause (resume_at)
    WHERE resumed_at IS NULL;

COMMIT;
