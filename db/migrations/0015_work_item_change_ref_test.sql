-- 0015_work_item_change_ref_test.sql — runnable self-check for ISI-4131 (M1.5).
--
-- Same no-framework discipline as 0014: plain SQL that fails loudly if the
-- change-ref surface or its append-only contract breaks. Runs AFTER the full
-- migration set, inside one transaction ROLLED BACK at the end:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f db/migrations/*.sql \
--                                              -f db/migrations/0015_work_item_change_ref_test.sql

BEGIN;

-- (1) The table exists with the tight kind enum and required provenance columns.
DO $$
DECLARE t text;
BEGIN
    SELECT data_type INTO t FROM information_schema.columns
     WHERE table_schema='coord' AND table_name='change_ref' AND column_name='kind';
    ASSERT t = 'text', format('expected coord.change_ref.kind text, found %s', coalesce(t,'<absent>'));

    SELECT data_type INTO t FROM information_schema.columns
     WHERE table_schema='coord' AND table_name='change_ref' AND column_name='author_principal';
    ASSERT t = 'text', format('expected coord.change_ref.author_principal text, found %s', coalesce(t,'<absent>'));
END $$;

-- (2) A valid change ref round-trips; attribution + run provenance survive.
DO $$
DECLARE wid uuid; rid uuid; k text; r text; a text;
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by)
    VALUES (gen_random_uuid(), 'change-ref round-trip self-check', 'tester')
    RETURNING id INTO wid;

    INSERT INTO coord.change_ref (work_item_id, run_id, kind, ref, summary, author_principal)
    VALUES (wid, gen_random_uuid(), 'commit', 'deadbeefcafe', 'fix the widget', 'agent-A');

    SELECT kind, ref, author_principal INTO k, r, a FROM coord.change_ref WHERE work_item_id = wid;
    ASSERT k = 'commit', format('kind round-trip mismatch, found %s', k);
    ASSERT r = 'deadbeefcafe', format('ref round-trip mismatch, found %s', r);
    ASSERT a = 'agent-A', format('author round-trip mismatch, found %s', a);
END $$;

-- (3) The kind enum rejects anything that is not commit|pull_request.
DO $$
DECLARE wid uuid; ok boolean := false;
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by)
    VALUES (gen_random_uuid(), 'change-ref enum self-check', 'tester')
    RETURNING id INTO wid;

    BEGIN
        INSERT INTO coord.change_ref (work_item_id, kind, ref, author_principal)
        VALUES (wid, 'sneaky_kind', 'x', 'agent-A');
    EXCEPTION WHEN check_violation THEN
        ok := true;
    END;
    ASSERT ok, 'coord.change_ref.kind accepted a value outside (commit, pull_request)';
END $$;

-- (4) APPEND-ONLY: UPDATE and DELETE on coord.change_ref are rejected by the
-- shared reject_mutation trigger, and TRUNCATE is closed too (§6.5).
DO $$
DECLARE wid uuid; crid uuid; upd boolean := false; del boolean := false; trn boolean := false;
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by)
    VALUES (gen_random_uuid(), 'change-ref append-only self-check', 'tester')
    RETURNING id INTO wid;

    INSERT INTO coord.change_ref (work_item_id, kind, ref, author_principal)
    VALUES (wid, 'pull_request', 'https://scm.example/pr/7', 'agent-A')
    RETURNING id INTO crid;

    BEGIN
        UPDATE coord.change_ref SET summary = 'rewritten' WHERE id = crid;
    EXCEPTION WHEN restrict_violation THEN
        upd := true;
    END;
    ASSERT upd, 'coord.change_ref UPDATE was not rejected — append-only contract broken';

    BEGIN
        DELETE FROM coord.change_ref WHERE id = crid;
    EXCEPTION WHEN restrict_violation THEN
        del := true;
    END;
    ASSERT del, 'coord.change_ref DELETE was not rejected — append-only contract broken';

    BEGIN
        TRUNCATE coord.change_ref;
    EXCEPTION WHEN restrict_violation THEN
        trn := true;
    END;
    ASSERT trn, 'coord.change_ref TRUNCATE was not rejected — append-only contract broken';
END $$;

ROLLBACK;
