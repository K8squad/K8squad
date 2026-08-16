-- 0003_coord_outbox_test.sql — runnable self-check for the outbox (Story 12.1 / ISI-2260)
--
-- No framework, no fixture: plain SQL that fails loudly if the outbox invariants
-- break. Runs against a throwaway Postgres AFTER the schema chain is applied:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 \
--          -f db/migrations/0001_coord_schema.sql \
--          -f db/migrations/0002_coord_dispatch.sql \
--          -f db/migrations/0003_coord_outbox.sql \
--          -f db/migrations/0003_coord_outbox_test.sql
--
-- Everything runs inside one transaction that is ROLLED BACK at the end, so the
-- check leaves no residue and can run repeatedly. Any failed assertion aborts with
-- a non-zero exit (ON_ERROR_STOP). This proves the STRUCTURAL AC of Story 12.1:
-- append-only event content + set-once relay marker + unflushed-backlog visibility.
-- Live at-least-once relay/republish behaviour is the pkg/events Go suite's job.

BEGIN;

-- (1) coord.outbox exists.
DO $$
DECLARE n int;
BEGIN
    SELECT count(*) INTO n FROM information_schema.tables
     WHERE table_schema = 'coord' AND table_name = 'outbox';
    ASSERT n = 1, 'coord.outbox table must exist';
END $$;

-- (2) An inserted event lands unflushed (published_at NULL) and is visible to the
--     relay's unpublished-backlog scan.
DO $$
DECLARE eid bigint; pub timestamptz; seen int;
BEGIN
    INSERT INTO coord.outbox (entity, project_id, squad, event_type, payload)
         VALUES ('run', gen_random_uuid(), 'squad-a', 'completed', '{"run":"r1"}'::jsonb)
      RETURNING id, published_at INTO eid, pub;
    ASSERT pub IS NULL, 'a fresh outbox event must be unflushed (published_at NULL)';

    SELECT count(*) INTO seen FROM coord.outbox WHERE published_at IS NULL AND id = eid;
    ASSERT seen = 1, 'unflushed event must be visible to the relay backlog scan';
END $$;

-- (3) The relay may stamp published_at exactly once (NULL → timestamp).
DO $$
DECLARE eid bigint;
BEGIN
    SELECT id INTO eid FROM coord.outbox WHERE published_at IS NULL LIMIT 1;
    UPDATE coord.outbox SET published_at = now() WHERE id = eid;   -- legal flush
    ASSERT (SELECT published_at FROM coord.outbox WHERE id = eid) IS NOT NULL,
           'relay flush (published_at NULL→timestamp) must be allowed';
END $$;

-- (4) published_at is set-once: re-stamping a flushed row is rejected (idempotent relay).
DO $$
DECLARE eid bigint; blocked boolean := false;
BEGIN
    SELECT id INTO eid FROM coord.outbox WHERE published_at IS NOT NULL LIMIT 1;
    BEGIN
        UPDATE coord.outbox SET published_at = now() + interval '1 hour' WHERE id = eid;
    EXCEPTION WHEN others THEN blocked := true;
    END;
    ASSERT blocked, 'coord.outbox.published_at must be set-once (re-stamp rejected)';
END $$;

-- (5) Event content is immutable: mutating any event column is rejected.
DO $$
DECLARE eid bigint; blocked boolean := false;
BEGIN
    SELECT id INTO eid FROM coord.outbox LIMIT 1;
    BEGIN
        UPDATE coord.outbox SET payload = '{"tampered":true}'::jsonb WHERE id = eid;
    EXCEPTION WHEN others THEN blocked := true;
    END;
    ASSERT blocked, 'coord.outbox event columns must be immutable (payload UPDATE rejected)';
END $$;

-- (6) DELETE is rejected (append-only, no event may be erased).
DO $$
DECLARE eid bigint; blocked boolean := false;
BEGIN
    SELECT id INTO eid FROM coord.outbox LIMIT 1;
    BEGIN
        DELETE FROM coord.outbox WHERE id = eid;
    EXCEPTION WHEN others THEN blocked := true;
    END;
    ASSERT blocked, 'DELETE on coord.outbox must be rejected (append-only)';
END $$;

-- (7) TRUNCATE is rejected (row triggers don't fire on TRUNCATE; statement guard closes it).
DO $$
DECLARE blocked boolean := false;
BEGIN
    BEGIN
        TRUNCATE coord.outbox;
    EXCEPTION WHEN others THEN blocked := true;
    END;
    ASSERT blocked, 'TRUNCATE on coord.outbox must be rejected';
END $$;

-- (8) The entity CHECK has teeth: a garbage entity (→ garbage NATS subject) is rejected.
DO $$
DECLARE blocked boolean := false;
BEGIN
    BEGIN
        INSERT INTO coord.outbox (entity, project_id, event_type, payload)
             VALUES ('bogus', gen_random_uuid(), 'x', '{}'::jsonb);
    EXCEPTION WHEN others THEN blocked := true;
    END;
    ASSERT blocked, 'coord.outbox.entity must reject values outside the taxonomy';
END $$;

ROLLBACK;
