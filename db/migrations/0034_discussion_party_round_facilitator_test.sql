-- 0034_discussion_party_round_facilitator_test.sql — runnable self-check for ISI-5615 (ADR-0027 §3.2/§3.3).
--
-- Same no-framework discipline as 0031/0033: plain SQL that fails loudly if the per-round facilitator
-- link breaks. Runs AFTER the full migration set, inside one transaction ROLLED BACK at the end:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f db/migrations/*.sql \
--                                              -f db/migrations/0034_discussion_party_round_facilitator_test.sql

BEGIN;

-- Fixtures: a thread, a party_start topic message, a facilitator round message, and a session row.
INSERT INTO discussion.thread (id, project_id, team_id, title, created_by)
VALUES ('00000000-0000-0000-0000-0000000f0001', 'team-a/proj-x',
        '00000000-0000-0000-0000-0000000f00aa', 'party round fixture', 'tester');
INSERT INTO discussion.message (id, thread_id, author_principal, body, audience, kind)
VALUES ('00000000-0000-0000-0000-0000000f0002', '00000000-0000-0000-0000-0000000f0001',
        'henrik', 'Let''s debate the storage approach', 'party', 'party_start');
INSERT INTO discussion.message (id, thread_id, author_principal, author_agent_id, body, audience, kind)
VALUES ('00000000-0000-0000-0000-0000000f0003', '00000000-0000-0000-0000-0000000f0001',
        'coordinator-run', 'coordinator', '@winston @sally what do you think?', 'party', 'text');
INSERT INTO discussion.party_session
    (id, thread_id, project_id, team_id, started_by, topic_message_id,
     max_rounds, max_voices_per_round, paid_run_budget)
VALUES ('00000000-0000-0000-0000-0000000f00f1', '00000000-0000-0000-0000-0000000f0001',
        'team-a/proj-x', '00000000-0000-0000-0000-0000000f00aa', 'henrik',
        '00000000-0000-0000-0000-0000000f0002', 3, 3, 12);

-- (1) The new columns exist, default to NULL (no facilitator message for the current round yet), and
--     are NOT on the 0004 fence forbidden-token list (they are a message ref + a timestamp).
DO $$
DECLARE m uuid; ts timestamptz; bad text;
BEGIN
    SELECT current_round_message_id, round_started_at INTO m, ts
      FROM discussion.party_session WHERE id = '00000000-0000-0000-0000-0000000f00f1';
    ASSERT m IS NULL,  format('expected current_round_message_id NULL on a fresh session, found %s', m);
    ASSERT ts IS NULL, format('expected round_started_at NULL on a fresh session, found %s', ts);

    SELECT string_agg(column_name, ', ') INTO bad
      FROM information_schema.columns
     WHERE table_schema = 'discussion' AND table_name = 'party_session'
       AND column_name IN ('claim','lease','fence_token','state','holder','assignee','status',
                           'checked_out_by','holder_principal','custody');
    ASSERT bad IS NULL, format('fence breach: party_session grew a custody column: %s', bad);
END $$;

-- (2) The advancer stamps the current round's facilitator message + mint time in place (mutable row).
DO $$
DECLARE m uuid; ts timestamptz;
BEGIN
    UPDATE discussion.party_session
       SET round = 1, round_started_at = now(), current_round_message_id = '00000000-0000-0000-0000-0000000f0003'
     WHERE id = '00000000-0000-0000-0000-0000000f00f1';
    SELECT current_round_message_id, round_started_at INTO m, ts
      FROM discussion.party_session WHERE id = '00000000-0000-0000-0000-0000000f00f1';
    ASSERT m = '00000000-0000-0000-0000-0000000f0003', format('expected facilitator message stamped, found %s', m);
    ASSERT ts IS NOT NULL, 'expected round_started_at stamped';
END $$;

-- (3) current_round_message_id REFERENCES discussion.message — a dangling id is rejected (the link is
--     a real room message, never an opaque token).
DO $$
DECLARE failed boolean := false;
BEGIN
    BEGIN
        UPDATE discussion.party_session
           SET current_round_message_id = '00000000-0000-0000-0000-0000000fdead'
         WHERE id = '00000000-0000-0000-0000-0000000f00f1';
    EXCEPTION WHEN foreign_key_violation THEN
        failed := true;
    END;
    ASSERT failed, 'expected a dangling current_round_message_id to violate the FK to discussion.message';
END $$;

ROLLBACK;
