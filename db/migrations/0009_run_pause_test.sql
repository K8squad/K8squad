-- 0009_run_pause_test.sql — runnable self-check for the production uuid-keyed
-- pause-episode table (Story 3.7 / ISI-2531, wired by ISI-2883).
--
-- Same discipline as the 0001/0005/0007 self-checks: plain SQL, no framework,
-- run after the migrations it checks, inside one transaction ROLLED BACK at the
-- end so it leaves no residue:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 \
--          -f db/migrations/0001_coord_schema.sql \
--          -f db/migrations/0002_coord_dispatch.sql \
--          -f db/migrations/0003_coord_outbox.sql \
--          -f db/migrations/0005_reconcile_step.sql \
--          -f db/migrations/0007_reconcile_effects.sql \
--          -f db/migrations/0009_run_pause.sql \
--          -f db/migrations/0009_run_pause_test.sql
--
-- The BEHAVIOURAL guarantees (single durable wake, crash-safe re-derivation,
-- exactly-once resume claim, escalation/backoff policy) are exercised by the Go
-- gates (pkg/coord/resume_test.go + resumeprod_chaos_test.go, -tags=chaos). This
-- file proves the *structural* AC of the schema this story adds.

BEGIN;

-- (1) coord.run_pause exists keyed by the uuid work item (one live episode per
--     work item, rewritten in place) with the custody + schedule columns.
DO $$
DECLARE cols int; pk_type text; fk_ok boolean; pend_idx int;
BEGIN
    SELECT count(*) INTO cols
      FROM information_schema.columns
     WHERE table_schema = 'coord' AND table_name = 'run_pause'
       AND column_name IN ('work_item_id','run_id','reason','retry_after_ms',
                           'attempt','resume_at','paused_at','resumed_at');
    IF cols <> 8 THEN
        RAISE EXCEPTION 'coord.run_pause must carry the 8 episode columns, found %', cols;
    END IF;

    SELECT c.data_type INTO pk_type
      FROM information_schema.table_constraints tc
      JOIN information_schema.key_column_usage k
        ON k.constraint_name = tc.constraint_name
       AND k.table_schema = tc.table_schema
      JOIN information_schema.columns c
        ON c.table_schema = k.table_schema
       AND c.table_name = k.table_name
       AND c.column_name = k.column_name
     WHERE tc.table_schema='coord' AND tc.table_name='run_pause'
       AND tc.constraint_type='PRIMARY KEY'
       AND k.column_name='work_item_id';
    IF pk_type IS DISTINCT FROM 'uuid' THEN
        RAISE EXCEPTION 'coord.run_pause PK on work_item_id must be uuid, found %', pk_type;
    END IF;

    SELECT EXISTS (
        SELECT 1 FROM information_schema.table_constraints
         WHERE table_schema='coord' AND table_name='run_pause'
           AND constraint_type='FOREIGN KEY')
      INTO fk_ok;
    IF NOT fk_ok THEN
        RAISE EXCEPTION 'coord.run_pause must reference coord.work_item (episode dies with its item)';
    END IF;

    SELECT count(*) INTO pend_idx
      FROM pg_indexes
     WHERE schemaname='coord' AND tablename='run_pause' AND indexdef LIKE '%WHERE%';
    IF pend_idx < 1 THEN
        RAISE EXCEPTION 'coord.run_pause needs the partial pending-wake index (resumed_at IS NULL)';
    END IF;
END $$;

-- (2) One episode per work item, rewritten in place: the PK rejects a second
--     row for the same item; the UPSERT path (pkg/coord/resumeprod.go Pause)
--     rewrites instead.
DO $$
DECLARE wi uuid; dupe int := 0;
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by)
         VALUES (gen_random_uuid(), 'pause self-check item', 'principal:self-check')
      RETURNING id INTO wi;

    INSERT INTO coord.run_pause (work_item_id, run_id, reason, attempt,
                                 resume_at, paused_at)
    VALUES (wi, gen_random_uuid(), 'rate_limited', 1, now(), now());

    BEGIN
        INSERT INTO coord.run_pause (work_item_id, run_id, reason, attempt,
                                     resume_at, paused_at)
        VALUES (wi, gen_random_uuid(), 'rate_limited', 2, now(), now());
    EXCEPTION WHEN unique_violation THEN
        dupe := 1;
    END;
    IF dupe <> 1 THEN
        RAISE EXCEPTION 'coord.run_pause must be one-episode-per-item (PK rejected the dupe = good; no rejection here)';
    END IF;

    -- reason stays the pause family today but is a free text column: the 7.4
    -- credential reason lands in the same machinery without a migration.
    UPDATE coord.run_pause SET reason='credential' WHERE work_item_id=wi;
END $$;

ROLLBACK;
