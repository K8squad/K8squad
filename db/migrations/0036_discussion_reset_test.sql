-- 0036_discussion_reset_test.sql — runnable self-check for ISI-5672 (ADR-0030 Option 2).
--
-- Same no-framework discipline as 0025/0031/0033: plain SQL that fails loudly if the reset-marker
-- surface or the kind-CHECK widening breaks. Runs AFTER the full migration set, inside one transaction
-- ROLLED BACK at the end:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f db/migrations/*.sql \
--                                              -f db/migrations/0036_discussion_reset_test.sql

BEGIN;

-- Fixtures: a thread to hang messages on. Shapes mirror 0004/0024/0033.
INSERT INTO discussion.thread (id, project_id, team_id, title, created_by)
VALUES ('00000000-0000-0000-0000-0000000f0001', 'team-a/proj-x',
        '00000000-0000-0000-0000-0000000f00aa', 'reset fixture', 'tester');

-- (1) kind='reset' is now an accepted message kind (0036 widened the CHECK). The pre-0036 kinds
--     (party_start, decision_request, proposal, text) must still be accepted — the widen is additive.
INSERT INTO discussion.message (id, thread_id, author_principal, body, audience, kind)
VALUES ('00000000-0000-0000-0000-0000000f0002', '00000000-0000-0000-0000-0000000f0001',
        'henrik', 'discussion cleared', 'party', 'reset');
INSERT INTO discussion.message (id, thread_id, author_principal, body, audience, kind)
VALUES ('00000000-0000-0000-0000-0000000f0003', '00000000-0000-0000-0000-0000000f0001',
        'henrik', 'ordinary text still accepted', 'party', 'text');
INSERT INTO discussion.message (id, thread_id, author_principal, body, audience, kind)
VALUES ('00000000-0000-0000-0000-0000000f0004', '00000000-0000-0000-0000-0000000f0001',
        'henrik', 'party start still accepted', 'party', 'party_start');

DO $$
DECLARE k text;
BEGIN
    SELECT kind INTO k FROM discussion.message WHERE id = '00000000-0000-0000-0000-0000000f0002';
    ASSERT k = 'reset', format('expected reset kind, found %s', coalesce(k, '<absent>'));
END $$;

-- (2) An unknown kind is still rejected — the widen did not loosen the CHECK to arbitrary text.
DO $$
DECLARE failed boolean := false;
BEGIN
    BEGIN
        INSERT INTO discussion.message (thread_id, author_principal, body, audience, kind)
        VALUES ('00000000-0000-0000-0000-0000000f0001', 'henrik', 'bogus', 'party', 'wipe');
    EXCEPTION WHEN check_violation THEN
        failed := true;
    END;
    ASSERT failed, 'expected kind=wipe to violate kind_must_be_text_or_extension (widen is additive, not open)';
END $$;

-- (3) The reset marker is soft-retractable like any message — the one-way invalidated_at stamp
--     (ADR-0030: retracting the marker is the undo-reset path). The append-only trigger permits exactly
--     this NULL→ts transition.
DO $$
DECLARE inv timestamptz;
BEGIN
    UPDATE discussion.message SET invalidated_at = now()
     WHERE id = '00000000-0000-0000-0000-0000000f0002';
    SELECT invalidated_at INTO inv FROM discussion.message
     WHERE id = '00000000-0000-0000-0000-0000000f0002';
    ASSERT inv IS NOT NULL, 'expected the reset marker to be soft-retractable (invalidated_at set)';
END $$;

ROLLBACK;
