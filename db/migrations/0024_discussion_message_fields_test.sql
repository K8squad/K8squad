-- 0024_discussion_message_fields_test.sql — runnable self-check for the v2
-- audience/kind/payload wire columns (ISI-4925). Same discipline as the 0020
-- self-check: plain SQL, no framework, run after the migrations it checks,
-- inside one transaction rolled back at the end so it leaves no residue:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 \
--          -f db/migrations/0004_discussion_schema.sql \
--          -f db/migrations/0024_discussion_message_fields.sql \
--          -f db/migrations/0024_discussion_message_fields_test.sql
--
-- The BEHAVIOURAL guarantees (Go validates enums before insert) are exercised
-- by the Go tests (internal/discussion). This file proves the *structural* AC:
-- the columns exist with honest defaults, the audience CHECK admits party and
-- direct:{agentId} only, and the kind CHECK admits every kind the wire can
-- stamp — including 'proposal' (ISI-4931 findability depends on it) — while
-- rejecting a value outside the enum. Pins the PR #627 review finding that the
-- kind CHECK originally omitted 'proposal' (SQLSTATE 23514 on a legal write).

BEGIN;

-- A thread to hang messages off (tenancy NOT NULL columns satisfied).
CREATE TEMP TABLE t_ctx AS
WITH th AS (
    INSERT INTO discussion.thread (project_id, team_id, title, created_by)
         VALUES (gen_random_uuid(), gen_random_uuid(), 'v2 self-check room', 'principal:test')
      RETURNING id
)
SELECT id FROM th;

-- (1) The columns exist and default to the honest v1 shape on a fresh message:
--     party audience, text kind, NULL payload.
DO $$
DECLARE mid uuid;
BEGIN
    INSERT INTO discussion.message (thread_id, author_principal, body)
         VALUES ((SELECT id FROM t_ctx), 'principal:test', 'defaults message')
      RETURNING id INTO mid;
    ASSERT (SELECT audience FROM discussion.message WHERE id = mid) = 'party',
        'a fresh message must default to audience ''party''';
    ASSERT (SELECT kind FROM discussion.message WHERE id = mid) = 'text',
        'a fresh message must default to kind ''text''';
    ASSERT (SELECT payload IS NULL FROM discussion.message WHERE id = mid),
        'a fresh message must carry payload NULL';
END $$;

-- (2) Every kind the wire can stamp is accepted — including 'proposal'
--     (guards against a typo'd / incomplete CHECK list; the PR #627 blocker).
DO $$
BEGIN
    INSERT INTO discussion.message (thread_id, author_principal, body, kind)
         VALUES ((SELECT id FROM t_ctx), 'principal:test', 'k-structured', 'structured'),
                ((SELECT id FROM t_ctx), 'principal:test', 'k-task',      'task'),
                ((SELECT id FROM t_ctx), 'principal:test', 'k-decision',  'decision'),
                ((SELECT id FROM t_ctx), 'principal:test', 'k-vote',      'vote'),
                ((SELECT id FROM t_ctx), 'principal:test', 'k-proposal',  'proposal');
END $$;

-- (3) The kind CHECK rejects a value outside the enum.
DO $$
BEGIN
    BEGIN
        INSERT INTO discussion.message (thread_id, author_principal, body, kind)
             VALUES ((SELECT id FROM t_ctx), 'principal:test', 'bad kind', 'missive');
        ASSERT false, 'kind CHECK must reject a value outside the enum';
    EXCEPTION WHEN check_violation THEN
        NULL; -- expected
    END;
END $$;

-- (4) The audience CHECK admits 'party' and 'direct:{agentId}' (any target
--     shape — the principal/agent split is the read predicate''s job, R2).
DO $$
BEGIN
    INSERT INTO discussion.message (thread_id, author_principal, body, audience)
         VALUES ((SELECT id FROM t_ctx), 'principal:test', 'a-party',   'party'),
                ((SELECT id FROM t_ctx), 'principal:test', 'a-direct',  'direct:agent:auditor');
END $$;

-- (5) The audience CHECK rejects anything that is neither party nor direct:*.
DO $$
BEGIN
    BEGIN
        INSERT INTO discussion.message (thread_id, author_principal, body, audience)
             VALUES ((SELECT id FROM t_ctx), 'principal:test', 'bad audience', 'teamroom');
        ASSERT false, 'audience CHECK must reject a value outside party/direct:*';
    EXCEPTION WHEN check_violation THEN
        NULL; -- expected
    END;
END $$;

-- (6) A proposal wire row round-trips: kind 'proposal' plus a jsonb payload
--     survives the write and reads back verbatim (the ISI-4931 index input).
DO $$
DECLARE mid uuid;
BEGIN
    INSERT INTO discussion.message (thread_id, author_principal, body, audience, kind, payload)
         VALUES ((SELECT id FROM t_ctx), 'agent:planner', 'propose we file the hardening ticket',
                 'party', 'proposal', '{"action":"create_ticket","title":"Quarantine"}'::jsonb)
      RETURNING id INTO mid;
    ASSERT (SELECT kind FROM discussion.message WHERE id = mid) = 'proposal',
        'kind ''proposal'' must be stampable';
    ASSERT (SELECT payload->>'action' FROM discussion.message WHERE id = mid) = 'create_ticket',
        'payload must round-trip as jsonb';
END $$;

ROLLBACK;
