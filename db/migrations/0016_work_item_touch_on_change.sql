-- 0016_work_item_touch_on_change.sql — the work-item revision pin must survive
-- no-op custody writes (ISI-4217).
--
-- Root cause this corrects (forward-only per the db/migrations README: 0001 is
-- shipped, so the fix is a new migration, never an edit): the ISI-4183 re-acquire
-- mark
--
--     UPDATE coord.work_item SET state = $1
--      WHERE id = $2::uuid AND state IN ($3, $1)      -- pkg/coord/prodclaim.go
--
-- deliberately matches an ALREADY in_progress row (a §8 retry lap re-enters
-- claiming_sandbox after RetryEnter released the checkout; a lapsed-lease
-- takeover re-claims an in_progress item). That write is a NO-OP VALUE change —
-- the lane does not move — but 0001's work_item_touch_updated_at BEFORE UPDATE
-- trigger bumps updated_at unconditionally, minting a PHANTOM REVISION.
--
-- Live wedge (k8squad-test, item 16d0c223, PR #399 / main 3199117): the lap-2
-- dispatch re-enters with the revision token pinned by lap 1's dispatch build
-- (pkg/controller/contextsource/source.go — the exact-match deterministic-resume
-- check) and refuses to fall back to latest, so every drive pass errors in a
-- backoff loop while the HeartbeatSweeper keeps renewing the held lease: no
-- fail-over, no release, item held in_progress forever.
--
-- Contract correction: updated_at — the WorkItemRevision pin source (ADR-001
-- opaque token, contextsource.Source.WorkItem) — moves ONLY when the row
-- actually changes. A custody write that changes nothing mints no revision; a
-- real edit (title/body/state-lane-move/acceptance_criteria/goals, 0014) still
-- bumps. Deterministic resume keeps its teeth for genuine edits and stops
-- failing on custody bookkeeping.
--
-- Deliberate locus choice (ISI-4217 "pick one deliberately"):
--   - NOT RetryEnter refreshing/invalidating the durable pin: the pin lives on
--     the Run CR (status.contextSnapshot), so clearing it AFTER the RetryEnter
--     commit is a second, non-atomic write — a crash between the two re-wedges
--     identically — and it leaves the lapsed-lease takeover shape (same no-op
--     mark, no lap involved) broken.
--   - NOT a lap-boundary-annotated re-pin in contextsource: plumbs lap state
--     into the opaque revision token, growing its surface for no determinism
--     gain — the phantom revision simply should not exist.
--   - THIS: suppress the bump when the row is unchanged. Crash-safe (the
--     phantom revision never exists, no window), fixes the whole class (every
--     current or future no-op writer), zero application-SQL changes.
--
-- Blast radius: coord.touch_updated_at() is shared by 0009's
-- rate_limit_reroute_touch_updated_at trigger. Suppression only affects no-op
-- rewrites there; a real escalation episode rewrite (attempt/resume_at/released_
-- fence move) still bumps — the same corrected semantics, applied consistently.

CREATE OR REPLACE FUNCTION coord.touch_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW IS DISTINCT FROM OLD THEN
        NEW.updated_at := now();
    ELSE
        NEW.updated_at := OLD.updated_at;
    END IF;
    RETURN NEW;
END;
$$;
