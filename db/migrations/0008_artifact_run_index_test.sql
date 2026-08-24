-- 0008_artifact_run_index_test.sql — runnable self-check for the artifact
-- browser's serving index (ISI-2900, cursor review on PR #88).
--
-- Same discipline as the 0001/0005/0007 self-checks: plain SQL, no framework,
-- run after the migrations it checks, inside one transaction ROLLED BACK at the
-- end so it leaves no residue:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 \
--          -f db/migrations/0001_coord_schema.sql \
--          -f db/migrations/0008_artifact_run_index.sql \
--          -f db/migrations/0008_artifact_run_index_test.sql
--
-- Proves the *structural* AC: the index exists, is on coord.artifact, and leads
-- with run_id (so the browser's WHERE run_id = $1 ORDER BY created_at, id rides
-- it instead of a sequential scan).

BEGIN;

-- (1) The index exists on coord.artifact.
DO $$
DECLARE
    n integer;
BEGIN
    SELECT COUNT(*)
      INTO n
      FROM pg_indexes
     WHERE schemaname = 'coord'
       AND tablename  = 'artifact'
       AND indexname  = 'idx_artifact_run_created';
    IF n <> 1 THEN
        RAISE EXCEPTION 'expected exactly one idx_artifact_run_created on coord.artifact, found %', n;
    END IF;
END $$;

-- (2) It leads with run_id and covers (created_at, id) — the browser's filter + ORDER BY.
DO $$
DECLARE
    cols text;
BEGIN
    SELECT string_agg(a.attname, ',' ORDER BY x.ord)
      INTO cols
      FROM pg_index i
      JOIN pg_class c ON c.oid = i.indexrelid
      JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS x(attnum, ord) ON true
      JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = x.attnum
     WHERE c.relname = 'idx_artifact_run_created';
    IF cols IS DISTINCT FROM 'run_id,created_at,id' THEN
        RAISE EXCEPTION 'idx_artifact_run_created columns are %, expected run_id,created_at,id', cols;
    END IF;
END $$;

-- (3) Re-applying the migration is a no-op (IF NOT EXISTS discipline).
CREATE INDEX IF NOT EXISTS idx_artifact_run_created
    ON coord.artifact (run_id, created_at, id);

ROLLBACK;
