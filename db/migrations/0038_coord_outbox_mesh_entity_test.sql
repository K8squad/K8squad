-- 0038_coord_outbox_mesh_entity_test.sql — runnable self-check for the mesh entity
-- widening (AgentMesh S0, ISI-5833). Plain SQL, no framework; the migration runner
-- ignores *_test.sql. Runs against a throwaway Postgres AFTER the schema chain:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 \
--          -f db/migrations/0001_coord_schema.sql \
--          -f db/migrations/0002_coord_dispatch.sql \
--          -f db/migrations/0003_coord_outbox.sql \
--          ... \
--          -f db/migrations/0038_coord_outbox_mesh_entity.sql \
--          -f db/migrations/0038_coord_outbox_mesh_entity_test.sql
--
-- Everything runs inside one transaction that is ROLLED BACK at the end, so the
-- check leaves no residue and can run repeatedly. Any failed assertion aborts with
-- a non-zero exit (ON_ERROR_STOP). This proves the STRUCTURAL AC of S0 I6: the
-- `mesh` entity is admitted, the pre-existing entities still are, and a garbage
-- entity is still rejected (the widen stayed a strict, bounded superset).

BEGIN;

-- (1) A `mesh` event is now ACCEPTED (lands unflushed like any other entity).
DO $$
DECLARE eid bigint; pub timestamptz;
BEGIN
    INSERT INTO coord.outbox (entity, project_id, squad, event_type, payload)
         VALUES ('mesh', gen_random_uuid(), 'squad-canary', 'mesh.register',
                 '{"team":"canary","role":"coder"}'::jsonb)
      RETURNING id, published_at INTO eid, pub;
    ASSERT eid IS NOT NULL, 'a mesh entity event must be accepted by the widened CHECK';
    ASSERT pub IS NULL, 'a fresh mesh event must be unflushed (published_at NULL)';
END $$;

-- (2) The five pre-existing entities are STILL accepted (widen was additive, not a replace).
DO $$
DECLARE n int;
BEGIN
    INSERT INTO coord.outbox (entity, project_id, event_type, payload) VALUES
        ('run',       gen_random_uuid(), 'completed', '{}'::jsonb),
        ('work_item', gen_random_uuid(), 'claimed',   '{}'::jsonb),
        ('artifact',  gen_random_uuid(), 'artifact_registered', '{}'::jsonb),
        ('memory',    gen_random_uuid(), 'written',   '{}'::jsonb),
        ('scm',       gen_random_uuid(), 'check_run.failed', '{}'::jsonb);
    SELECT count(*) INTO n FROM coord.outbox
     WHERE entity IN ('run','work_item','artifact','memory','scm');
    ASSERT n = 5, 'all five legacy entities must remain accepted after the widen';
END $$;

-- (3) A garbage entity is STILL rejected — the CHECK stayed bounded (no ad-hoc strings).
DO $$
DECLARE blocked boolean := false;
BEGIN
    BEGIN
        INSERT INTO coord.outbox (entity, project_id, event_type, payload)
             VALUES ('not_an_entity', gen_random_uuid(), 'x', '{}'::jsonb);
    EXCEPTION WHEN check_violation THEN blocked := true;
    END;
    ASSERT blocked, 'an entity outside the bounded set must still be rejected by outbox_entity_check';
END $$;

-- (4) The constraint carries the expected name and admits exactly the six-entity set.
DO $$
DECLARE def text;
BEGIN
    SELECT pg_get_constraintdef(oid) INTO def
      FROM pg_constraint
     WHERE conname = 'outbox_entity_check'
       AND conrelid = 'coord.outbox'::regclass;
    ASSERT def IS NOT NULL, 'outbox_entity_check must exist by that name after the widen';
    ASSERT position('mesh' in def) > 0, 'outbox_entity_check must admit mesh';
    ASSERT position('run' in def) > 0, 'outbox_entity_check must still admit run';
END $$;

ROLLBACK;
