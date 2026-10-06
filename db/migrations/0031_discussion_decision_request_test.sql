-- 0031_discussion_decision_request_test.sql — runnable self-check for ISI-5536 (ADR-0026 §4).
--
-- Same no-framework discipline as 0025/0030: plain SQL that fails loudly if the decision_request
-- lifecycle surface or its CHECK/UNIQUE contracts break. Runs AFTER the full migration set, inside
-- one transaction ROLLED BACK at the end:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f db/migrations/*.sql \
--                                              -f db/migrations/0031_discussion_decision_request_test.sql

BEGIN;

-- Fixtures: a thread + a decision_request message to hang a lifecycle row on. The thread/message
-- shapes mirror 0004/0024 so this stays truthful against the real schema.
INSERT INTO discussion.thread (id, project_id, team_id, title, created_by)
VALUES ('00000000-0000-0000-0000-0000000d0001', 'team-a/proj-x',
        '00000000-0000-0000-0000-0000000d00aa', 'decision fixture', 'tester');

INSERT INTO discussion.message (id, thread_id, author_principal, body, audience, kind, payload)
VALUES ('00000000-0000-0000-0000-0000000d0002', '00000000-0000-0000-0000-0000000d0001',
        'winston', 'Which HTTP client?', 'party', 'decision_request',
        '{"version":1,"mode":"choose_one","title":"Which HTTP client?"}');

-- (1) kind='decision_request' is now an accepted message kind (0031 widened the CHECK).
DO $$
DECLARE k text;
BEGIN
    SELECT kind INTO k FROM discussion.message WHERE id = '00000000-0000-0000-0000-0000000d0002';
    ASSERT k = 'decision_request', format('expected decision_request kind, found %s', coalesce(k,'<absent>'));
END $$;

-- (2) A lifecycle row inserts with the honest defaults (phase=open, continuation set) and round-trips
--     the work_item_id binding the card to its ticket (BE-6 continuation / BE-7 Inbox join).
DO $$
DECLARE p text; c text; w uuid;
BEGIN
    INSERT INTO discussion.decision_request (message_id, team_id, idempotency_key, work_item_id, bound_revision_id)
    VALUES ('00000000-0000-0000-0000-0000000d0002', '00000000-0000-0000-0000-0000000d00aa',
            'decision:t1:http-client:r1', '00000000-0000-0000-0000-0000000d00f1', 'rev-1');
    SELECT phase, continuation, work_item_id INTO p, c, w
      FROM discussion.decision_request WHERE message_id = '00000000-0000-0000-0000-0000000d0002';
    ASSERT p = 'open', format('expected default phase open, found %s', p);
    ASSERT c = 'resume_agent_on_answer', format('expected default continuation, found %s', c);
    ASSERT w = '00000000-0000-0000-0000-0000000d00f1', format('work_item_id did not round-trip, found %s', coalesce(w::text,'<null>'));
END $$;

-- (3) The phase CHECK rejects a value outside the state machine.
DO $$
DECLARE ok boolean := false;
BEGIN
    BEGIN
        UPDATE discussion.decision_request SET phase = 'exploded'
         WHERE message_id = '00000000-0000-0000-0000-0000000d0002';
    EXCEPTION WHEN check_violation THEN
        ok := true;
    END;
    ASSERT ok, 'discussion.decision_request.phase accepted a value outside the allowed set';
END $$;

-- (4) (team_id, idempotency_key) is UNIQUE: a second card with the same key IN THE SAME TEAM is
--     refused (idempotent-create guard).
DO $$
DECLARE ok boolean := false;
BEGIN
    -- a sibling message to attach the duplicate lifecycle to (PK would otherwise mask the test)
    INSERT INTO discussion.message (id, thread_id, author_principal, body, audience, kind, payload)
    VALUES ('00000000-0000-0000-0000-0000000d0003', '00000000-0000-0000-0000-0000000d0001',
            'winston', 'dup', 'party', 'decision_request', '{"version":1,"mode":"approve","title":"dup"}');
    BEGIN
        INSERT INTO discussion.decision_request (message_id, team_id, idempotency_key)
        VALUES ('00000000-0000-0000-0000-0000000d0003', '00000000-0000-0000-0000-0000000d00aa',
                'decision:t1:http-client:r1');
    EXCEPTION WHEN unique_violation THEN
        ok := true;
    END;
    ASSERT ok, 'discussion.decision_request accepted a duplicate (team_id, idempotency_key)';
END $$;

-- (4b) The SAME idempotency_key in a DIFFERENT team is ALLOWED (M5: uniqueness is tenant-scoped, not
--      global — a global unique would let one team's key block another team's create).
DO $$
DECLARE n int;
BEGIN
    INSERT INTO discussion.thread (id, project_id, team_id, title, created_by)
    VALUES ('00000000-0000-0000-0000-0000000d0010', 'team-b/proj-x',
            '00000000-0000-0000-0000-0000000d00bb', 'team B room', 'tester');
    INSERT INTO discussion.message (id, thread_id, author_principal, body, audience, kind, payload)
    VALUES ('00000000-0000-0000-0000-0000000d0011', '00000000-0000-0000-0000-0000000d0010',
            'winston', 'B asks', 'party', 'decision_request', '{"version":1,"mode":"approve","title":"B"}');
    INSERT INTO discussion.decision_request (message_id, team_id, idempotency_key)
    VALUES ('00000000-0000-0000-0000-0000000d0011', '00000000-0000-0000-0000-0000000d00bb',
            'decision:t1:http-client:r1');  -- same key as team A, different team
    SELECT count(*) INTO n FROM discussion.decision_request WHERE idempotency_key = 'decision:t1:http-client:r1';
    ASSERT n = 2, format('expected 2 cards sharing a key across 2 teams, found %s', n);
END $$;

-- (5) The open→answered transition round-trips the stamp + typed answer.
DO $$
DECLARE p text; a jsonb; who text;
BEGIN
    UPDATE discussion.decision_request
       SET phase = 'answered', answered_by = 'alice', answered_at = now(),
           answer = '{"mode":"choose_one","selectedOptionIds":["reqwest"],"rejected":false}', updated_at = now()
     WHERE message_id = '00000000-0000-0000-0000-0000000d0002' AND phase = 'open';
    SELECT phase, answer, answered_by INTO p, a, who
      FROM discussion.decision_request WHERE message_id = '00000000-0000-0000-0000-0000000d0002';
    ASSERT p = 'answered', format('expected phase answered, found %s', p);
    ASSERT who = 'alice', format('expected answered_by alice, found %s', coalesce(who,'<null>'));
    ASSERT a->>'mode' = 'choose_one', format('answer payload did not round-trip, found %s', coalesce(a::text,'<null>'));
END $$;

ROLLBACK;
