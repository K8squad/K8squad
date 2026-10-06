-- 0032_decision_read_marker_test.sql — runnable self-check for ISI-5535 (E1 of ISI-5531, ADR-0026 §6).
--
-- Same no-framework discipline as the sibling companions: plain SQL that fails loudly if the
-- coord.decision_read_marker surface or its key/upsert contracts break. Runs AFTER the full migration
-- set, inside one transaction ROLLED BACK at the end:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f db/migrations/*.sql \
--                                              -f db/migrations/0032_decision_read_marker_test.sql

BEGIN;

-- (1) The table exists with the expected columns/types and a now() default on seen_at.
DO $$
DECLARE t text; d text;
BEGIN
    SELECT data_type INTO t FROM information_schema.columns
     WHERE table_schema='coord' AND table_name='decision_read_marker' AND column_name='user_principal';
    ASSERT t = 'text', format('expected coord.decision_read_marker.user_principal text, found %s', coalesce(t,'<absent>'));

    SELECT data_type INTO t FROM information_schema.columns
     WHERE table_schema='coord' AND table_name='decision_read_marker' AND column_name='item_key';
    ASSERT t = 'text', format('expected coord.decision_read_marker.item_key text, found %s', coalesce(t,'<absent>'));

    SELECT data_type, column_default INTO t, d FROM information_schema.columns
     WHERE table_schema='coord' AND table_name='decision_read_marker' AND column_name='seen_at';
    ASSERT t = 'timestamp with time zone', format('expected seen_at timestamptz, found %s', coalesce(t,'<absent>'));
    ASSERT d LIKE 'now()%', format('expected seen_at default now(), found %s', coalesce(d,'<null>'));
END $$;

-- (2) (user_principal, item_key) is the primary key — a second seen of the same item by the same
--     user UPSERTs the one row (idempotent mark-seen), never a duplicate.
DO $$
DECLARE n integer; t1 timestamptz; t2 timestamptz;
BEGIN
    INSERT INTO coord.decision_read_marker (user_principal, item_key, seen_at)
    VALUES ('alice', 'inReview:11111111-1111-1111-1111-111111111111', '2026-10-06T10:00:00Z');
    SELECT seen_at INTO t1 FROM coord.decision_read_marker
     WHERE user_principal='alice' AND item_key='inReview:11111111-1111-1111-1111-111111111111';

    INSERT INTO coord.decision_read_marker (user_principal, item_key, seen_at)
    VALUES ('alice', 'inReview:11111111-1111-1111-1111-111111111111', '2026-10-06T11:00:00Z')
    ON CONFLICT (user_principal, item_key) DO UPDATE SET seen_at = EXCLUDED.seen_at;

    SELECT count(*), seen_at INTO n, t2 FROM coord.decision_read_marker
     WHERE user_principal='alice' AND item_key='inReview:11111111-1111-1111-1111-111111111111'
     GROUP BY seen_at;
    ASSERT n = 1, format('expected exactly one marker row after re-seen, found %s', n);
    ASSERT t2 > t1, 'expected the upsert to advance seen_at';
END $$;

-- (3) Markers are strictly per-user: the same item_key for a different principal is a distinct row,
--     so one human marking a shared in_review item seen never clears it for a teammate.
DO $$
DECLARE n integer;
BEGIN
    INSERT INTO coord.decision_read_marker (user_principal, item_key)
    VALUES ('bob', 'inReview:11111111-1111-1111-1111-111111111111');
    SELECT count(*) INTO n FROM coord.decision_read_marker
     WHERE item_key='inReview:11111111-1111-1111-1111-111111111111';
    ASSERT n = 2, format('expected two per-user markers for the shared item, found %s', n);
END $$;

ROLLBACK;
