-- 0022_coord_outbox_trace_carrier_test.sql — runnable self-check for the outbox
-- trace-carrier column (ISI-4440). Plain SQL, no framework; run after the
-- migrations it checks, inside one transaction ROLLED BACK at the end:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 \
--          -f db/migrations/0001_coord_schema.sql \
--          -f db/migrations/0003_coord_outbox.sql \
--          -f db/migrations/0022_coord_outbox_trace_carrier.sql \
--          -f db/migrations/0022_coord_outbox_trace_carrier_test.sql
--
-- It proves the STRUCTURAL ACs: the column exists as nullable jsonb, a capture
-- may store or omit the carrier, and the immutability guard now (a) rejects a
-- content edit to trace_carrier while (b) still allowing the one legal write,
-- published_at NULL → timestamp. The Go plumbing (capture inject, relay extract,
-- producer/consumer spans) is exercised by pkg/events + pkg/events/jetstream.

BEGIN;

-- A project_id is needed for the NOT NULL subject component. The column is named
-- `proj` (not `pid`) so `SELECT proj INTO pid` below is unambiguous — a column
-- named `pid` would collide with the PL/pgSQL variable of the same name.
CREATE TEMP TABLE t_ctx AS SELECT gen_random_uuid() AS proj;

-- (1) The column exists, is jsonb, and is NULLABLE (no backfill / no default).
DO $$
DECLARE nullable text; dtype text;
BEGIN
    SELECT is_nullable, data_type INTO nullable, dtype
      FROM information_schema.columns
     WHERE table_schema = 'coord' AND table_name = 'outbox'
       AND column_name = 'trace_carrier';
    ASSERT nullable = 'YES', 'trace_carrier must be nullable (a row with no active span carries none)';
    ASSERT dtype = 'jsonb', 'trace_carrier must be jsonb (a W3C carrier map)';
END $$;

-- (2) A capture may omit the carrier (NULL) or store it; both persist verbatim.
DO $$
DECLARE pid uuid; withc bigint; withoutc bigint; tc jsonb;
BEGIN
    SELECT proj INTO pid FROM t_ctx;

    INSERT INTO coord.outbox (entity, project_id, event_type, payload, trace_carrier)
         VALUES ('run', pid, 'started', '{}'::jsonb,
                 '{"traceparent":"00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"}'::jsonb)
      RETURNING id INTO withc;
    SELECT trace_carrier INTO tc FROM coord.outbox WHERE id = withc;
    ASSERT tc ->> 'traceparent' = '00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01',
        'a stored carrier must persist the traceparent verbatim';

    INSERT INTO coord.outbox (entity, project_id, event_type, payload)
         VALUES ('run', pid, 'ended', '{}'::jsonb)
      RETURNING id INTO withoutc;
    SELECT trace_carrier INTO tc FROM coord.outbox WHERE id = withoutc;
    ASSERT tc IS NULL, 'a capture with no carrier must leave trace_carrier NULL';
END $$;

-- (3) The immutability guard rejects a content edit to trace_carrier...
DO $$
DECLARE pid uuid; rid bigint; got text;
BEGIN
    SELECT proj INTO pid FROM t_ctx;
    INSERT INTO coord.outbox (entity, project_id, event_type, payload, trace_carrier)
         VALUES ('run', pid, 'started', '{}'::jsonb, '{"traceparent":"tp-a"}'::jsonb)
      RETURNING id INTO rid;
    BEGIN
        UPDATE coord.outbox SET trace_carrier = '{"traceparent":"tp-b"}'::jsonb WHERE id = rid;
        ASSERT false, 'editing trace_carrier must be rejected by the immutability guard';
    EXCEPTION WHEN restrict_violation THEN
        NULL; -- expected
    END;
END $$;

-- (4) ...while the one legal write, published_at NULL → timestamp, still succeeds.
DO $$
DECLARE pid uuid; rid bigint; pub timestamptz;
BEGIN
    SELECT proj INTO pid FROM t_ctx;
    INSERT INTO coord.outbox (entity, project_id, event_type, payload, trace_carrier)
         VALUES ('run', pid, 'started', '{}'::jsonb, '{"traceparent":"tp-a"}'::jsonb)
      RETURNING id INTO rid;
    UPDATE coord.outbox SET published_at = now() WHERE id = rid AND published_at IS NULL;
    SELECT published_at INTO pub FROM coord.outbox WHERE id = rid;
    ASSERT pub IS NOT NULL, 'the relay flush (published_at NULL → now) must still be allowed';
END $$;

ROLLBACK;
