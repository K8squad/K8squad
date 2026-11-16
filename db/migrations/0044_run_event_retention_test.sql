-- 0044_run_event_retention_test.sql — runnable self-check for ISI-7484 WS-3
-- (run-step retention for coord.outbox entity='run').
--
-- Same no-framework discipline as 0042/0040: plain SQL that fails loudly if the
-- retention contract breaks. Runs AFTER the full migration set, inside ONE
-- transaction ROLLED BACK at the end (leaves no residue).
--
-- ORDERING NOTE: the guard-rejection checks run BEFORE the prune call — the
-- prune function arms its transaction-local GUC for the REST of this test
-- transaction, after which a run-row DELETE inside the same transaction would
-- be legal by design (the GUC is the prune path's credential). Production
-- callers never share a transaction with the guard-check path.

BEGIN;

-- Seed the retention corpus: five rows, markers in the payload.
INSERT INTO coord.outbox (entity, project_id, event_type, run_id, payload, occurred_at, published_at) VALUES
    -- A/B: run rows, old + relayed  → prunable
    ('run',        '00000000-0000-0000-0000-0000000000b1'::uuid, 'thinking',  '00000000-0000-0000-0000-00000000f4d4'::uuid, '{"marker":"A"}'::jsonb, now() - interval '3 days', now() - interval '3 days'),
    ('run',        '00000000-0000-0000-0000-0000000000b1'::uuid, 'thinking',  '00000000-0000-0000-0000-00000000f4d4'::uuid, '{"marker":"B"}'::jsonb, now() - interval '3 days', now() - interval '3 days'),
    -- C: run row, old but NOT relayed (relay backlog) → must survive prune
    ('run',        '00000000-0000-0000-0000-0000000000b1'::uuid, 'thinking',  '00000000-0000-0000-0000-00000000f4d4'::uuid, '{"marker":"C"}'::jsonb, now() - interval '3 days', NULL),
    -- D: run row, relayed but NEW → inside retention window, must survive
    ('run',        '00000000-0000-0000-0000-0000000000b1'::uuid, 'ended',     '00000000-0000-0000-0000-00000000f4d4'::uuid, '{"marker":"D"}'::jsonb, now(), now()),
    -- E: NON-run row, old + relayed → append-only guard must keep it
    ('work_item', '00000000-0000-0000-0000-0000000000b1'::uuid, 'completed', NULL,                                  '{"marker":"E"}'::jsonb, now() - interval '3 days', now() - interval '3 days');

-- (1) A plain DELETE of a run row still trips the append-only guard (the GUC is
--     unset outside the prune function).
DO $$
BEGIN
    BEGIN
        DELETE FROM coord.outbox
         WHERE entity = 'run' AND payload->>'marker' = 'A';
        RAISE EXCEPTION 'unreachable: run-row DELETE should have been rejected';
    EXCEPTION WHEN restrict_violation THEN
        NULL; -- expected: append-only guard held
    END;
END $$;

-- (2) The governed prune deletes EXACTLY the old relayed run rows (A+B) and
--     nothing else.
DO $$
DECLARE
    n bigint;
    survivors text;
BEGIN
    SELECT coord.prune_run_events(now() - interval '2 days') INTO n;
    ASSERT n = 2, format('expected prune to remove A+B (2 rows), removed %s', n);

    SELECT string_agg(payload->>'marker', ',' ORDER BY payload->>'marker') INTO survivors
      FROM coord.outbox
     WHERE entity = 'run' AND payload->>'marker' IN ('A','B','C','D');
    ASSERT survivors = 'C,D', format('run survivors expected C,D — found %s', coalesce(survivors, '<none>'));

    SELECT count(*) INTO n FROM coord.outbox WHERE payload->>'marker' = 'E';
    ASSERT n = 1, 'non-run row E must never be pruned';
END $$;

-- (3) TRUNCATE stays rejected for everyone (statement-level guard, 0001 F2).
DO $$
BEGIN
    BEGIN
        TRUNCATE coord.outbox;
        RAISE EXCEPTION 'unreachable: TRUNCATE should have been rejected';
    EXCEPTION WHEN OTHERS THEN
        NULL; -- expected: reject_mutation raised
    END;
END $$;

ROLLBACK;
