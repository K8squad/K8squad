-- 0010_project_membership_test.sql — runnable self-check for Epic 15.3 memberships (ADR-035, ISI-2921)
--
-- No framework, no fixture: plain SQL that fails loudly if the RBAC-store invariants break. In CI
-- (db / migrations self-check) it runs against a throwaway Postgres AFTER THE FULL migration set is
-- applied (0001..000N in filename order):
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f db/migrations/*.sql \
--                                              -f db/migrations/0010_project_membership_test.sql
--
-- Everything runs inside one transaction that is ROLLED BACK at the end, so the check leaves no
-- residue. Any failed assertion aborts with a non-zero exit (ON_ERROR_STOP). This file proves the
-- *structural* contract; the enforcement/read path (rank comparison, fail-closed on no membership)
-- is exercised by the Go unit tests (pkg/auth.TestRoleAtLeast, internal/apiserver rbac tests).

BEGIN;

-- The table exists in the auth schema.
DO $$
DECLARE n int;
BEGIN
    SELECT count(*) INTO n FROM information_schema.tables
     WHERE table_schema = 'auth' AND table_name = 'project_membership';
    ASSERT n = 1, format('expected auth.project_membership, found %s', n);
END $$;

-- NOT-NULL discipline: an unscoped or unattributed grant is un-representable
-- (user_id, project, role, created_at all NOT NULL; created_by is nullable by design).
DO $$
DECLARE nn text;
BEGIN
    SELECT string_agg(column_name, ', ') INTO nn
      FROM information_schema.columns
     WHERE table_schema='auth' AND table_name='project_membership'
       AND column_name IN ('user_id','project','role','created_at')
       AND is_nullable = 'YES';
    ASSERT nn IS NULL, format('auth.project_membership column(s) must be NOT NULL: %s', nn);
END $$;

-- Seed one user to hang memberships off (the FK target).
INSERT INTO auth.user (id, username, principal, password_hash, team_id, global_role)
VALUES ('11111111-1111-1111-1111-111111111111', 'alice', 'alice',
        '$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA', gen_random_uuid(), 'user');

-- A valid grant inserts.
INSERT INTO auth.project_membership (user_id, project, role, created_by)
VALUES ('11111111-1111-1111-1111-111111111111', 'checkout', 'contributor', 'admin');

-- CHECK enforces the ADR-035 role vocabulary: an unknown role is refused by the store of record.
DO $$
BEGIN
    BEGIN
        INSERT INTO auth.project_membership (user_id, project, role)
        VALUES ('11111111-1111-1111-1111-111111111111', 'billing', 'owner');
        ASSERT false, 'expected role CHECK to reject "owner"';
    EXCEPTION WHEN check_violation THEN
        -- expected
    END;
END $$;

-- UNIQUE(user_id, project): one role per user per Project (no ambiguous double-grant).
DO $$
BEGIN
    BEGIN
        INSERT INTO auth.project_membership (user_id, project, role)
        VALUES ('11111111-1111-1111-1111-111111111111', 'checkout', 'maintainer');
        ASSERT false, 'expected UNIQUE(user_id, project) to reject a second role on the same Project';
    EXCEPTION WHEN unique_violation THEN
        -- expected
    END;
END $$;

-- ON DELETE CASCADE: removing the user removes the grant (no orphaned authority).
DELETE FROM auth.user WHERE id = '11111111-1111-1111-1111-111111111111';
DO $$
DECLARE n int;
BEGIN
    SELECT count(*) INTO n FROM auth.project_membership
     WHERE user_id = '11111111-1111-1111-1111-111111111111';
    ASSERT n = 0, format('expected cascade delete to remove memberships, found %s', n);
END $$;

ROLLBACK;
