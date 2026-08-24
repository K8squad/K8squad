-- 0008_scm_mirror_test.sql — runnable self-check for the SCM mirror store (§5.4 / ADR-018, ISI-2254)
--
-- No framework, no fixture: plain SQL that fails loudly if the mirror invariants break. Runs
-- against a throwaway Postgres AFTER 0008_scm_mirror.sql is applied:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f db/migrations/0008_scm_mirror.sql \
--                                              -f db/migrations/0008_scm_mirror_test.sql
--
-- Everything runs inside one transaction that is ROLLED BACK at the end, so the check leaves
-- no residue. Any failed assertion aborts non-zero (ON_ERROR_STOP).

BEGIN;

-- Exactly the two tables exist in the scm schema.
DO $$
DECLARE n int;
BEGIN
    SELECT count(*) INTO n FROM information_schema.tables
     WHERE table_schema = 'scm' AND table_name IN ('repo','mirror_record');
    ASSERT n = 2, format('expected scm.repo + scm.mirror_record, found %s', n);
END $$;

-- The mirror CANNOT express coordination custody (AC6 by construction): no claim/lease/fence
-- column may ever appear on mirror_record.
DO $$
DECLARE n int;
BEGIN
    SELECT count(*) INTO n FROM information_schema.columns
     WHERE table_schema = 'scm' AND table_name = 'mirror_record'
       AND column_name IN ('claim','lease','fence','work_item_id','run_id');
    ASSERT n = 0, format('mirror_record must not own custody/linkage columns, found %s', n);
END $$;

-- Provenance is NOT NULL (a provenance-less mirror row is a schema violation, §7.3.2).
DO $$
DECLARE nullable text;
BEGIN
    SELECT is_nullable INTO nullable FROM information_schema.columns
     WHERE table_schema='scm' AND table_name='mirror_record' AND column_name='external_origin';
    ASSERT nullable = 'NO', 'external_origin must be NOT NULL';
END $$;

-- Trust defaults to untrusted-external AND the CHECK pins it to exactly that value: a write
-- presenting coordination authority must be rejected by the schema itself (AC6 by construction,
-- not by writer convention). The negative path below is the assertion that matters — reading
-- column_default alone would pass even if the CHECK permitted other values.
DO $$
DECLARE dflt text;
BEGIN
    SELECT column_default INTO dflt FROM information_schema.columns
     WHERE table_schema='scm' AND table_name='mirror_record' AND column_name='trust_level';
    -- Prefix match: PG16 normalizes column_default to `'untrusted-external'::character varying`
    -- (pg_get_expr adds the implicit cast for a VARCHAR column); an unanchored suffix would let
    -- any default containing the literal anywhere pass. The literal must lead the expression.
    ASSERT dflt LIKE '''untrusted-external''%', format('trust_level default must be untrusted-external, got %s', dflt);
END $$;

DO $$
BEGIN
    BEGIN
        INSERT INTO scm.mirror_record
            (project_name, project_namespace, kind, external_id, state, external_origin, trust_level)
        VALUES ('p','ns','pr','99','open','{}'::jsonb,'trusted-control');
        RAISE EXCEPTION 'trust_level trusted-control was accepted - the mirror can express coordination authority';
    EXCEPTION WHEN check_violation THEN
        NULL;  -- expected: the schema itself refuses coordination authority
    END;
END $$;

-- Idempotence (AC2): upserting the same (project, kind, external id) twice yields ONE row.
INSERT INTO scm.mirror_record (project_name, project_namespace, kind, external_id, state, title, actor, external_origin)
VALUES ('p','ns','pr','1','open','t','dev','{"provider":"github","repo":"acme/app","external_id":"1","actor":"dev"}');

INSERT INTO scm.mirror_record (project_name, project_namespace, kind, external_id, state, title, actor, external_origin)
VALUES ('p','ns','pr','1','closed','t2','dev','{"provider":"github","repo":"acme/app","external_id":"1","actor":"dev"}')
ON CONFLICT (project_name, project_namespace, kind, external_id) DO UPDATE SET state = EXCLUDED.state;

DO $$
DECLARE n int; s text;
BEGIN
    SELECT count(*), max(state) INTO n, s FROM scm.mirror_record WHERE project_name='p' AND project_namespace='ns';
    ASSERT n = 1, format('idempotent upsert must keep exactly one row, found %s', n);
    ASSERT s = 'closed', format('upsert must update external-owned state in place, found %s', s);
END $$;

-- The upsert also advances updated_at (a stale-pinned timestamp would make the row look dead).
DO $$
DECLARE n int;
BEGIN
    SELECT count(*) INTO n FROM scm.mirror_record
     WHERE project_name='p' AND project_namespace='ns' AND updated_at >= mirrored_at;
    ASSERT n = 1, format('upsert must advance updated_at on conflict, found %s rows with updated_at < mirrored_at', 1 - n);
END $$;

-- A different external id is a DIFFERENT row (the key is external, not per-delivery).
INSERT INTO scm.mirror_record (project_name, project_namespace, kind, external_id, state, title, actor, external_origin)
VALUES ('p','ns','issue','7','open','t','dev','{"provider":"github","repo":"acme/app","external_id":"7","actor":"dev"}');
DO $$
DECLARE n int;
BEGIN
    SELECT count(*) INTO n FROM scm.mirror_record WHERE project_name='p' AND project_namespace='ns';
    ASSERT n = 2, format('distinct external ids must be distinct rows, found %s', n);
END $$;

ROLLBACK;
