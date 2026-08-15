-- 0002_coord_dispatch_test.sql — runnable self-check for the §6.4 dispatch marker
-- (Story 2.9 / ISI-2526).
--
-- Same discipline as 0001_coord_schema_test.sql: plain SQL, no framework, run
-- after the migration it checks, inside one transaction that is ROLLED BACK at
-- the end so it leaves no residue:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f db/migrations/0001_coord_schema.sql \
--                                              -f db/migrations/0002_coord_dispatch.sql \
--                                              -f db/migrations/0002_coord_dispatch_test.sql
--
-- The behavioural guarantees (idempotent dispatch under concurrency, no custody
-- transfer, coordinator-defined content) are exercised by the Go chaos suite
-- (spine-chaos.yml, TestSpineProdDispatch D1..D4) — this file proves the
-- *structural* AC of the marker.

BEGIN;

-- (1) The marker table exists in the coord schema.
DO $$
DECLARE n int;
BEGIN
    SELECT count(*) INTO n FROM information_schema.tables
     WHERE table_schema = 'coord' AND table_name = 'dispatch';
    ASSERT n = 1, format('expected coord.dispatch, found %s', n);
END $$;

-- (2) The dedupe key is a hard structural guard: the SAME (source item, source
--     run) cannot record two dispatches — the second insert is rejected, so a
--     concurrent/re-driven coordinator converges on the first writer's item.
DO $$
DECLARE src uuid; dst uuid; dedupe text; first uuid; blocked boolean;
BEGIN
    INSERT INTO coord.work_item (project_id, title, state, created_by)
         VALUES (gen_random_uuid(), 'completed source', 'done', 'principal:test')
      RETURNING id INTO src;
    INSERT INTO coord.work_item (project_id, title, state, created_by)
         VALUES ((SELECT project_id FROM coord.work_item WHERE id = src),
                 'dispatched next', 'todo', 'principal:coordinator')
      RETURNING id INTO dst;

    dedupe := src::text || ':00000000-0000-0000-0000-000000000001:next';
    INSERT INTO coord.dispatch
        (dedupe_key, source_work_item_id, source_run_id, created_work_item_id, coordinator_principal)
    VALUES
        (dedupe, src, '00000000-0000-0000-0000-000000000001'::uuid, dst, 'principal:coordinator');

    blocked := false;
    BEGIN
        INSERT INTO coord.dispatch
            (dedupe_key, source_work_item_id, source_run_id, created_work_item_id, coordinator_principal)
        VALUES
            (dedupe, src, '00000000-0000-0000-0000-000000000001'::uuid,
             gen_random_uuid(), 'principal:coordinator');   -- a second, duplicate dispatch
    EXCEPTION WHEN unique_violation THEN blocked := true;
    END;
    ASSERT blocked, 'duplicate dedupe_key must be rejected (one dispatch per source+run)';
END $$;

-- (3) The marker cannot outlive its work items: RESTRICT keeps the provenance
--     chain (source → created) intact — deleting a referenced work_item fails.
DO $$
DECLARE src uuid; dst uuid; blocked boolean;
BEGIN
    SELECT source_work_item_id, created_work_item_id INTO src, dst FROM coord.dispatch LIMIT 1;
    blocked := false;
    BEGIN
        DELETE FROM coord.work_item WHERE id = dst;
    EXCEPTION
        -- RESTRICT deletion raises restrict_violation (23001); a plain FK
        -- reference would raise foreign_key_violation (23503). Catch both so
        -- the check asserts "the delete is rejected", not one specific code.
        WHEN SQLSTATE '23001' THEN blocked := true;
        WHEN SQLSTATE '23503' THEN blocked := true;
    END;
    ASSERT blocked, 'deleting a dispatched work_item must be rejected (RESTRICT provenance)';
END $$;

ROLLBACK;
