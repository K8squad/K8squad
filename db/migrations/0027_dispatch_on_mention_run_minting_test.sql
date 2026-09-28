-- 0027_dispatch_on_mention_run_minting_test.sql — runnable self-check for the dispatch-on-mention
-- run-minting schema (ISI-5116). Same discipline as the sibling self-checks: plain SQL, no framework,
-- run after the migrations it checks, inside one transaction rolled back at the end so it leaves no
-- residue:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 \
--          -f db/migrations/0001_coord_schema.sql \
--          -f db/migrations/0004_discussion_schema.sql \
--          ... (all migrations through 0026) ... \
--          -f db/migrations/0027_dispatch_on_mention_run_minting.sql \
--          -f db/migrations/0027_dispatch_on_mention_run_minting_test.sql
--
-- Proves the STRUCTURAL ACs of ISI-5116 / ADR-0024b §4:
--   1. coord.work_item.source defaults to 'board' (existing + new board items are board-visible).
--   2. source is CHECK-constrained to the {board, discussion} allowlist (a typo cannot smuggle a
--      third, silently-board-visible source).
--   3. a discussion-sourced item round-trips (the marker the board-hide filter keys on).
--   4. discussion.mention_dispatch enforces (message_id, agent_name) idempotency (one dispatch per
--      message per agent — the second INSERT is a no-op under ON CONFLICT DO NOTHING).

BEGIN;

-- ── AC1: source defaults to 'board' ───────────────────────────────────────────────────────────────
INSERT INTO coord.work_item (id, project_id, team_id, title, created_by)
VALUES ('00000000-0000-0000-0000-0000000000a1',
        '00000000-0000-0000-0000-0000000000b1',
        '00000000-0000-0000-0000-0000000000c1',
        'board default check', 'tester');

DO $$
DECLARE got text;
BEGIN
    SELECT source INTO got FROM coord.work_item WHERE id = '00000000-0000-0000-0000-0000000000a1';
    IF got IS DISTINCT FROM 'board' THEN
        RAISE EXCEPTION 'AC1 FAILED: expected source default ''board'', got %', got;
    END IF;
END $$;

-- ── AC2: source CHECK rejects an off-allowlist value ──────────────────────────────────────────────
DO $$
BEGIN
    BEGIN
        INSERT INTO coord.work_item (id, project_id, team_id, title, created_by, source)
        VALUES ('00000000-0000-0000-0000-0000000000a2',
                '00000000-0000-0000-0000-0000000000b1',
                '00000000-0000-0000-0000-0000000000c1',
                'bad source', 'tester', 'kanban');
        RAISE EXCEPTION 'AC2 FAILED: source=''kanban'' was accepted (CHECK missing)';
    EXCEPTION WHEN check_violation THEN
        NULL; -- expected
    END;
END $$;

-- ── AC3: a discussion-sourced item round-trips ────────────────────────────────────────────────────
INSERT INTO coord.work_item (id, project_id, team_id, title, created_by, source)
VALUES ('00000000-0000-0000-0000-0000000000a3',
        '00000000-0000-0000-0000-0000000000b1',
        '00000000-0000-0000-0000-0000000000c1',
        'thread-run', 'discussion-dispatch', 'discussion');

DO $$
DECLARE got text;
BEGIN
    SELECT source INTO got FROM coord.work_item WHERE id = '00000000-0000-0000-0000-0000000000a3';
    IF got IS DISTINCT FROM 'discussion' THEN
        RAISE EXCEPTION 'AC3 FAILED: expected source ''discussion'', got %', got;
    END IF;
END $$;

-- ── AC4: mention_dispatch idempotency on (message_id, agent_name) ──────────────────────────────────
INSERT INTO discussion.mention_dispatch (message_id, agent_name, project_id, thread_id, hop_depth)
VALUES ('00000000-0000-0000-0000-0000000000d1', 'Robo-Coder',
        'ns/proj', '00000000-0000-0000-0000-0000000000e1', 1);

-- second INSERT for the SAME (message, agent) is a no-op — the idempotency guarantee.
INSERT INTO discussion.mention_dispatch (message_id, agent_name, project_id, thread_id, hop_depth)
VALUES ('00000000-0000-0000-0000-0000000000d1', 'Robo-Coder',
        'ns/proj', '00000000-0000-0000-0000-0000000000e1', 2)
ON CONFLICT (message_id, agent_name) DO NOTHING;

DO $$
DECLARE n int; hop int;
BEGIN
    SELECT count(*), max(hop_depth) INTO n, hop
      FROM discussion.mention_dispatch
     WHERE message_id = '00000000-0000-0000-0000-0000000000d1' AND agent_name = 'Robo-Coder';
    IF n <> 1 THEN
        RAISE EXCEPTION 'AC4 FAILED: expected 1 row after idempotent re-insert, got %', n;
    END IF;
    IF hop <> 1 THEN
        RAISE EXCEPTION 'AC4 FAILED: first-writer-wins violated — hop_depth should remain 1, got %', hop;
    END IF;
END $$;

-- a DIFFERENT agent on the SAME message is a distinct dispatch (fan-out to N mentioned agents).
INSERT INTO discussion.mention_dispatch (message_id, agent_name, project_id, thread_id, hop_depth)
VALUES ('00000000-0000-0000-0000-0000000000d1', 'Reviewer',
        'ns/proj', '00000000-0000-0000-0000-0000000000e1', 1);

DO $$
DECLARE n int;
BEGIN
    SELECT count(*) INTO n FROM discussion.mention_dispatch
     WHERE message_id = '00000000-0000-0000-0000-0000000000d1';
    IF n <> 2 THEN
        RAISE EXCEPTION 'AC4 FAILED: expected 2 distinct-agent rows for one message, got %', n;
    END IF;
END $$;

ROLLBACK;
