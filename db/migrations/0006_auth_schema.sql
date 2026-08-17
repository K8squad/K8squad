-- 0006_auth_schema.sql — local-cred identity + session store (Arch §12.3 / ADR-033, ISI-2758)
--
-- Forward-only, versioned migration (same discipline as 0001_coord_schema.sql). Applied exactly
-- once, in filename order (after 0005_reconcile_step.sql), by the migration runner on startup.
-- There is NO down migration.
--
-- WHAT THIS IS (ADR-033 / §12.3): the backing store that turns the opaque HttpOnly `ksquad_session`
-- cookie the console BFF forwards (console/lib/bff.ts) into a server-derived identity + Team scope.
-- ADR-033 pinned the v1 console authN mechanism to a LOCAL username/password store (argon2id) with
-- OIDC/SSO an opt-in fast-follow behind the AuthProvider seam. This migration lands the two durable
-- tables that store of record needs:
--   * auth.user     — the local-cred principal (stable identity, Team scope, admin flag, credential).
--   * auth.session  — an opaque, time-boxed, revocable session bound to a user.
--
-- IDENTITY CONTRACT (the crux — why this issue is the true E2E blocker for ISI-2750):
--   The apiserver's §13 choke point resolves `ksquad_session` through a pluggable SessionResolver
--   (internal/apiserver/auth.go). Its production implementation — internal/apiserver.PostgresSessionResolver
--   — reads THIS schema:
--       SELECT u.principal, u.team_id, u.is_admin
--         FROM auth.session s JOIN auth.user u ON u.id = s.user_id
--        WHERE s.token_hash = sha256($cookie) AND s.revoked_at IS NULL AND s.expires_at > now();
--   No row ⇒ ErrNoSession ⇒ 401 (fail-closed). Until this migration is applied the resolver's query
--   errors and every gated route stays 401 — deny-by-default, never fail-open.
--
-- SCOPE BOUNDARY (this issue vs Epic 15): ISI-2758 owns the SCHEMA + the read-path resolver. The
-- write path — argon2id login (A1), password reset (A5), session mint/revoke — is the auth/console
-- track's Epic-15 source (pkg/auth, §12.3; contract pinned in pkg/auth/a5_authsession_contract_test.go).
-- The columns here are shaped so that flow drops in without a schema change: `password_hash` holds the
-- argon2id PHC string login writes; `auth.session.revoked_at` is the one-way stamp A5 sets to sign a
-- session out ("old cookie -> 401 after reset"). This migration writes NO rows and grants NO login.
--
-- THE LOAD-BEARING INVARIANTS THIS STRUCTURE ENFORCES (not application discipline alone):
--   * Sessions store only a HASH, never the bearer token (NFR-SEC) — `token_hash` is sha256(cookie
--     value); the plaintext session token is NEVER persisted, so a DB dump yields no usable cookie.
--   * Tenancy is un-skippable (§7.3.3) — `team_id`, `principal` are NOT NULL on auth.user, so an
--     unscoped or unattributed identity is un-representable; every resolved AuthorContext carries a Team.
--   * Fail-closed by construction — resolution filters `revoked_at IS NULL AND expires_at > now()`, so
--     an expired or revoked session cannot resolve to an identity.
--
-- NOTE on retention (why sessions are NOT append-only, unlike discussion.message): sessions are
-- operational state, not an audit record. Expired rows are pruned by a janitor (DELETE ... WHERE
-- expires_at < now()); an append-only trigger would make that unprunable. Revocation is the one-way
-- `revoked_at` stamp, not a delete. The coordination/discussion audit immutability guarantees do NOT
-- apply here.

CREATE SCHEMA IF NOT EXISTS auth;

-- gen_random_uuid() is a core function since PG13 (CNPG ships >=13) — no pgcrypto extension needed,
-- so the migration runs under a least-privilege app role without CREATE EXTENSION rights (mirrors 0001).

-- ---------------------------------------------------------------------------
-- auth.user — a local-cred principal (§12.3 / ADR-033)
--
-- One row per human identity that can log into the console. `principal` is the STABLE identity string
-- the write path stamps as author_principal / created_by across the platform (discussion §7.5, memory
-- §7.2) — it is what the resolved AuthorContext.Principal carries, so it must never change under a row
-- (username may be display-facing; principal is the durable key). `team_id` is the caller's authorized
-- Team scope (tenancy root, §7.3.3 — 1:1 with squad/namespace). `password_hash` is the argon2id PHC
-- string ("$argon2id$...") the Epic-15 login/reset path writes and verifies; this migration never
-- populates it.
--
-- v1 models one Team per user (team_id lives here, 1:1). A future multi-Team model moves the scope
-- selection onto auth.session (a session is scoped to exactly one Team even if the user has many); the
-- resolver already selects Team through the session join, so that evolution is a forward migration that
-- adds session.team_id without touching the resolver's shape.
-- ---------------------------------------------------------------------------
CREATE TABLE auth.user (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    username      text        NOT NULL UNIQUE,             -- login handle (display-facing; may change)
    principal     text        NOT NULL UNIQUE,             -- STABLE identity key stamped as author_principal
    password_hash text        NOT NULL,                    -- argon2id PHC string — WRITTEN by Epic-15 login/reset
    team_id       uuid        NOT NULL,                    -- authorized Team scope (tenancy root, §7.3.3)
    is_admin      boolean     NOT NULL DEFAULT false,      -- author-or-admin surface (may retract others' msgs)
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- auth.session — an opaque, time-boxed, revocable session (§12.3 / ADR-033)
--
-- The value of the HttpOnly `ksquad_session` cookie is a high-entropy random token minted at login and
-- delivered ONLY to the browser. We persist sha256(token), NEVER the token itself: a high-entropy token
-- needs no slow KDF (unlike a password), and hashing means a DB read cannot replay a live session.
--
-- Resolution (PostgresSessionResolver) is fail-closed: a session resolves ONLY while
-- `revoked_at IS NULL AND expires_at > now()`. `revoked_at` is the one-way sign-out stamp the A5 reset
-- flow sets to invalidate every live session for a principal ("old cookie -> 401" — the A5 contract).
-- ---------------------------------------------------------------------------
CREATE TABLE auth.session (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash bytea       NOT NULL UNIQUE,                -- sha256(ksquad_session value); NEVER the plaintext token
    user_id    uuid        NOT NULL REFERENCES auth.user(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,                       -- fail-closed after this instant (time-boxed)
    revoked_at timestamptz     NULL,                       -- one-way sign-out (A5 reset / logout); NULL = live
    CONSTRAINT session_token_hash_len CHECK (octet_length(token_hash) = 32)  -- sha256 is exactly 32 bytes
);

-- Resolution fast path: the resolver looks a session up by token_hash; the partial predicate keeps the
-- hot index to live sessions.
CREATE INDEX idx_session_live ON auth.session (token_hash) WHERE revoked_at IS NULL;
-- Revoke-all-for-principal (A5) and janitor prune (expires_at < now()) both scan by user.
CREATE INDEX idx_session_user ON auth.session (user_id);
