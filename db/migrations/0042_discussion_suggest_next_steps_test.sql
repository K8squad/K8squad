-- 0042_discussion_suggest_next_steps_test.sql — runnable self-check for ISI-7218
-- (ISI-7075 C4, taxonomy §5).
--
-- Same no-framework discipline as 0031: plain SQL that fails loudly if the widened
-- kind CHECK or the SHARED lifecycle surface breaks. Runs AFTER the full migration
-- set, inside one transaction ROLLED BACK at the end:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f db/migrations/*.sql \
--                                  -f db/migrations/0042_discussion_suggest_next_steps_test.sql

BEGIN;

-- Fixtures: a thread + a kind='suggest_next_steps' message to hang a lifecycle row
-- on (distinct uuid block from 0031's 0d fixtures). Shapes mirror 0004/0024.
INSERT INTO discussion.thread (id, project_id, team_id, title, created_by)
VALUES ('00000000-0000-0000-0000-0000000e0001', 'team-a/proj-x',
        '00000000-0000-0000-0000-0000000e00aa', 'suggest fixture', 'tester');

INSERT INTO discussion.message (id, thread_id, author_principal, body, audience, kind, payload)
VALUES ('00000000-0000-0000-0000-0000000e0002', '00000000-0000-0000-0000-0000000e0001',
        'alfred', 'Blocked on GPU quota — next steps?', 'party', 'suggest_next_steps',
        '{"version":1,"mode":"choose_one","title":"Blocked on GPU quota — next steps?"}');

-- (1) kind='suggest_next_steps' is now an accepted message kind (0042 widened the
--     CHECK) and round-trips through the append-only message row.
DO $$
DECLARE k text;
BEGIN
    SELECT kind INTO k FROM discussion.message WHERE id = '00000000-0000-0000-0000-0000000e0002';
    ASSERT k = 'suggest_next_steps', format('expected suggest_next_steps kind, found %s', coalesce(k,'<absent>'));
END $$;

-- (2) The lifecycle rides the SHARED discussion.decision_request side table (0031):
--     a suggest card inserts with the honest defaults (phase=open, continuation
--     resume_agent_on_answer) and round-trips the work_item_id binding — one state
--     machine for both kinds, the message kind is the discriminator.
DO $$
DECLARE p text; c text; w uuid;
BEGIN
    INSERT INTO discussion.decision_request (message_id, team_id, idempotency_key, work_item_id)
    VALUES ('00000000-0000-0000-0000-0000000e0002', '00000000-0000-0000-0000-0000000e00aa',
            'suggest:t1:quota:r1', '00000000-0000-0000-0000-0000000e00f1');
    SELECT phase, continuation, work_item_id INTO p, c, w
      FROM discussion.decision_request WHERE message_id = '00000000-0000-0000-0000-0000000e0002';
    ASSERT p = 'open', format('expected default phase open, found %s', p);
    ASSERT c = 'resume_agent_on_answer', format('expected default continuation, found %s', c);
    ASSERT w = '00000000-0000-0000-0000-0000000e00f1', format('work_item_id did not round-trip, found %s', coalesce(w::text,'<null>'));
END $$;

-- (3) The widened CHECK is still a fence: an unknown kind remains refused.
DO $$
DECLARE ok boolean := false;
BEGIN
    BEGIN
        INSERT INTO discussion.message (id, thread_id, author_principal, body, audience, kind)
        VALUES ('00000000-0000-0000-0000-0000000e0003', '00000000-0000-0000-0000-0000000e0001',
                'alfred', 'forge', 'party', 'mind_control');
    EXCEPTION WHEN check_violation THEN
        ok := true;
    END;
    ASSERT ok, 'discussion.message.kind accepted a value outside the allowed set';
END $$;

-- (4) (team_id, idempotency_key) UNIQUE guards the suggest idempotent create too: a
--     second card with the same key IN THE SAME TEAM is refused.
DO $$
DECLARE ok boolean := false;
BEGIN
    BEGIN
        INSERT INTO discussion.decision_request (message_id, team_id, idempotency_key)
        VALUES ('00000000-0000-0000-0000-0000000e0003', '00000000-0000-0000-0000-0000000e00aa',
                'suggest:t1:quota:r1');
    EXCEPTION WHEN unique_violation THEN
        ok := true;
    END;
    ASSERT ok, 'a duplicate (team_id, idempotency_key) was accepted for a suggest card';
END $$;

ROLLBACK;
