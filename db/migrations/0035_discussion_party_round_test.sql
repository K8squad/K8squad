-- 0035_discussion_party_round_test.sql — runnable self-check for ISI-5616 (ADR-0027 §3.3 / addendum).
--
-- Same no-framework discipline as 0033/0034: plain SQL that fails loudly if the party-round ledger or its
-- CHECK/PK/FK contracts break. Runs AFTER the full migration set, inside one transaction ROLLED BACK at the
-- end:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f db/migrations/*.sql \
--                                              -f db/migrations/0035_discussion_party_round_test.sql

BEGIN;

-- Fixtures: a thread + party_start opener + an active session + a facilitator round message to link
-- (shapes mirror 0004/0027/0033/0034).
INSERT INTO discussion.thread (id, project_id, team_id, title, created_by)
VALUES ('00000000-0000-0000-0000-00000010a001', 'team-a/proj-x',
        '00000000-0000-0000-0000-00000010a0aa', 'party-round fixture', 'tester');
INSERT INTO discussion.message (id, thread_id, author_principal, body, audience, kind)
VALUES ('00000000-0000-0000-0000-00000010a002', '00000000-0000-0000-0000-00000010a001',
        'henrik', 'Let''s debate storage', 'party', 'party_start');
INSERT INTO discussion.message (id, thread_id, author_principal, author_agent_id, body, audience, kind)
VALUES ('00000000-0000-0000-0000-00000010a003', '00000000-0000-0000-0000-00000010a001',
        'winston', 'winston', 'Round 1 — @sam @riley weigh in', 'party', 'text');
INSERT INTO discussion.party_session
    (id, thread_id, project_id, team_id, started_by, topic_message_id,
     max_rounds, max_voices_per_round, paid_run_budget)
VALUES ('00000000-0000-0000-0000-00000010a0f1', '00000000-0000-0000-0000-00000010a001',
        'team-a/proj-x', '00000000-0000-0000-0000-00000010a0aa', 'henrik',
        '00000000-0000-0000-0000-00000010a002', 3, 3, 12);

-- (1) A round row inserts with honest defaults (kind='round') and round-trips its facilitator-message link.
DO $$
DECLARE k text; fmid uuid;
BEGIN
    INSERT INTO discussion.party_round (session_id, round, facilitator_message_id)
    VALUES ('00000000-0000-0000-0000-00000010a0f1', 1, '00000000-0000-0000-0000-00000010a003');
    SELECT kind, facilitator_message_id INTO k, fmid
      FROM discussion.party_round
     WHERE session_id = '00000000-0000-0000-0000-00000010a0f1' AND round = 1;
    ASSERT k = 'round',                                          format('expected kind=round, found %s', k);
    ASSERT fmid = '00000000-0000-0000-0000-00000010a003',        'expected the facilitator message linked';
END $$;

-- (2) PRIMARY KEY (session_id, round) makes the per-round stamp idempotent: a second INSERT of the same
--     round conflicts (the SetRoundFacilitatorMessage first-writer-wins guard, ADR-0027 §3.3).
DO $$
DECLARE failed boolean := false;
BEGIN
    BEGIN
        INSERT INTO discussion.party_round (session_id, round, facilitator_message_id)
        VALUES ('00000000-0000-0000-0000-00000010a0f1', 1, '00000000-0000-0000-0000-00000010a002');
    EXCEPTION WHEN unique_violation THEN
        failed := true;
    END;
    ASSERT failed, 'expected a second party_round for the same (session, round) to violate the PK';
END $$;

-- (3) The facilitator_message_id FK to discussion.message holds (a dangling link is rejected).
DO $$
DECLARE failed boolean := false;
BEGIN
    BEGIN
        INSERT INTO discussion.party_round (session_id, round, facilitator_message_id)
        VALUES ('00000000-0000-0000-0000-00000010a0f1', 2, '00000000-0000-0000-0000-0000deadbeef');
    EXCEPTION WHEN foreign_key_violation THEN
        failed := true;
    END;
    ASSERT failed, 'expected a dangling facilitator_message_id to violate the FK';
END $$;

-- (4) The kind CHECK rejects an unknown round kind (by exclusion, any smuggled custody token — the fence
--     carve-out is that kind is a round-type label, never a work-item custody-state).
DO $$
DECLARE failed boolean := false;
BEGIN
    BEGIN
        INSERT INTO discussion.party_round (session_id, round, facilitator_message_id, kind)
        VALUES ('00000000-0000-0000-0000-00000010a0f1', 3, '00000000-0000-0000-0000-00000010a003', 'claimed');
    EXCEPTION WHEN check_violation THEN
        failed := true;
    END;
    ASSERT failed, 'expected kind=claimed to violate the party_round kind CHECK';
END $$;

-- (5) round > 0: a 0/negative round is a nonsense mint (rounds are 1-based).
DO $$
DECLARE failed boolean := false;
BEGIN
    BEGIN
        INSERT INTO discussion.party_round (session_id, round, facilitator_message_id)
        VALUES ('00000000-0000-0000-0000-00000010a0f1', 0, '00000000-0000-0000-0000-00000010a003');
    EXCEPTION WHEN check_violation THEN
        failed := true;
    END;
    ASSERT failed, 'expected round=0 to violate party_round_round_positive';
END $$;

ROLLBACK;
