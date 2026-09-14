-- 0019_a2a_settlement_test.sql — runnable self-check for the durable a2a
-- follow-settlement marker (ADR-0020 §2.1, ISI-4348-S1).
--
-- Same discipline as the 0005/0010/0017 self-checks: plain SQL, no framework,
-- run after the migrations it checks, inside one transaction ROLLED BACK at the
-- end so it leaves no residue:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 \
--          -f db/migrations/0001_coord_schema.sql \
--          -f db/migrations/0005_reconcile_step.sql \
--          -f db/migrations/0019_a2a_settlement.sql \
--          -f db/migrations/0019_a2a_settlement_test.sql
--
-- This file proves the STRUCTURAL ACs of the schema (columns, CHECK, partial
-- index, the at-most-once conditional-UPDATE shape). The Go writer's behaviour
-- (audit 'a2a_settled' emitted exactly once; outcome derivation) is exercised by
-- pkg/coord/a2asettle_test.go.

BEGIN;

-- (1) Both settlement columns exist and a valid outcome persists. The at-most-once
--     conditional UPDATE (... WHERE a2a_task_id=$1 AND settled_at IS NULL) lands
--     settled_at + settle_outcome on a fresh (unsettled) dispatch row.
DO $$
DECLARE wi uuid; rid uuid := gen_random_uuid(); n int;
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by)
         VALUES (gen_random_uuid(), 'settle item', 'principal:test')
      RETURNING id INTO wi;
    INSERT INTO coord.a2a_dispatch (a2a_task_id, work_item_id, run_id)
         VALUES (rid::text, wi, rid);

    UPDATE coord.a2a_dispatch
       SET settled_at = now(), settle_outcome = 'succeeded'
     WHERE a2a_task_id = rid::text AND settled_at IS NULL;
    GET DIAGNOSTICS n = ROW_COUNT;
    ASSERT n = 1, 'first Settle must land on the fresh dispatch row (1 row)';

    ASSERT (SELECT settle_outcome FROM coord.a2a_dispatch WHERE a2a_task_id = rid::text) = 'succeeded',
        'settle_outcome must persist the written outcome';
    ASSERT (SELECT settled_at IS NOT NULL FROM coord.a2a_dispatch WHERE a2a_task_id = rid::text),
        'settled_at must be stamped on settle';
END $$;

-- (2) At-most-once: a SECOND Settle with the same a2a_task_id is a no-op — the
--     `settled_at IS NULL` guard matches nothing, so ROW_COUNT is 0 and the
--     first writer's outcome is never overwritten.
DO $$
DECLARE wi uuid; rid uuid := gen_random_uuid(); n int;
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by)
         VALUES (gen_random_uuid(), 'settle idem item', 'principal:test')
      RETURNING id INTO wi;
    INSERT INTO coord.a2a_dispatch (a2a_task_id, work_item_id, run_id)
         VALUES (rid::text, wi, rid);

    UPDATE coord.a2a_dispatch SET settled_at = now(), settle_outcome = 'succeeded'
     WHERE a2a_task_id = rid::text AND settled_at IS NULL;

    UPDATE coord.a2a_dispatch SET settled_at = now(), settle_outcome = 'failed'
     WHERE a2a_task_id = rid::text AND settled_at IS NULL;
    GET DIAGNOSTICS n = ROW_COUNT;
    ASSERT n = 0, 'second Settle must be a no-op (0 rows) — the marker is at-most-once';

    ASSERT (SELECT settle_outcome FROM coord.a2a_dispatch WHERE a2a_task_id = rid::text) = 'succeeded',
        'the first writer wins — a re-entry must not overwrite the outcome';
END $$;

