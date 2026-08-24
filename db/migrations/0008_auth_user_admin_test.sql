-- 0008_auth_user_admin_test.sql — runnable self-check for the Epic 15.2 user-admin columns
-- (ISI-2920). Same convention as 0006_auth_schema_test.sql: plain SQL that fails loudly if the
-- invariants break, inside one transaction that is ROLLED BACK at the end.
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f db/migrations/0006_auth_schema.sql \
--                                              -f db/migrations/0008_auth_user_admin.sql \
--                                              -f db/migrations/0008_auth_user_admin_test.sql
--
-- It proves the STRUCTURAL contract: global_role enum, is_admin gone, deactivated_at
-- fail-closedness on the resolver's join shape, email uniqueness, and the one-way soft-delete
-- semantics.

BEGIN;

-- is_admin is GONE; global_role exists with the two-value enum.
DO $$
DECLARE n int; t text;
BEGIN
    SELECT count(*) INTO n FROM information_schema.columns
     WHERE table_schema='auth' AND table_name='user' AND column_name='is_admin';
    ASSERT n = 0, 'auth.user.is_admin must be dropped (global_role is the one representation)';

    SELECT data_type INTO t FROM information_schema.columns
     WHERE table_schema='auth' AND table_name='user' AND column_name='global_role';
    ASSERT t = 'text', format('auth.user.global_role must be text, got %s', t);
END $$;

-- Seed rows: one admin (the shape a pre-0008 is_admin=true row derives to) and one user.
-- (The boolean→global_role derivation itself happens inside 0008 before is_admin is dropped;
-- it cannot be re-probed after the column is gone, so it is asserted here only via the value.)
INSERT INTO auth.user (id, username, principal, password_hash, team_id, global_role, email, created_by)
VALUES ('11111111-1111-1111-1111-111111111111', 'amelia', 'user:amelia', '$argon2id$v=19$m=65536,t=3,p=4$cw$0123456789abcdef', '22222222-2222-2222-2222-222222222222', 'admin', 'amelia@example.invalid', NULL),
       ('33333333-3333-3333-3333-333333333333', 'bob', 'user:bob', '$argon2id$v=19$m=65536,t=3,p=4$cw$0123456789abcdef', '22222222-2222-2222-2222-222222222222', 'user', NULL, 'user:amelia');

-- The enum CHECK rejects anything but admin|user.
DO $$
BEGIN
    BEGIN
        UPDATE auth.user SET global_role = 'superuser' WHERE principal = 'user:bob';
        ASSERT false, 'expected the global_role CHECK to reject a non-enum value';
    EXCEPTION WHEN check_violation THEN
        -- expected
    END;
END $$;

-- email is optional but unique-when-present.
DO $$
BEGIN
    BEGIN
        UPDATE auth.user SET email = 'amelia@example.invalid' WHERE principal = 'user:bob';
        ASSERT false, 'expected UNIQUE(email) to reject a duplicate address';
    EXCEPTION WHEN unique_violation THEN
        -- expected
    END;
END $$;

-- Soft-delete is one-way fail-closed on the resolver's join shape: a live session of a
-- deactivated user resolves to NOTHING (deactivated_at IS NULL filter), while an active
-- user's live session still resolves.
INSERT INTO auth.session (token_hash, user_id, expires_at)
VALUES (sha256('bob-live'::bytea), '33333333-3333-3333-3333-333333333333', now() + interval '1 hour');

DO $$
DECLARE got text;
BEGIN
    SELECT u.principal INTO got
      FROM auth.session s JOIN auth.user u ON u.id = s.user_id
     WHERE s.token_hash = sha256('bob-live'::bytea)
       AND s.revoked_at IS NULL AND s.expires_at > now()
       AND u.deactivated_at IS NULL;
    ASSERT got = 'user:bob', 'an active user''s live session must resolve';

    UPDATE auth.user SET deactivated_at = now() WHERE principal = 'user:bob';

    SELECT u.principal INTO got
      FROM auth.session s JOIN auth.user u ON u.id = s.user_id
     WHERE s.token_hash = sha256('bob-live'::bytea)
       AND s.revoked_at IS NULL AND s.expires_at > now()
       AND u.deactivated_at IS NULL;
    ASSERT got IS NULL, 'a deactivated user''s live session must NOT resolve (fail-closed)';
END $$;

-- Tenancy stays un-skippable: team_id rejects NULL even through the new admin path.
DO $$
BEGIN
    BEGIN
        UPDATE auth.user SET team_id = NULL WHERE principal = 'user:bob';
        ASSERT false, 'expected NOT NULL(team_id) to hold (tenancy un-skippable)';
    EXCEPTION WHEN not_null_violation THEN
        -- expected
    END;
END $$;

ROLLBACK;
