-- 0014_work_item_acceptance_goals_test.sql — runnable self-check for ISI-3606.
--
-- No framework, no fixture: plain SQL that fails loudly if the AC/goals surface
-- or its revision-discipline invariant breaks. Runs AFTER the full migration set
-- (0001..000N in filename order), inside one transaction ROLLED BACK at the end:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f db/migrations/*.sql \
--                                              -f db/migrations/0014_work_item_acceptance_goals_test.sql
--
-- It proves the STRUCTURAL + revision contract both read models depend on; the
-- Go read/write wiring is exercised by pkg/coord + pkg/controller/contextsource
-- tests (the delegated child issue).

BEGIN;

-- (1) Both columns exist on coord.work_item and are Postgres arrays (text[]).
DO $$
DECLARE t text; e text;
BEGIN
    FOR e IN SELECT unnest(ARRAY['acceptance_criteria','goals']) LOOP
        SELECT data_type INTO t FROM information_schema.columns
         WHERE table_schema='coord' AND table_name='work_item' AND column_name=e;
        ASSERT t = 'ARRAY', format('expected coord.work_item.%s ARRAY, found %s', e, coalesce(t,'<absent>'));
    END LOOP;
END $$;

-- (2) NOT NULL DEFAULT '{}' — a bare insert reads back two empty lists, so every
-- pre-existing row is byte-identical to the old degraded (empty) behaviour.
DO $$
DECLARE wid uuid; ac text[]; g text[];
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by)
    VALUES (gen_random_uuid(), 'ac/goals default self-check', 'tester')
    RETURNING id INTO wid;

    SELECT acceptance_criteria, goals INTO ac, g FROM coord.work_item WHERE id = wid;
    ASSERT ac = '{}'::text[], format('acceptance_criteria default expected {}, found %s', ac);
    ASSERT g  = '{}'::text[], format('goals default expected {}, found %s', g);
END $$;

-- (3) Arrays round-trip through an UPDATE (the write path both read models feed off).
DO $$
DECLARE wid uuid; ac text[]; g text[];
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by)
    VALUES (gen_random_uuid(), 'ac/goals round-trip self-check', 'tester')
    RETURNING id INTO wid;

    UPDATE coord.work_item
       SET acceptance_criteria = ARRAY['api returns 200','audit row written'],
           goals               = ARRAY['ship the read model']
     WHERE id = wid;

    SELECT acceptance_criteria, goals INTO ac, g FROM coord.work_item WHERE id = wid;
    ASSERT ac = ARRAY['api returns 200','audit row written'],
        format('acceptance_criteria round-trip mismatch, found %s', ac);
    ASSERT g = ARRAY['ship the read model'],
        format('goals round-trip mismatch, found %s', g);
END $$;

-- (4) REVISION DISCIPLINE (the load-bearing invariant, asserted structurally):
-- AC/goals get pinned by the assembler's WorkItemRevision for free ONLY because
-- editing them is a plain UPDATE covered by the existing whole-row
-- work_item_touch_updated_at BEFORE UPDATE trigger (0001), which bumps
-- updated_at. (A per-transaction now() comparison can't show this — now() is
-- frozen per txn and this whole check is one txn; in prod each edit is its own
-- txn.) So we lock the MECHANISM instead: a BEFORE UPDATE row trigger exists on
-- coord.work_item AND is not scoped to a column subset — i.e. it fires for the
-- new acceptance_criteria/goals columns too. If someone ever narrowed it to
-- `UPDATE OF title, body`, AC/goals edits would stop bumping updated_at and
-- deterministic resume (AC3) would silently serve stale AC — this fails first.
DO $$
DECLARE n int; scoped int;
BEGIN
    SELECT count(*) INTO n FROM information_schema.triggers
     WHERE event_object_schema='coord' AND event_object_table='work_item'
       AND action_timing='BEFORE' AND event_manipulation='UPDATE';
    ASSERT n >= 1, 'no BEFORE UPDATE trigger on coord.work_item — AC/goals edits would not bump updated_at (revision pin broken)';

    -- A column-scoped trigger (UPDATE OF ...) would list its columns here; a
    -- whole-row trigger lists none. AC/goals are covered iff none are scoped.
    SELECT count(*) INTO scoped FROM information_schema.triggered_update_columns
     WHERE event_object_schema='coord' AND event_object_table='work_item';
    ASSERT scoped = 0,
        'coord.work_item has a column-scoped BEFORE UPDATE trigger — verify it covers acceptance_criteria/goals or WorkItemRevision will miss AC/goals edits';
END $$;

ROLLBACK;
