-- 0019_a2a_settlement.sql — the durable a2a follow-settlement marker
-- (ADR-0020 §2.1, ISI-4348-S1). Forward-only companion to 0005_reconcile_step.sql
-- (which created coord.a2a_dispatch).
--
-- WHY (ADR-0020 §1.2): coord.a2a_dispatch records only that a task was
-- *submitted* (dispatched_at), never that its follow *settled*. The follow's
-- OnDone release (ISI-4346) is in-memory and process-local: an operator restart
-- mid-run kills the follow goroutine, OnDone never fires, and the run-owned
-- sandbox pod (an HTTP supervisor with no ownerReferences, holding a full CPU
-- request) leaks forever. A durable marker lets a post-restart reaper (S2) tell
-- "agent finished before the crash" (reap) from "agent still working" (keep).
--
-- DECISION (§2.1): add settlement columns to the existing per-lap dispatch row
-- rather than a new coord.a2a_settlement table. The row is already keyed by
-- a2a_task_id (PK, per §8 retry lap) and already references work_item_id/run_id,
-- so settlement is 1:1 with a dispatch lap and the at-most-once guard is a single
-- indexed conditional UPDATE (... WHERE a2a_task_id=$1 AND settled_at IS NULL) —
-- no join, no second write. The audit trail rides the existing coord.audit_log
-- ('a2a_settled'), not a settlement table.
--
-- §6.4 CHECK-enum note (§2.1): settlement is NOT a reconcile step and this
-- migration deliberately does NOT touch 0005/0017's reconcile_step CHECK enum.
-- The step is already succeeded/failed when settlement lands; adding a step would
-- fork the happy path and break the at-most-once machine's falsification proof.
--
-- Renumbered from the ADR's working name 0017: 0017 (reconcile_cancel_step) and
-- 0018 (claim_assignee) were already taken when this landed — same forward-only
-- renumber discipline 0017 itself used (it was authored as 0016 on a side branch).
--
-- Forward-only: both columns are added NULL (settled_at IS NULL = "follow not yet
-- observed to complete"), so no existing coord.a2a_dispatch row can violate the
-- new CHECK, and the partial index only covers already-settled rows.
ALTER TABLE coord.a2a_dispatch
    ADD COLUMN settled_at     timestamptz,          -- NULL = follow not yet observed to complete
    ADD COLUMN settle_outcome text                  -- 'succeeded' | 'failed' | 'follow_error'
        CHECK (settle_outcome IN ('succeeded', 'failed', 'follow_error'));

-- Reaper lookup (S2): settled dispatches whose run-owned pods may be reapable.
-- Partial on `settled_at IS NOT NULL` so it indexes only the settled minority the
-- reaper scans by run_id, and stays out of the hot un-settled dispatch path.
CREATE INDEX idx_a2a_dispatch_settled
    ON coord.a2a_dispatch (run_id) WHERE settled_at IS NOT NULL;