-- (3) The CHECK admits exactly the closed outcome set: all three canonical
--     outcomes are accepted, and drift to an unclassifiable outcome fails closed.
DO $$
DECLARE wi uuid; rid uuid; outcome text; blocked boolean := false;
BEGIN
    -- Each canonical outcome lands on its own fresh dispatch row.
    FOREACH outcome IN ARRAY ARRAY['succeeded', 'failed', 'follow_error'] LOOP
        rid := gen_random_uuid();
        INSERT INTO coord.work_item (project_id, title, created_by)
             VALUES (gen_random_uuid(), 'settle ok item', 'principal:test')
          RETURNING id INTO wi;
        INSERT INTO coord.a2a_dispatch (a2a_task_id, work_item_id, run_id)
             VALUES (rid::text, wi, rid);
        UPDATE coord.a2a_dispatch SET settled_at = now(), settle_outcome = outcome
         WHERE a2a_task_id = rid::text AND settled_at IS NULL;
        ASSERT (SELECT settle_outcome FROM coord.a2a_dispatch WHERE a2a_task_id = rid::text) = outcome,
            format('canonical outcome %L must be admitted', outcome);
    END LOOP;

    -- A near-miss (typo) outcome is rejected by the CHECK.
    rid := gen_random_uuid();
    INSERT INTO coord.work_item (project_id, title, created_by)
         VALUES (gen_random_uuid(), 'settle drift item', 'principal:test')
      RETURNING id INTO wi;
    INSERT INTO coord.a2a_dispatch (a2a_task_id, work_item_id, run_id)
         VALUES (rid::text, wi, rid);
    BEGIN
        UPDATE coord.a2a_dispatch SET settled_at = now(), settle_outcome = 'succeeeded'
         WHERE a2a_task_id = rid::text;
    EXCEPTION WHEN check_violation THEN blocked := true;
    END;
    ASSERT blocked, 'an out-of-family settle_outcome must be rejected by the CHECK';
END $$;

-- (4) The reaper's partial index exists on (run_id) WHERE settled_at IS NOT NULL.
DO $$
BEGIN
    ASSERT EXISTS (
        SELECT 1 FROM pg_indexes
         WHERE schemaname = 'coord'
           AND tablename  = 'a2a_dispatch'
           AND indexname  = 'idx_a2a_dispatch_settled'),
        'the reaper lookup index idx_a2a_dispatch_settled must exist';
END $$;

-- (5) S3 read path (ADR-0020 §2.4, ISI-4403): the `workItemRef → settled_at` join.
--     spec.workItemRef IS a coord work_item id, so S3 reads settled_at keyed by
--     work_item_id. Prove (a) that read returns the settled marker, and (b) it is
--     index-covered by the leading column of idx_a2a_dispatch_run (from 0005) —
--     the guarantee ISI-4403 asked S1 to confirm, so no new join index is needed.
DO $$
DECLARE wi uuid; rid uuid := gen_random_uuid(); got timestamptz;
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by)
         VALUES (gen_random_uuid(), 'settle join item', 'principal:test')
      RETURNING id INTO wi;
    INSERT INTO coord.a2a_dispatch (a2a_task_id, work_item_id, run_id)
         VALUES (rid::text, wi, rid);
    UPDATE coord.a2a_dispatch SET settled_at = now(), settle_outcome = 'succeeded'
     WHERE a2a_task_id = rid::text AND settled_at IS NULL;

    -- (a) S3's join key `wi` (= workItemRef) reads back the settlement marker.
    SELECT settled_at INTO got
      FROM coord.a2a_dispatch WHERE work_item_id = wi;
    ASSERT got IS NOT NULL,
        'S3 must be able to read settled_at keyed by work_item_id (= spec.workItemRef)';

    -- (b) that lookup is index-covered: work_item_id is the LEADING column of
    --     idx_a2a_dispatch_run, so no dedicated join index is required in S1.
    ASSERT EXISTS (
        SELECT 1 FROM pg_indexes
         WHERE schemaname = 'coord'
           AND tablename  = 'a2a_dispatch'
           AND indexname  = 'idx_a2a_dispatch_run'),
        'S3 join key work_item_id must be index-covered by idx_a2a_dispatch_run (0005)';
END $$;

ROLLBACK;
