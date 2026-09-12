-- 0016_work_item_touch_on_change_test.sql — runnable self-check for ISI-4217.
--
-- No framework, no fixture: plain SQL that fails loudly if the no-op-revision
-- discipline breaks. Runs AFTER the full migration set (0001..0016 in filename
-- order), inside one transaction ROLLED BACK at the end:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f db/migrations/*.sql \
--                                              -f db/migrations/0016_work_item_touch_on_change_test.sql
--
-- It proves the invariant the retry-lap dispatch wedge (ISI-4217, live on
-- k8squad-test) broke against: a no-op custody write must NOT mint a work-item
-- revision, while a REAL edit still must. The Go-level round-trip (pin →
-- RetryEnter → re-acquire → pin resolves) is exercised by the chaos gate's D6
-- case (pkg/controller/rundrive/chaos_test.go).

BEGIN;

-- (1) STRUCTURAL: the shipped trigger function still carries the no-op guard
-- AND the generated-column exclusion (ISI-4298). If someone reverts the
-- function to the 0001 unconditional shape (or replaces it with something
-- that cannot compare whole rows), every assert below would still pass
-- vacuously only until the next no-op write — this fails first. The exclusion
-- assert fires even on a schema WITHOUT 0012 applied: the plain whole-row
-- `NEW IS DISTINCT FROM OLD` shape passes block (2) vacuously there (no
-- generated column ⇒ NEW/OLD match), which is exactly how 98eb5b8 verified
-- green locally while being defeated live — so the exclusion is asserted on
-- the function TEXT, not the behavior.
DO $$
DECLARE def text;
BEGIN
    SELECT pg_get_functiondef('coord.touch_updated_at()'::regprocedure) INTO def;
    ASSERT def IS NOT NULL, 'coord.touch_updated_at() missing entirely — 0001 regressed';
    ASSERT def ~* 'IS\s+DISTINCT\s+FROM',
        format('coord.touch_updated_at() lost the NEW/OLD no-op guard (ISI-4217): %s', coalesce(def, '<none>'));
    ASSERT def ~* '-\s*''search_tsv''',
        format('coord.touch_updated_at() does not exclude the 0012 GENERATED column search_tsv from the row compare (ISI-4298) — the whole-row guard is defeated by it on any schema with 0012 applied: %s', coalesce(def, '<none>'));
END $$;

-- (2) THE WEDGE PRECONDITION: the exact ISI-4183 re-acquire mark shape over an
-- already-claimed lane is a no-op VALUE write — the revision must NOT move.
-- updated_at is staged one hour in the past so "preserved" is an exact
-- comparison against a constant, not a frozen-now() artifact (now() is
-- transaction-frozen; clock_timestamp() is live wall clock).
DO $$
DECLARE wid uuid; staged timestamptz := clock_timestamp() - interval '1 hour'; n bigint;
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by, state, updated_at)
    VALUES (gen_random_uuid(), 'no-op mark self-check', 'tester', 'in_progress', staged)
    RETURNING id INTO wid;

    UPDATE coord.work_item
       SET state = 'in_progress'
     WHERE id = wid AND state IN ('todo', 'in_progress');
    GET DIAGNOSTICS n = ROW_COUNT;

    ASSERT n = 1, format('no-op mark matched %s rows, want 1 (the n==1 lane-guard contract)', n);
    ASSERT (SELECT updated_at FROM coord.work_item WHERE id = wid) = staged,
        'no-op re-acquire mark moved updated_at — phantom revision, the ISI-4217 dispatch wedge is back';
END $$;

-- (3) Real edits still bump: the revision pin keeps its teeth for actual
-- content changes (title here; acceptance_criteria/goals ride the same
-- whole-row trigger, 0014).
DO $$
DECLARE wid uuid; staged timestamptz := clock_timestamp() - interval '1 hour';
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by, state, updated_at)
    VALUES (gen_random_uuid(), 'real-edit self-check', 'tester', 'in_progress', staged)
    RETURNING id INTO wid;

    UPDATE coord.work_item SET title = 'real-edit self-check (edited)' WHERE id = wid;
    ASSERT (SELECT updated_at FROM coord.work_item WHERE id = wid) > staged,
        'real edit did not bump updated_at — WorkItemRevision pin broken (stale resume, AC3)';

    UPDATE coord.work_item SET acceptance_criteria = ARRAY['pin still bumps'] WHERE id = wid;
    ASSERT (SELECT updated_at FROM coord.work_item WHERE id = wid) > staged,
        'acceptance_criteria edit did not bump updated_at — 0014 revision discipline broken';
END $$;

-- (4) First-acquire shape still bumps: todo → in_progress is a REAL lane move
-- (a lifecycle transition, not custody bookkeeping) and must mint a revision.
DO $$
DECLARE wid uuid; staged timestamptz := clock_timestamp() - interval '1 hour';
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by, state, updated_at)
    VALUES (gen_random_uuid(), 'fresh-acquire self-check', 'tester', 'todo', staged)
    RETURNING id INTO wid;

    UPDATE coord.work_item
       SET state = 'in_progress'
     WHERE id = wid AND state IN ('todo', 'in_progress');
    ASSERT (SELECT updated_at FROM coord.work_item WHERE id = wid) > staged,
        'todo → in_progress lane advance did not bump updated_at';
END $$;

-- (5) The mark's lane guard still rejects non-claimable lanes (rollback
-- semantics, ISI-4183): backlog / in_review / done match 0 rows.
DO $$
DECLARE wid uuid; n bigint;
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by, state)
    VALUES (gen_random_uuid(), 'terminal-lane guard self-check', 'tester', 'done')
    RETURNING id INTO wid;

    UPDATE coord.work_item
       SET state = 'in_progress'
     WHERE id = wid AND state IN ('todo', 'in_progress');
    GET DIAGNOSTICS n = ROW_COUNT;
    ASSERT n = 0, format('mark matched a done item (%s rows, want 0) — lane guard broken', n);
END $$;

-- (6) Shared-function blast radius (0009): coord.rate_limit_reroute rides the
-- same function. A no-op episode rewrite mints no revision; a real episode
-- rewrite (attempt/resume_at move) still bumps.
DO $$
DECLARE wid uuid; staged timestamptz := clock_timestamp() - interval '1 hour';
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by, state, updated_at)
    VALUES (gen_random_uuid(), 'reroute touch self-check', 'tester', 'todo', staged)
    RETURNING id INTO wid;

    INSERT INTO coord.rate_limit_reroute
        (work_item_id, throttled_credential, attempt, resume_at,
         released_fence, released_run, coordinator, updated_at)
    VALUES (wid, 'cred:test', 1, clock_timestamp() + interval '30 seconds', 7,
            gen_random_uuid(), 'principal:coordinator', staged);

    UPDATE coord.rate_limit_reroute SET attempt = 1 WHERE work_item_id = wid;
    ASSERT (SELECT updated_at FROM coord.rate_limit_reroute WHERE work_item_id = wid) = staged,
        'no-op reroute rewrite moved updated_at — shared-function suppression regressed';

    UPDATE coord.rate_limit_reroute
       SET attempt = 2, resume_at = clock_timestamp() + interval '60 seconds'
     WHERE work_item_id = wid;
    ASSERT (SELECT updated_at FROM coord.rate_limit_reroute WHERE work_item_id = wid) > staged,
        'real reroute episode rewrite did not bump updated_at';
END $$;

ROLLBACK;
