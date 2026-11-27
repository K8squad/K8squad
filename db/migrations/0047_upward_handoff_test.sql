-- 0047_upward_handoff_test.sql — runnable self-check for the WS-B marker
-- table (ISI-7956). Same discipline as the 0045 companion: plain SQL, loud
-- ASSERTs, wrapped in BEGIN/ROLLBACK so it leaves no residue.
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f db/migrations/0001_coord_schema.sql
--          ... -f db/migrations/0047_upward_handoff.sql
--          -f db/migrations/0047_upward_handoff_test.sql

BEGIN;

-- (1) The table exists with the closed kind enum and the NOT NULL run key.
DO $$
DECLARE k text; n text;
BEGIN
    SELECT data_type INTO k FROM information_schema.columns
     WHERE table_schema='coord' AND table_name='upward_handoff' AND column_name='kind';
    ASSERT k = 'text', format('expected coord.upward_handoff.kind text, found %s', coalesce(k,'<absent>'));

    SELECT is_nullable INTO n FROM information_schema.columns
     WHERE table_schema='coord' AND table_name='upward_handoff' AND column_name='run_id';
    ASSERT n = 'NO', format('expected run_id NOT NULL (the conflict guard keys on it), found is_nullable=%s', coalesce(n,'<absent>'));
END $$;

-- (2) A push round-trips; a second push of the same (item, run, kind) is
-- rejected by the at-most-once constraint.
DO $$
DECLARE cnt bigint;
BEGIN
    INSERT INTO coord.work_item (project_id, title, state, created_by)
    VALUES ('33333333-3333-3333-3333-333333333333', 'wsb selfcheck', 'todo', 'selfcheck');

    INSERT INTO coord.upward_handoff (work_item_id, run_id, from_agent, manager, kind, brief)
    SELECT id, '44444444-4444-4444-4444-444444444444', 'ic-one', 'manager-two', 'pr',
           '{"did":["run finished"]}'::jsonb
      FROM coord.work_item WHERE title = 'wsb selfcheck';

    BEGIN
        INSERT INTO coord.upward_handoff (work_item_id, run_id, from_agent, manager, kind, brief)
        SELECT id, '44444444-4444-4444-4444-444444444444', 'ic-one', 'manager-two', 'pr',
               '{"did":["run finished"]}'::jsonb
          FROM coord.work_item WHERE title = 'wsb selfcheck';
        RAISE EXCEPTION 'duplicate (work_item_id, run_id, kind) push was admitted';
    EXCEPTION WHEN unique_violation THEN NULL; -- the at-most-once guard fired
    END;

    SELECT count(*) INTO cnt FROM coord.upward_handoff
     WHERE run_id = '44444444-4444-4444-4444-444444444444';
    ASSERT cnt = 1, format('expected exactly one marker row, found %s', cnt);
END $$;

ROLLBACK;

