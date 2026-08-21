-- 0006_auth_schema_test.sql — runnable self-check for the auth/session store (ADR-033 / §12.3, ISI-2758)
--
-- No framework, no fixture: plain SQL that fails loudly if the identity/session invariants break. In CI
-- (db / migrations self-check) it runs against a throwaway Postgres AFTER THE FULL migration set is
-- applied (0001..000N in filename order), so it must assert the FINAL schema shape — e.g. auth.user
-- carries global_role (0008 derived it from 0006's is_admin boolean, then dropped the boolean):
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f db/migrations/*.sql \
--                                              -f db/migrations/0006_auth_schema_test.sql
--
-- Everything runs inside one transaction that is ROLLED BACK at the end, so the check leaves no residue
-- and can run repeatedly. Any failed assertion aborts with a non-zero exit (ON_ERROR_STOP). The read
-- PATH (fail-closed resolution: revoked/expired/miss -> no identity) is additionally exercised by the
-- apiserver integration test (internal/apiserver.PostgresSessionResolver) against a real Postgres — that
-- is a query-path property. This file proves the *structural* contract.

BEGIN;

-- Exactly the two tables exist in the auth schema.
DO $$
DECLARE n int;
BEGIN
    SELECT count(*) INTO n FROM information_schema.tables
     WHERE table_schema = 'auth' AND table_name IN ('user','session');
    ASSERT n = 2, format('expected auth.user + auth.session, found %s', n);
END $$;

-- NOT-NULL discipline: an unscoped or unattributed identity is un-representable.
DO $$
DECLARE nn text;
BEGIN
    -- user: username, principal, password_hash, team_id, global_role (0008 renamed
    -- 0006's is_admin), created_at all NOT NULL
    SELECT string_agg(column_name, ', ') INTO nn
      FROM information_schema.columns
     WHERE table_schema='auth' AND table_name='user'
       AND column_name IN ('username','principal','password_hash','team_id','global_role','created_at')
       AND is_nullable = 'YES';
    ASSERT nn IS NULL, format('auth.user column(s) must be NOT NULL: %s', nn);

    -- session: token_hash, user_id, created_at, expires_at NOT NULL; revoked_at is the ONLY nullable col.
    SELECT string_agg(column_name, ', ') INTO nn
      FROM information_schema.columns
     WHERE table_schema='auth' AND table_name='session'
       AND column_name IN ('token_hash','user_id','created_at','expires_at')
       AND is_nullable = 'YES';
    ASSERT nn IS NULL, format('auth.session column(s) must be NOT NULL: %s', nn);
END $$;

-- token_hash is bytea (a HASH, never the plaintext token) and length-checked to sha256's 32 bytes.
DO $$
DECLARE t text;
BEGIN
    SELECT data_type INTO t FROM information_schema.columns
     WHERE table_schema='auth' AND table_name='session' AND column_name='token_hash';
    ASSERT t = 'bytea', format('auth.session.token_hash must be bytea (sha256 digest), got %s', t);
END $$;

-- Seed one user + one live session so the structural guards below have real rows to bite on.
-- global_role (final schema): 0008 replaced 0006's is_admin boolean with this text enum.
INSERT INTO auth.user (id, username, principal, password_hash, team_id, global_role)
VALUES ('11111111-1111-1111-1111-111111111111', 'amelia', 'user:amelia',
        '$argon2id$v=19$m=65536,t=3,p=4$c29tZXNhbHQ$0123456789abcdef0123456789abcdef', -- dummy PHC (not a real hash)
        '22222222-2222-2222-2222-222222222222', 'user');

INSERT INTO auth.session (token_hash, user_id, expires_at)
VALUES (sha256('live-token'::bytea), '11111111-1111-1111-1111-111111111111', now() + interval '1 hour');

-- The 32-byte CHECK rejects a non-sha256 token_hash (structural: no short/garbage hash can be stored).
DO $$
BEGIN
    BEGIN
        INSERT INTO auth.session (token_hash, user_id, expires_at)
        VALUES ('\x1234'::bytea, '11111111-1111-1111-1111-111111111111', now() + interval '1 hour');
        ASSERT false, 'expected the octet_length(token_hash)=32 CHECK to reject a non-sha256 hash';
    EXCEPTION WHEN check_violation THEN
        -- expected
    END;
END $$;

-- token_hash is UNIQUE: the same session token cannot be double-inserted.
DO $$
BEGIN
    BEGIN
        INSERT INTO auth.session (token_hash, user_id, expires_at)
        VALUES (sha256('live-token'::bytea), '11111111-1111-1111-1111-111111111111', now() + interval '1 hour');
        ASSERT false, 'expected UNIQUE(token_hash) to reject a duplicate session token';
    EXCEPTION WHEN unique_violation THEN
        -- expected
    END;
END $$;

-- Fail-closed resolution shape: the resolver's exact predicate returns the identity for a live session
-- and NOTHING for a revoked or expired one. This mirrors PostgresSessionResolver.Resolve.
DO $$
DECLARE got text;
BEGIN
    -- live -> resolves to the principal + team.
    SELECT u.principal INTO got
      FROM auth.session s JOIN auth.user u ON u.id = s.user_id
     WHERE s.token_hash = sha256('live-token'::bytea)
       AND s.revoked_at IS NULL AND s.expires_at > now();
    ASSERT got = 'user:amelia', format('live session must resolve to user:amelia, got %s', got);

    -- revoked -> no row (one-way sign-out; A5 "old cookie -> 401").
    UPDATE auth.session SET revoked_at = now() WHERE token_hash = sha256('live-token'::bytea);
    SELECT u.principal INTO got
      FROM auth.session s JOIN auth.user u ON u.id = s.user_id
     WHERE s.token_hash = sha256('live-token'::bytea)
       AND s.revoked_at IS NULL AND s.expires_at > now();
    ASSERT got IS NULL, 'a revoked session must not resolve to an identity (fail-closed)';

    -- expired -> no row (time-boxed).
    INSERT INTO auth.session (token_hash, user_id, expires_at)
    VALUES (sha256('expired-token'::bytea), '11111111-1111-1111-1111-111111111111', now() - interval '1 second');
    SELECT u.principal INTO got
      FROM auth.session s JOIN auth.user u ON u.id = s.user_id
     WHERE s.token_hash = sha256('expired-token'::bytea)
       AND s.revoked_at IS NULL AND s.expires_at > now();
    ASSERT got IS NULL, 'an expired session must not resolve to an identity (fail-closed)';
END $$;

ROLLBACK;
