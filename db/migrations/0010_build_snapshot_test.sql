-- 0010_build_snapshot_test.sql — runnable self-check for the build-snapshot
-- meta column (Story 8.7c, ISI-2903).
--
-- Same discipline as the 0001/0005/0007/0008 self-checks: plain SQL, no
-- framework, run after the migrations it checks, inside one transaction ROLLED
-- BACK at the end so it leaves no residue:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 \
--          -f db/migrations/0001_coord_schema.sql \
--          -f db/migrations/0010_build_snapshot.sql \
--          -f db/migrations/0010_build_snapshot_test.sql
--
-- Proves the *structural* AC: the meta column exists on coord.artifact, is
-- jsonb NOT NULL with a '{}' default (so existing INSERTs keep working), and a
-- build-snapshot row round-trips its summary object.

BEGIN;

-- (1) The column exists on coord.artifact, is jsonb, NOT NULL, defaults to '{}'.
DO $$
DECLARE
    t   text;
    nn  text;
    def text;
BEGIN
    SELECT data_type, is_nullable, column_default
      INTO t, nn, def
      FROM information_schema.columns
     WHERE table_schema = 'coord'
       AND table_name   = 'artifact'
       AND column_name  = 'meta';
    IF NOT FOUND THEN
        RAISE EXCEPTION 'coord.artifact.meta column is missing';
    END IF;
    IF t <> 'jsonb' THEN
        RAISE EXCEPTION 'coord.artifact.meta is %, expected jsonb', t;
    END IF;
    IF nn <> 'NO' THEN
        RAISE EXCEPTION 'coord.artifact.meta must be NOT NULL, is_nullable=%', nn;
    END IF;
    IF def IS NULL OR def NOT LIKE '%''{}''%' THEN
        RAISE EXCEPTION 'coord.artifact.meta default is %, expected ''{}''::jsonb', def;
    END IF;
END $$;

-- (2) A pre-0010 INSERT (no meta) still works — the DEFAULT keeps it '{}'.
DO $$
DECLARE
    wid uuid;
    got jsonb;
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by)
         VALUES (gen_random_uuid(), 't-0010', 'tester')
      RETURNING id INTO wid;

    INSERT INTO coord.artifact (work_item_id, run_id, kind, uri, sha256)
         VALUES (wid, gen_random_uuid(), 'patch', 'sha256:deadbeef', 'deadbeef');

    SELECT meta INTO got
      FROM coord.artifact
     WHERE work_item_id = wid AND kind = 'patch';
    IF got IS DISTINCT FROM '{}'::jsonb THEN
        RAISE EXCEPTION 'legacy INSERT left meta = %, expected {}', got;
    END IF;
END $$;

-- (3) A build-snapshot row round-trips its 8.7c summary object.
DO $$
DECLARE
    wid uuid;
    got jsonb;
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by)
         VALUES (gen_random_uuid(), 't-0010-snap', 'tester')
      RETURNING id INTO wid;

    INSERT INTO coord.artifact (work_item_id, run_id, kind, uri, sha256, meta)
         VALUES (wid, gen_random_uuid(), 'build-snapshot', 'sha256:cafe', 'cafe',
                 '{"base":"aaaa","runRef":"bbbb","commit":"bbbb","fileCount":3,
                   "totalAdditions":10,"totalDeletions":2,"truncated":false}'::jsonb);

    SELECT meta INTO got
      FROM coord.artifact
     WHERE work_item_id = wid AND kind = 'build-snapshot';
    IF (got->>'fileCount')::int <> 3 THEN
        RAISE EXCEPTION 'build-snapshot meta.fileCount = %, expected 3', got->>'fileCount';
    END IF;
    IF (got->>'runRef') <> 'bbbb' THEN
        RAISE EXCEPTION 'build-snapshot meta.runRef = %, expected bbbb', got->>'runRef';
    END IF;
END $$;

-- (4) Re-applying the migration is a no-op (IF NOT EXISTS discipline).
ALTER TABLE coord.artifact
    ADD COLUMN IF NOT EXISTS meta jsonb NOT NULL DEFAULT '{}'::jsonb;

ROLLBACK;
