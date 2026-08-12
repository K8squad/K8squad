-- 0001_coord_schema_test.sql — runnable self-check for the coord schema (Story 2.1 / ISI-2191)
--
-- No framework, no fixture: plain SQL that fails loudly if the schema's invariants break. Runs
-- against a throwaway Postgres AFTER 0001_coord_schema.sql is applied:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f db/migrations/0001_coord_schema.sql \
--                                              -f db/migrations/0001_coord_schema_test.sql
--
-- Everything runs inside one transaction that is ROLLED BACK at the end, so the check leaves no
-- residue and can run repeatedly. Any failed assertion aborts with a non-zero exit (ON_ERROR_STOP).
-- The live concurrency/fencing guarantees (§6.2/§6.3) are exercised by the Go chaos suite
-- (spine-chaos.yml, C1..C7) — this file proves the *structural* AC of Story 2.1.

BEGIN;

-- (1) All five §6.1 tables exist in the coord schema.
DO $$
DECLARE n int;
BEGIN
    SELECT count(*) INTO n FROM information_schema.tables
     WHERE table_schema = 'coord'
       AND table_name IN ('work_item','comment','artifact','claim','run_event');
    ASSERT n = 5, format('expected 5 coord tables, found %s', n);
END $$;

-- (2) Creating a work_item auto-provisions exactly one unheld claim row (F3 structural invariant).
DO $$
DECLARE wid uuid; c int; fence bigint; holder text;
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by)
         VALUES (gen_random_uuid(), 'test item', 'principal:test')
      RETURNING id INTO wid;

    SELECT count(*) INTO c FROM coord.claim WHERE work_item_id = wid;
    ASSERT c = 1, format('expected exactly 1 auto-provisioned claim row, found %s', c);

    SELECT fence_token, holder_principal INTO fence, holder FROM coord.claim WHERE work_item_id = wid;
    ASSERT fence = 0,        format('new claim fence_token should be 0, was %s', fence);
    ASSERT holder IS NULL,   'new claim should be unheld (holder_principal NULL)';
END $$;

-- (3) comment is append-only: UPDATE and DELETE are both rejected.
DO $$
DECLARE wid uuid; cid uuid; blocked boolean;
BEGIN
    SELECT id INTO wid FROM coord.work_item LIMIT 1;
    INSERT INTO coord.comment (work_item_id, author_principal, body)
         VALUES (wid, 'principal:test', 'hello') RETURNING id INTO cid;

    blocked := false;
    BEGIN
        UPDATE coord.comment SET body = 'tampered' WHERE id = cid;
    EXCEPTION WHEN others THEN blocked := true;
    END;
    ASSERT blocked, 'UPDATE on coord.comment must be rejected (append-only)';

    blocked := false;
    BEGIN
        DELETE FROM coord.comment WHERE id = cid;
    EXCEPTION WHEN others THEN blocked := true;
    END;
    ASSERT blocked, 'DELETE on coord.comment must be rejected (append-only)';
END $$;

-- (4) run_event is append-only: UPDATE and DELETE are both rejected.
DO $$
DECLARE eid bigint; blocked boolean;
BEGIN
    INSERT INTO coord.run_event (event_type, principal) VALUES ('completed', 'principal:test')
      RETURNING id INTO eid;

    blocked := false;
    BEGIN
        UPDATE coord.run_event SET event_type = 'tampered' WHERE id = eid;
    EXCEPTION WHEN others THEN blocked := true;
    END;
    ASSERT blocked, 'UPDATE on coord.run_event must be rejected (append-only)';

    blocked := false;
    BEGIN
        DELETE FROM coord.run_event WHERE id = eid;
    EXCEPTION WHEN others THEN blocked := true;
    END;
    ASSERT blocked, 'DELETE on coord.run_event must be rejected (append-only)';
END $$;

-- (5) artifact upsert key rejects a duplicate (work_item_id, run_id, kind) (§6.4 idempotency).
DO $$
DECLARE wid uuid; rid uuid := gen_random_uuid(); blocked boolean;
BEGIN
    SELECT id INTO wid FROM coord.work_item LIMIT 1;
    INSERT INTO coord.artifact (work_item_id, run_id, kind, uri, sha256)
         VALUES (wid, rid, 'build', 'oci://x', 'abc');
    blocked := false;
    BEGIN
        INSERT INTO coord.artifact (work_item_id, run_id, kind, uri, sha256)
             VALUES (wid, rid, 'build', 'oci://y', 'def');
    EXCEPTION WHEN unique_violation THEN blocked := true;
    END;
    ASSERT blocked, 'duplicate (work_item_id, run_id, kind) must violate the artifact upsert key';
END $$;

-- (6) board state enum is enforced; self-parent is rejected.
DO $$
DECLARE wid uuid; blocked boolean;
BEGIN
    SELECT id INTO wid FROM coord.work_item LIMIT 1;

    blocked := false;
    BEGIN
        INSERT INTO coord.work_item (project_id, title, created_by, state)
             VALUES (gen_random_uuid(), 'bad state', 'principal:test', 'blocked');
    EXCEPTION WHEN check_violation THEN blocked := true;
    END;
    ASSERT blocked, 'state=''blocked'' must be rejected — blocked is a condition, not a board state';

    blocked := false;
    BEGIN
        UPDATE coord.work_item SET parent_id = wid WHERE id = wid;
    EXCEPTION WHEN check_violation THEN blocked := true;
    END;
    ASSERT blocked, 'self-parent (parent_id = id) must be rejected';
END $$;

-- (7) orphans are first-class: deleting a parent leaves the child as a root (parent_id → NULL).
DO $$
DECLARE pid uuid; cid uuid; p uuid;
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by)
         VALUES (gen_random_uuid(), 'parent', 'principal:test') RETURNING id INTO pid;
    INSERT INTO coord.work_item (project_id, title, created_by, parent_id)
         VALUES (gen_random_uuid(), 'child', 'principal:test', pid) RETURNING id INTO cid;
    DELETE FROM coord.work_item WHERE id = pid;   -- allowed: parent has no comments/artifacts/events
    SELECT parent_id INTO p FROM coord.work_item WHERE id = cid;
    ASSERT p IS NULL, 'orphaned child must render as a root (parent_id set NULL on parent delete)';
END $$;

ROLLBACK;

\echo 'coord schema self-check PASSED (7 assertions)'
