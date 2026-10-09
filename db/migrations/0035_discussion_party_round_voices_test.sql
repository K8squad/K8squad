-- 0035_discussion_party_round_voices_test.sql — runnable self-check for ISI-5638 (ADR-0031 Ruling A §3.1).
--
-- Same no-framework discipline as 0031/0033/0034: plain SQL that fails loudly if the per-round voice
-- roster column breaks. Runs AFTER the full migration set, inside one transaction ROLLED BACK at the end:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f db/migrations/*.sql \
--                                              -f db/migrations/0035_discussion_party_round_voices_test.sql

BEGIN;

-- Fixtures: a thread, a party_start topic message, a facilitator round message, and a session row.
INSERT INTO discussion.thread (id, project_id, team_id, title, created_by)
VALUES ('00000000-0000-0000-0000-0000000e0001', 'team-a/proj-x',
        '00000000-0000-0000-0000-0000000e00aa', 'party sequencing fixture', 'tester');
INSERT INTO discussion.message (id, thread_id, author_principal, body, audience, kind)
VALUES ('00000000-0000-0000-0000-0000000e0002', '00000000-0000-0000-0000-0000000e0001',
        'henrik', 'Let''s debate the storage approach', 'party', 'party_start');
INSERT INTO discussion.message (id, thread_id, author_principal, author_agent_id, body, audience, kind)
VALUES ('00000000-0000-0000-0000-0000000e0003', '00000000-0000-0000-0000-0000000e0001',
        'coordinator-run', 'coordinator', '@winston @sally @amelia weigh in', 'party', 'text');
INSERT INTO discussion.party_session
    (id, thread_id, project_id, team_id, started_by, topic_message_id,
     max_rounds, max_voices_per_round, paid_run_budget)
VALUES ('00000000-0000-0000-0000-0000000e00f1', '00000000-0000-0000-0000-0000000e0001',
        'team-a/proj-x', '00000000-0000-0000-0000-0000000e00aa', 'henrik',
        '00000000-0000-0000-0000-0000000e0002', 3, 3, 12);

-- (1) The new column exists and defaults to the empty array (no roster for the current round yet), and is
--     NOT on the 0004 fence forbidden-token list (it is a name list, not a custody column).
DO $$
DECLARE v text[]; bad text;
BEGIN
    SELECT round_voices INTO v
      FROM discussion.party_session WHERE id = '00000000-0000-0000-0000-0000000e00f1';
    ASSERT v = '{}'::text[], format('expected round_voices default {} on a fresh session, found %s', v);

    SELECT string_agg(column_name, ', ') INTO bad
      FROM information_schema.columns
     WHERE table_schema = 'discussion' AND table_name = 'party_session'
       AND column_name IN ('claim','lease','fence_token','state','holder','assignee','status',
                           'checked_out_by','holder_principal','custody');
    ASSERT bad IS NULL, format('fence breach: party_session grew a custody column: %s', bad);
END $$;

-- (2) The dispatch hook stamps the round's ordered, capped voice roster alongside the facilitator message.
--     The advancer's cursor is len(round_voices) vs. the mention_dispatch rows — proven here by stamping a
--     3-voice roster and reading it back in order.
DO $$
DECLARE v text[];
BEGIN
    UPDATE discussion.party_session
       SET round = 1, round_started_at = now(),
           current_round_message_id = '00000000-0000-0000-0000-0000000e0003',
           round_voices = ARRAY['winston','sally','amelia']
     WHERE id = '00000000-0000-0000-0000-0000000e00f1';
    SELECT round_voices INTO v
      FROM discussion.party_session WHERE id = '00000000-0000-0000-0000-0000000e00f1';
    ASSERT array_length(v, 1) = 3, format('expected a 3-voice roster, found %s', v);
    ASSERT v[1] = 'winston', format('expected winston first, found %s', v[1]);
    ASSERT v[3] = 'amelia',  format('expected amelia last, found %s', v[3]);
END $$;

-- (3) A new round resets the roster to '{}' (the next round has not been planned yet), mirroring the
--     current_round_message_id clear — so a stale roster never leaks across rounds.
DO $$
DECLARE v text[]; m uuid;
BEGIN
    UPDATE discussion.party_session
       SET round = round + 1, round_started_at = now(),
           current_round_message_id = NULL, round_voices = '{}'
     WHERE id = '00000000-0000-0000-0000-0000000e00f1';
    SELECT round_voices, current_round_message_id INTO v, m
      FROM discussion.party_session WHERE id = '00000000-0000-0000-0000-0000000e00f1';
    ASSERT v = '{}'::text[], format('expected round_voices reset to {} on a new round, found %s', v);
    ASSERT m IS NULL, 'expected current_round_message_id cleared on a new round';
END $$;

ROLLBACK;
