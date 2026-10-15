-- 0040_coord_mesh_endpoint_test.sql — runnable self-check for ISI-5822 (AgentMesh P1.1 registry).
--
-- Same no-framework discipline as 0015/0029/0037: plain SQL that fails loudly if the mesh endpoint
-- registry or its contract breaks. Runs AFTER the full migration set, inside one transaction
-- ROLLED BACK at the end (leaves no residue).

BEGIN;

-- (1) The table exists in the coord schema with the required columns/types/nullability.
DO $$
DECLARE t text; n text;
BEGIN
    SELECT data_type INTO t FROM information_schema.columns
     WHERE table_schema='coord' AND table_name='mesh_endpoint' AND column_name='endpoint_id';
    ASSERT t = 'text', format('expected coord.mesh_endpoint.endpoint_id text, found %s', coalesce(t,'<absent>'));

    SELECT data_type INTO t FROM information_schema.columns
     WHERE table_schema='coord' AND table_name='mesh_endpoint' AND column_name='agent_card';
    ASSERT t = 'jsonb', format('expected coord.mesh_endpoint.agent_card jsonb, found %s', coalesce(t,'<absent>'));

    SELECT is_nullable INTO n FROM information_schema.columns
     WHERE table_schema='coord' AND table_name='mesh_endpoint' AND column_name='team';
    ASSERT n = 'NO', format('expected team NOT NULL, found is_nullable=%s', coalesce(n,'<absent>'));
    SELECT is_nullable INTO n FROM information_schema.columns
     WHERE table_schema='coord' AND table_name='mesh_endpoint' AND column_name='last_seen_at';
    ASSERT n = 'NO', format('expected last_seen_at NOT NULL, found is_nullable=%s', coalesce(n,'<absent>'));
END $$;

-- (2) Register → heartbeat → lookup round-trip; a re-register upserts (never shadows), and the
--     endpoint UNIQUE constraint refuses a second endpoint_id squatting the same address.
DO $$
DECLARE cnt int; seen timestamptz;
BEGIN
    INSERT INTO coord.mesh_endpoint (endpoint_id, team, role, pod, endpoint, agent_card)
    VALUES ('opencode-0-1', 'team-acme', 'coder', 'pool-aaa', 'http://10.0.0.1:8081',
            '{"agent":{"name":"coder","squad":"team-acme"}}'::jsonb);

    UPDATE coord.mesh_endpoint SET last_seen_at = now() + interval '5 seconds'
     WHERE endpoint_id = 'opencode-0-1';
    SELECT last_seen_at INTO seen FROM coord.mesh_endpoint WHERE endpoint_id = 'opencode-0-1';
    ASSERT seen > now(), 'heartbeat should refresh last_seen_at';

    -- Upsert on the same endpoint_id must not create a second row.
    INSERT INTO coord.mesh_endpoint (endpoint_id, team, role, pod, endpoint, agent_card)
    VALUES ('opencode-0-1', 'team-acme', 'coder', 'pool-aaa', 'http://10.0.0.1:8081',
            '{"agent":{"name":"coder","squad":"team-acme"}}'::jsonb)
    ON CONFLICT (endpoint_id) DO UPDATE
        SET last_seen_at = now(), agent_card = EXCLUDED.agent_card;
    SELECT count(*) INTO cnt FROM coord.mesh_endpoint WHERE pod = 'pool-aaa';
    ASSERT cnt = 1, format('re-register should upsert, found %s rows', cnt);

    BEGIN
        INSERT INTO coord.mesh_endpoint (endpoint_id, team, role, pod, endpoint, agent_card)
        VALUES ('other-id', 'team-acme', 'coder', 'pool-bbb', 'http://10.0.0.1:8081', '{}'::jsonb);
        RAISE EXCEPTION 'endpoint UNIQUE constraint missing: duplicate address accepted';
    EXCEPTION
        WHEN unique_violation THEN NULL; -- expected
    END;
END $$;

-- (3) The (team, role) lookup predicate + TTL filter behave: an expired endpoint is excluded by
--     the lookup's last_seen_at predicate even before the sweeper deletes it; Deregister removes.
DO $$
DECLARE cnt int;
BEGIN
    -- A stale row: heartbeat older than any plausible TTL.
    INSERT INTO coord.mesh_endpoint (endpoint_id, team, role, pod, endpoint, agent_card, last_seen_at)
    VALUES ('opencode-0-2', 'team-acme', 'coder', 'pool-aaa', 'http://10.0.0.1:8082',
            '{}'::jsonb, now() - interval '1 hour');

    SELECT count(*) INTO cnt FROM coord.mesh_endpoint
     WHERE team='team-acme' AND role='coder' AND last_seen_at > now() - interval '90 seconds';
    ASSERT cnt = 1, format('lookup should see only the live endpoint, found %s', cnt);

    DELETE FROM coord.mesh_endpoint WHERE pod = 'pool-aaa';
    SELECT count(*) INTO cnt FROM coord.mesh_endpoint WHERE pod = 'pool-aaa';
    ASSERT cnt = 0, format('drain-by-pod deregister should remove rows, found %s', cnt);
END $$;

ROLLBACK;
