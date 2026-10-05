-- 0030_scm_repo_staleness.sql — durable mirror staleness metadata on scm.repo
-- (ISI-5483, scope item 4 of ISI-5480).
--
-- Forward-only, versioned migration (same discipline as 0001..0029: applied exactly once, in
-- filename order, by the apiserver migration runner / db.dbmigrate.Apply on startup; there is NO
-- down migration — a mistake is corrected by a new forward file).
--
-- WHY: scm.repo (0008) is the durable anchor the reposync reconciler upserts after every completed
-- mirror pass — one row per mirrored (Project, repo) with `last_mirror_update` as the pass's freshness
-- observation. But the GitHub-status tab derives its "synced N ago / health" ENTIRELY from the live
-- Project CR status.sync (the informer cache): see internal/apiserver/githubstatus.go. When the
-- operator is down, restarting, or the CR has aged out of the cache, the tab shows nothing durable —
-- even though scm.repo still holds the last-known freshness on disk. This migration makes scm.repo
-- carry enough to render staleness UNIFORMLY from the DURABLE mirror, independent of the live
-- condition:
--
--   - sync_health  — the last-known health class the reconciler stamped on a completed pass. Today
--     only 'healthy' is written on the success anchor (UpsertRepo runs only after ApplySnapshot
--     succeeds); the enum leaves room for 'degraded'/'stale'/'error' so a later failure-path or
--     bridge-import stamp can widen WITHOUT another migration. 'unknown' is the honest pre-sync
--     default for a row that predates this column / has never completed a pass.
--   - ttl_seconds  — the expected refresh cadence in seconds (the Project's effective poll interval).
--     A reader derives staleness by comparing now() - last_mirror_update against this TTL: a durable
--     'healthy' row whose age exceeds its TTL is surfaced as STALE even if no live condition exists.
--     0 = unknown/no cadence (never treat as stale on a zero TTL).
--
-- last_mirror_update (0008) IS the "last_synced" timestamp — this migration deliberately does NOT add
-- a second redundant timestamp column; it reuses the one the anchor already stamps.
--
-- SHAPE — CHECK-constrained text + integer, matching existing conventions (0020 work_item fields):
-- sync_health is nullable-free with a DEFAULT + CHECK enum (the `state`-column style, not a Postgres
-- ENUM type — the enum set evolves with ALTER … DROP/ADD CONSTRAINT rather than the heavier ALTER
-- TYPE dance). ttl_seconds is a plain integer with a non-negative CHECK.
--
-- FR no-fabrication / backfill-safe: additive, forward-only, backfill-free. Every EXISTING scm.repo
-- row reads back sync_health='unknown', ttl_seconds=0 — an honest "we don't know yet", never a
-- made-up health — until the next level-triggered pass re-anchors and stamps the real values. No
-- field ownership changes: scm.repo remains written solely by the coordination/sync path and
-- expresses no coordination custody (0008 discipline).

CREATE SCHEMA IF NOT EXISTS scm;

ALTER TABLE scm.repo
    ADD COLUMN IF NOT EXISTS sync_health varchar(20) NOT NULL DEFAULT 'unknown',
    ADD COLUMN IF NOT EXISTS ttl_seconds integer     NOT NULL DEFAULT 0;

-- sync_health enum. 'healthy' is the only value the success anchor writes today; 'degraded'/'stale'/
-- 'error' are reserved for a later failure-path / import stamp; 'unknown' is the pre-sync default.
-- Widening the set is a forward migration (ALTER … DROP/ADD CONSTRAINT), never a silent widening.
ALTER TABLE scm.repo
    DROP CONSTRAINT IF EXISTS repo_sync_health_chk;
ALTER TABLE scm.repo
    ADD CONSTRAINT repo_sync_health_chk
        CHECK (sync_health IN ('healthy', 'degraded', 'stale', 'error', 'unknown'));

-- ttl_seconds is a cadence in seconds: non-negative, 0 = unknown/no cadence.
ALTER TABLE scm.repo
    DROP CONSTRAINT IF EXISTS repo_ttl_seconds_chk;
ALTER TABLE scm.repo
    ADD CONSTRAINT repo_ttl_seconds_chk
        CHECK (ttl_seconds >= 0);
