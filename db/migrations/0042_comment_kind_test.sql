-- 0042_comment_kind_test.sql — runnable self-check for ISI-7094 (two-phase comment kind).
--
-- Same no-framework discipline as 0040/0037: plain SQL that fails loudly if the comment
-- kind/run_id contract or the [run-prefix backfill breaks. Runs AFTER the full migration
-- set, inside ONE transaction ROLLED BACK at the end (leaves no residue).

BEGIN;

-- (1) The two additive columns exist with the right type/nullability/default.
DO $$
DECLARE t text; n text; d text;
BEGIN
    SELECT data_type, is_nullable, column_default
      INTO t, n, d
      FROM information_schema.columns
     WHERE table_schema='coord' AND table_name='comment' AND column_name='kind';
    ASSERT t = 'text',  format('expected coord.comment.kind text, found %s', coalesce(t,'<absent>'));
    ASSERT n = 'NO',    format('expected kind NOT NULL, found is_nullable=%s', coalesce(n,'<absent>'));
    ASSERT d LIKE '%human%', format('expected kind DEFAULT human, found %s', coalesce(d,'<absent>'));

    SELECT data_type, is_nullable
      INTO t, n
      FROM information_schema.columns
     WHERE table_schema='coord' AND table_name='comment' AND column_name='run_id';
    ASSERT t = 'uuid', format('expected coord.comment.run_id uuid, found %s', coalesce(t,'<absent>'));
    ASSERT n = 'YES',  format('expected run_id NULLable, found is_nullable=%s', coalesce(n,'<absent>'));
END $$;

-- Seed a parent work item the comments FK against.
INSERT INTO coord.work_item (id, project_id, title, created_by)
VALUES ('00000000-0000-0000-0000-0000000070a4'::uuid,
        '00000000-0000-0000-0000-0000000000a1'::uuid, 'ISI-7094 self-check', 'tester');

-- (2) A plain INSERT (no kind) defaults to 'human' — a first-class top-level comment,
--     exactly as every pre-0042 writer behaved.
DO $$
DECLARE k text;
BEGIN
    INSERT INTO coord.comment (work_item_id, author_principal, body)
    VALUES ('00000000-0000-0000-0000-0000000070a4'::uuid, 'human:alice', 'looks good to me');
    SELECT kind INTO k FROM coord.comment WHERE body = 'looks good to me';
    ASSERT k = 'human', format('expected default kind=human, found %s', coalesce(k,'<absent>'));
END $$;

-- (3) The CHECK constraint refuses an out-of-enum kind.
DO $$
DECLARE ok boolean := false;
BEGIN
    BEGIN
        INSERT INTO coord.comment (work_item_id, author_principal, body, kind)
        VALUES ('00000000-0000-0000-0000-0000000070a4'::uuid, 'agent:x', 'bogus', 'chatter');
    EXCEPTION WHEN check_violation THEN ok := true;
    END;
    ASSERT ok, 'expected CHECK to reject kind=chatter';
END $$;

-- (4) progress + summary rows accept a run_id and round-trip.
INSERT INTO coord.comment (work_item_id, author_principal, body, kind, run_id)
VALUES ('00000000-0000-0000-0000-0000000070a4'::uuid, 'agent:x',
        '[run abc12345][tool:bash/result(ok)] built', 'progress',
        '00000000-0000-0000-0000-000000000701'::uuid);
INSERT INTO coord.comment (work_item_id, author_principal, body, kind, run_id)
VALUES ('00000000-0000-0000-0000-0000000070a4'::uuid, 'ksquad-engine',
        'What I did: shipped the thing', 'summary',
        '00000000-0000-0000-0000-000000000701'::uuid);

DO $$
DECLARE cnt int;
BEGIN
    -- Default top-level query shape (readComments): kind IN (human,summary), progress excluded.
    SELECT count(*) INTO cnt FROM coord.comment
     WHERE work_item_id='00000000-0000-0000-0000-0000000070a4'::uuid
       AND kind IN ('human','summary');
    ASSERT cnt = 2, format('expected 2 top-level (human+summary), found %s', cnt);

    -- Per-run Activity query shape (ReadRunActivity): exactly one progress row for the run.
    SELECT count(*) INTO cnt FROM coord.comment
     WHERE work_item_id='00000000-0000-0000-0000-0000000070a4'::uuid
       AND kind='progress'
       AND run_id='00000000-0000-0000-0000-000000000701'::uuid;
    ASSERT cnt = 1, format('expected 1 progress row for run, found %s', cnt);

    -- The exactly-one-summary existence check settle runs.
    SELECT count(*) INTO cnt FROM coord.comment
     WHERE work_item_id='00000000-0000-0000-0000-0000000070a4'::uuid
       AND kind='summary'
       AND run_id='00000000-0000-0000-0000-000000000701'::uuid;
    ASSERT cnt = 1, format('expected exactly 1 summary for run, found %s', cnt);
END $$;

-- (5) append-only is STILL enforced after the backfill re-enabled the trigger: a runtime
--     UPDATE to a comment is rejected (the migration disabled it only for its own backfill).
DO $$
DECLARE ok boolean := false;
BEGIN
    BEGIN
        UPDATE coord.comment SET kind='progress' WHERE body = 'looks good to me';
    EXCEPTION WHEN restrict_violation THEN ok := true;
    END;
    ASSERT ok, 'expected comment_append_only to reject a runtime UPDATE';
END $$;

ROLLBACK;
