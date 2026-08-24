-- 0008_artifact_run_index.sql — serving index for the 8.3 artifact browser
-- (ISI-2900, cursor review on PR #88).
--
-- Forward-only companion to 0001_coord_schema.sql. coord.artifact carries a PK on
-- id and the UNIQUE(work_item_id, run_id, kind) re-entry upsert key, but NO index
-- with run_id as the leading column — so the artifact browser's per-Run reads
-- (ProdStore.ListByRun: WHERE run_id = $1 ORDER BY created_at, id) sequential-scan
-- on every list request. This adds the one index that matches both the filter and
-- the deterministic ORDER BY exactly.
--
-- Non-exclusive: creating an index CONCURRENTLY cannot run inside the transaction
-- the migration runner wraps each file in, and this lands before any deployment
-- has meaningful artifact volume (the read model ships in this same PR), so a
-- plain CREATE INDEX IF NOT EXISTS is the right trade.

CREATE INDEX IF NOT EXISTS idx_artifact_run_created
    ON coord.artifact (run_id, created_at, id);
