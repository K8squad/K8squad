-- 0030_scm_repo_staleness_test.sql — runnable self-check for ISI-5483 (scope item 4 of ISI-5480).
--
-- Same no-framework discipline as 0029: plain SQL that fails loudly if the scm.repo staleness
-- surface or its CHECK contracts break. Runs AFTER the full migration set, inside one transaction
-- ROLLED BACK at the end:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f db/migrations/*.sql \
--                                              -f db/migrations/0030_scm_repo_staleness_test.sql

BEGIN;

-- (1) The new columns exist with the expected types and defaults.
DO $$
DECLARE t text; d text;
BEGIN
    SELECT data_type, column_default INTO t, d FROM information_schema.columns
     WHERE table_schema='scm' AND table_name='repo' AND column_name='sync_health';
    ASSERT t = 'character varying', format('expected scm.repo.sync_health varchar, found %s', coalesce(t,'<absent>'));
    ASSERT d LIKE '''unknown''%', format('expected scm.repo.sync_health default ''unknown'', found %s', coalesce(d,'<null>'));

    SELECT data_type, column_default INTO t, d FROM information_schema.columns
     WHERE table_schema='scm' AND table_name='repo' AND column_name='ttl_seconds';
    ASSERT t = 'integer', format('expected scm.repo.ttl_seconds integer, found %s', coalesce(t,'<absent>'));
    ASSERT d LIKE '0%', format('expected scm.repo.ttl_seconds default 0, found %s', coalesce(d,'<null>'));
END $$;

-- (2) A row inserted WITHOUT the new columns reads back the honest defaults (backfill-free).
DO $$
DECLARE h text; ttl integer;
BEGIN
    INSERT INTO scm.repo (project_namespace, project_name, url, provider, last_mirror_update)
    VALUES ('team-a', 'proj-x', 'https://github.com/acme/widget', 'github', now());
    SELECT sync_health, ttl_seconds INTO h, ttl
      FROM scm.repo WHERE project_namespace='team-a' AND project_name='proj-x';
    ASSERT h = 'unknown', format('pre-sync default sync_health mismatch, found %s', h);
    ASSERT ttl = 0, format('pre-sync default ttl_seconds mismatch, found %s', ttl);
END $$;

-- (3) A completed-pass anchor round-trips health + ttl through the upsert path (ON CONFLICT refresh).
DO $$
DECLARE h text; ttl integer;
BEGIN
    INSERT INTO scm.repo (project_namespace, project_name, url, provider, mirror_enabled, last_mirror_update, sync_health, ttl_seconds)
    VALUES ('team-a', 'proj-x', 'https://github.com/acme/widget', 'github', true, now(), 'healthy', 300)
    ON CONFLICT (project_name, project_namespace, url) DO UPDATE SET
        last_mirror_update = EXCLUDED.last_mirror_update,
        sync_health        = EXCLUDED.sync_health,
        ttl_seconds        = EXCLUDED.ttl_seconds,
        updated_at         = now();
    SELECT sync_health, ttl_seconds INTO h, ttl
      FROM scm.repo WHERE project_namespace='team-a' AND project_name='proj-x';
    ASSERT h = 'healthy', format('anchored sync_health mismatch, found %s', h);
    ASSERT ttl = 300, format('anchored ttl_seconds mismatch, found %s', ttl);
END $$;

-- (4) The sync_health enum rejects anything outside the known set.
DO $$
DECLARE ok boolean := false;
BEGIN
    BEGIN
        INSERT INTO scm.repo (project_namespace, project_name, url, provider, sync_health)
        VALUES ('team-b', 'proj-y', 'https://github.com/acme/other', 'github', 'exploded');
    EXCEPTION WHEN check_violation THEN
        ok := true;
    END;
    ASSERT ok, 'scm.repo.sync_health accepted a value outside the allowed set';
END $$;

-- (5) ttl_seconds rejects a negative cadence.
DO $$
DECLARE ok boolean := false;
BEGIN
    BEGIN
        INSERT INTO scm.repo (project_namespace, project_name, url, provider, ttl_seconds)
        VALUES ('team-b', 'proj-z', 'https://github.com/acme/neg', 'github', -1);
    EXCEPTION WHEN check_violation THEN
        ok := true;
    END;
    ASSERT ok, 'scm.repo.ttl_seconds accepted a negative value';
END $$;

ROLLBACK;
