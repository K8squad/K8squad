-- 0004_discussion_schema_test.sql — runnable self-check for the discussion schema (Story 10.1 / ISI-2702)
--
-- No framework, no fixture: plain SQL that fails loudly if the §7.5 invariants break. Runs against a
-- throwaway Postgres AFTER 0004_discussion_schema.sql is applied:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f db/migrations/0004_discussion_schema.sql \
--                                              -f db/migrations/0004_discussion_schema_test.sql
--
-- Everything runs inside one transaction that is ROLLED BACK at the end, so the check leaves no
-- residue and can run repeatedly. Any failed assertion aborts with a non-zero exit (ON_ERROR_STOP).
-- Server-stamped provenance (AC3) and tenancy-scoped reads (AC5) are additionally exercised by the
-- apiserver integration test (internal/discussion) against a real Postgres — those are write-path/
-- query-path properties, not expressible in schema SQL. This file proves the *structural* ACs
-- (AC1/AC2/AC4). The falsification differential lives in docs/bmad/spikes/bench/discussion-schema-check.py.

BEGIN;

-- (AC1) Exactly the two §7.5 tables exist in the discussion schema, and there is NO room table anywhere.
DO $$
DECLARE n int;
BEGIN
    SELECT count(*) INTO n FROM information_schema.tables
     WHERE table_schema = 'discussion' AND table_name IN ('thread','message');
    ASSERT n = 2, format('expected discussion.thread + discussion.message, found %s', n);

    SELECT count(*) INTO n FROM information_schema.tables
     WHERE table_name IN ('discussion_room','discussion_rooms');
    ASSERT n = 0, format('R1 violation: a room table exists (%s) — the room is the Project', n);
END $$;

-- (AC4) NO coordination / custody column exists on either table (the §7.5 fence, structural).
DO $$
DECLARE bad text;
BEGIN
    SELECT string_agg(table_name||'.'||column_name, ', ') INTO bad
      FROM information_schema.columns
     WHERE table_schema = 'discussion'
       AND column_name IN ('claim','lease','fence_token','state','holder','assignee','status',
                           'checked_out_by','holder_principal','custody');
    ASSERT bad IS NULL, format('AC4 violation: coordination column(s) present: %s', bad);
END $$;

-- (AC2) NOT-NULL discipline: an unscoped or unattributed row is un-representable.
DO $$
DECLARE nn text;
BEGIN
    -- thread: project_id, team_id, title, created_by, created_at all NOT NULL
    SELECT string_agg(column_name, ', ') INTO nn
      FROM information_schema.columns
     WHERE table_schema='discussion' AND table_name='thread'
       AND column_name IN ('project_id','team_id','title','created_by','created_at')
       AND is_nullable = 'YES';
    ASSERT nn IS NULL, format('thread column(s) must be NOT NULL: %s', nn);

    -- message: thread_id, author_principal, body, created_at NOT NULL
    SELECT string_agg(column_name, ', ') INTO nn
      FROM information_schema.columns
     WHERE table_schema='discussion' AND table_name='message'
       AND column_name IN ('thread_id','author_principal','body','created_at')
       AND is_nullable = 'YES';
    ASSERT nn IS NULL, format('message column(s) must be NOT NULL: %s', nn);
END $$;

-- (AC2/R2) The provenance triple exists and author_agent_id / author_run_id / invalidated_at are NULLABLE.
DO $$
DECLARE n int;
BEGIN
    SELECT count(*) INTO n FROM information_schema.columns
     WHERE table_schema='discussion' AND table_name='message'
       AND column_name IN ('author_principal','author_agent_id','author_run_id');
    ASSERT n = 3, format('expected the provenance triple on message, found %s', n);

    SELECT count(*) INTO n FROM information_schema.columns
     WHERE table_schema='discussion' AND table_name='message'
       AND column_name IN ('author_agent_id','author_run_id','invalidated_at')
       AND is_nullable = 'NO';
    ASSERT n = 0, 'author_agent_id / author_run_id / invalidated_at must be NULLABLE';
END $$;

-- (AC2/§7.4) Append-only: hard DELETE and body/author UPDATE are rejected; the soft-retract is allowed once.
DO $$
DECLARE tid uuid; mid uuid; ok boolean;
BEGIN
    INSERT INTO discussion.thread (project_id, team_id, title, created_by)
         VALUES (gen_random_uuid(), gen_random_uuid(), 't', 'principal:opener')
      RETURNING id INTO tid;
    INSERT INTO discussion.message (thread_id, author_principal, body)
         VALUES (tid, 'principal:a', 'hello') RETURNING id INTO mid;

    -- hard DELETE must fail
    ok := true;
    BEGIN DELETE FROM discussion.message WHERE id = mid; EXCEPTION WHEN others THEN ok := false; END;
    ASSERT ok = false, 'AC2 violation: hard DELETE of a message was permitted';

    -- editing the body must fail (append-only)
    ok := true;
    BEGIN UPDATE discussion.message SET body='edited' WHERE id = mid; EXCEPTION WHEN others THEN ok := false; END;
    ASSERT ok = false, 'AC2 violation: editing message body was permitted';

    -- the soft-retract (invalidated_at NULL→ts) must succeed
    UPDATE discussion.message SET invalidated_at = now() WHERE id = mid;

    -- re-validation (ts→NULL) must fail (one-way)
    ok := true;
    BEGIN UPDATE discussion.message SET invalidated_at = NULL WHERE id = mid; EXCEPTION WHEN others THEN ok := false; END;
    ASSERT ok = false, '§7.4 violation: un-retract (invalidated_at ts→NULL) was permitted';

    -- thread is immutable
    ok := true;
    BEGIN UPDATE discussion.thread SET title='x' WHERE id = tid; EXCEPTION WHEN others THEN ok := false; END;
    ASSERT ok = false, 'AC2 violation: editing a thread was permitted';
END $$;

-- (structural) a reply's parent must live in the same thread.
DO $$
DECLARE t1 uuid; t2 uuid; m1 uuid; ok boolean;
BEGIN
    INSERT INTO discussion.thread (project_id, team_id, title, created_by)
         VALUES (gen_random_uuid(), gen_random_uuid(), 't1', 'p') RETURNING id INTO t1;
    INSERT INTO discussion.thread (project_id, team_id, title, created_by)
         VALUES (gen_random_uuid(), gen_random_uuid(), 't2', 'p') RETURNING id INTO t2;
    INSERT INTO discussion.message (thread_id, author_principal, body)
         VALUES (t1, 'p', 'root') RETURNING id INTO m1;

    ok := true;
    BEGIN
        INSERT INTO discussion.message (thread_id, parent_id, author_principal, body)
             VALUES (t2, m1, 'p', 'cross-thread reply');
    EXCEPTION WHEN others THEN ok := false; END;
    ASSERT ok = false, 'structural violation: a reply parented into a different thread was permitted';
END $$;

ROLLBACK;
