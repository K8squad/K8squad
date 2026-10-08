-- 0033_discussion_party_session_test.sql — runnable self-check for ISI-5585 (ADR-0027 §4.3).
--
-- Same no-framework discipline as 0025/0031: plain SQL that fails loudly if the party-session surface
-- or its CHECK/UNIQUE contracts break. Runs AFTER the full migration set, inside one transaction
-- ROLLED BACK at the end:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f db/migrations/*.sql \
--                                              -f db/migrations/0033_discussion_party_session_test.sql

BEGIN;

-- Fixtures: a thread + a party_start message to hang a session row on. Shapes mirror 0004/0024/0033.
INSERT INTO discussion.thread (id, project_id, team_id, title, created_by)
VALUES ('00000000-0000-0000-0000-0000000e0001', 'team-a/proj-x',
        '00000000-0000-0000-0000-0000000e00aa', 'party fixture', 'tester');

-- (1) kind='party_start' is now an accepted message kind (0033 widened the CHECK). The pre-0033 kinds
--     (decision_request, proposal, text) must still be accepted — the widen is additive.
INSERT INTO discussion.message (id, thread_id, author_principal, body, audience, kind)
VALUES ('00000000-0000-0000-0000-0000000e0002', '00000000-0000-0000-0000-0000000e0001',
        'henrik', 'Let''s debate the storage approach with the team', 'party', 'party_start');
INSERT INTO discussion.message (id, thread_id, author_principal, body, audience, kind)
VALUES ('00000000-0000-0000-0000-0000000e0003', '00000000-0000-0000-0000-0000000e0001',
        'henrik', 'plain post', 'party', 'decision_request');

DO $$
DECLARE k text;
BEGIN
    SELECT kind INTO k FROM discussion.message WHERE id = '00000000-0000-0000-0000-0000000e0002';
    ASSERT k = 'party_start', format('expected party_start kind, found %s', coalesce(k, '<absent>'));
END $$;

-- (2) A session row inserts with honest defaults (round=0, paid_runs_used=0, phase='active') and
--     round-trips the budgets + server-stamped opener/topic.
DO $$
DECLARE r int; u int; st text; sb text;
BEGIN
    INSERT INTO discussion.party_session
        (id, thread_id, project_id, team_id, started_by, topic_message_id,
         max_rounds, max_voices_per_round, paid_run_budget)
    VALUES ('00000000-0000-0000-0000-0000000e00f1', '00000000-0000-0000-0000-0000000e0001',
            'team-a/proj-x', '00000000-0000-0000-0000-0000000e00aa', 'henrik',
            '00000000-0000-0000-0000-0000000e0002', 3, 3, 12);
    SELECT round, paid_runs_used, phase, started_by INTO r, u, st, sb
      FROM discussion.party_session WHERE id = '00000000-0000-0000-0000-0000000e00f1';
    ASSERT r = 0,            format('expected round=0, found %s', r);
    ASSERT u = 0,            format('expected paid_runs_used=0, found %s', u);
    ASSERT st = 'active',    format('expected phase=active, found %s', st);
    ASSERT sb = 'henrik',    format('expected started_by=henrik, found %s', sb);
END $$;

-- (3) At most ONE active session per thread: a second active session on the same thread conflicts on
--     the partial UNIQUE (opt-in integrity — no second paid debate, §5.2).
DO $$
DECLARE failed boolean := false;
BEGIN
    BEGIN
        INSERT INTO discussion.party_session
            (thread_id, project_id, team_id, started_by, topic_message_id,
             max_rounds, max_voices_per_round, paid_run_budget)
        VALUES ('00000000-0000-0000-0000-0000000e0001', 'team-a/proj-x',
                '00000000-0000-0000-0000-0000000e00aa', 'henrik',
                '00000000-0000-0000-0000-0000000e0002', 3, 3, 12);
    EXCEPTION WHEN unique_violation THEN
        failed := true;
    END;
    ASSERT failed, 'expected a second ACTIVE session on the same thread to violate uq_party_session_active_thread';
END $$;

-- (4) The partial UNIQUE is scoped to active: closing the first session frees the thread for a new one.
DO $$
DECLARE cnt int;
BEGIN
    UPDATE discussion.party_session
       SET phase = 'closed', closed_at = now()
     WHERE id = '00000000-0000-0000-0000-0000000e00f1';
    INSERT INTO discussion.party_session
        (thread_id, project_id, team_id, started_by, topic_message_id,
         max_rounds, max_voices_per_round, paid_run_budget)
    VALUES ('00000000-0000-0000-0000-0000000e0001', 'team-a/proj-x',
            '00000000-0000-0000-0000-0000000e00aa', 'henrik',
            '00000000-0000-0000-0000-0000000e0002', 3, 3, 12);
    SELECT count(*) INTO cnt FROM discussion.party_session
     WHERE thread_id = '00000000-0000-0000-0000-0000000e0001' AND phase = 'active';
    ASSERT cnt = 1, format('expected exactly 1 active session after reopen, found %s', cnt);
END $$;

-- (5) Budget CHECKs fail closed: a non-positive cap is rejected (a 0-round/0-voice debate is a no-op).
DO $$
DECLARE failed boolean := false;
BEGIN
    BEGIN
        INSERT INTO discussion.party_session
            (thread_id, project_id, team_id, started_by, topic_message_id,
             max_rounds, max_voices_per_round, paid_run_budget)
        VALUES ('00000000-0000-0000-0000-0000000e0001', 'team-a/proj-x',
                '00000000-0000-0000-0000-0000000e00aa', 'henrik',
                '00000000-0000-0000-0000-0000000e0002', 0, 3, 12);
    EXCEPTION WHEN check_violation THEN
        failed := true;
    END;
    ASSERT failed, 'expected max_rounds=0 to violate party_session_budgets_positive';
END $$;

-- (6) The phase CHECK rejects an unknown lifecycle value (and, by exclusion, any smuggled custody
--     token — the fence carve-out is that phase is session-lifecycle only.
DO $$
DECLARE failed boolean := false;
BEGIN
    BEGIN
        UPDATE discussion.party_session
           SET phase = 'claimed'
         WHERE id = '00000000-0000-0000-0000-0000000e00f1';
    EXCEPTION WHEN check_violation THEN
        failed := true;
    END;
    ASSERT failed, 'expected phase=claimed to violate the party_session phase CHECK (session lifecycle only)';
END $$;

ROLLBACK;
