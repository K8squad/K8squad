-- 0008_auth_user_admin.sql — Epic 15.2 user-admin columns on auth.user (ADR-033 / §12.3, ISI-2920)
--
-- Forward-only, versioned migration (same discipline as 0001/0006). Applied exactly once, in
-- filename order (after 0007_reconcile_effects.sql). There is NO down migration.
--
-- WHAT THIS IS: the schema delta Epic 15.2 (user CRUD + role assignment) needs over the ISI-2758
-- local-cred store. 0006 landed the read-path identity/session tables with an `is_admin` boolean;
-- story 15.2 pins the user record as
--     {id, username, email?, hashed_password, global_role ∈ {admin,user},
--      created_at, created_by, deactivated_at}
-- so this migration:
--   * adds `global_role` (admin|user) and derives it from the legacy `is_admin` boolean, then
--     DROPS `is_admin` — one source of truth, and the resolver reads `(global_role = 'admin')`;
--   * adds `email` (optional profile attribute — `username` stays the login key, per 15.2);
--   * adds `created_by` (the acting admin's principal — provenance for user mutations);
--   * adds `deactivated_at` (soft-delete stamp; DELETE /admin/users/{id} sets it and revokes
--     every live session — and the resolver joins `deactivated_at IS NULL` so a deactivated
--     user's cookie is dead even before revocation lands, fail-closed).
--
-- LOAD-BEARING INVARIANTS PRESERVED FROM 0006:
--   * Tenancy stays un-skippable: team_id remains NOT NULL. 15.2's create-user omits team from
--     the spec record; the API layer defaults a new user's team to the creating admin's team
--     (1:1 v1 model, 0006's note) so no unscoped identity is ever representable.
--   * The stable `principal` key is untouched — global_role changes never rewrite identity.
--   * Sessions keep storing only sha256(token); this migration touches only auth.user.
--
-- AUDIT (15.2 "all mutations emit to the audit trail"): admin user mutations append to the
-- existing append-only coord.audit_log (§6.5, ADR-040) with event_type user_created /
-- user_updated / user_deactivated, principal = the acting admin, and a jsonb payload naming the
-- target user. No new audit subsystem (R6/ponytail); the reject_mutation triggers already make
-- that log structurally append-only.
--
-- BOOTSTRAP (15.2): the initial admin is provisioned by the apiserver at start from
-- auth.bootstrap.adminUsername/adminPassword (chart value / env), ONLY when auth.user is empty
-- (idempotent: skipped if any user exists). This migration writes no rows and grants no login.

-- ---------------------------------------------------------------------------
-- global_role: the Epic-15 base-permission axis (admin = fleet-wide authority; user = base role,
-- permissions come from project memberships in 15.3). Derived from is_admin, then the boolean is
-- dropped so there is exactly one representation of "admin-ness" to keep in sync.
-- ---------------------------------------------------------------------------
ALTER TABLE auth.user ADD COLUMN global_role text NOT NULL DEFAULT 'user';
UPDATE auth.user SET global_role = 'admin' WHERE is_admin;
ALTER TABLE auth.user
    ADD CONSTRAINT user_global_role_check CHECK (global_role IN ('admin','user'));
ALTER TABLE auth.user DROP COLUMN is_admin;

-- ---------------------------------------------------------------------------
-- email — optional profile attribute (15.2: username is the login key; email is NOT an identity
-- key). Nullable, and UNIQUE-when-present so two accounts cannot claim one address.
-- ---------------------------------------------------------------------------
ALTER TABLE auth.user ADD COLUMN email text;
CREATE UNIQUE INDEX idx_user_email ON auth.user (email) WHERE email IS NOT NULL;

-- ---------------------------------------------------------------------------
-- created_by — the acting admin's principal at provisioning time (provenance, 15.2). Nullable:
-- the bootstrap admin creates itself (created_by IS NULL marks the install-time seed row).
-- ---------------------------------------------------------------------------
ALTER TABLE auth.user ADD COLUMN created_by text;

-- ---------------------------------------------------------------------------
-- deactivated_at — one-way soft-delete stamp (15.2 DELETE semantics). NULL = active. Login AND
-- session resolution both filter on it, so a deactivated account can neither authenticate nor
-- keep riding an existing cookie.
-- ---------------------------------------------------------------------------
ALTER TABLE auth.user ADD COLUMN deactivated_at timestamptz;
-- No extra index: username is already UNIQUE (0006) and the partial index on
-- (username WHERE deactivated_at IS NULL) served no query in the PR (PR #90 review).

-- Sessions of deactivated users must be dead NOW, not just at next login: the resolver's join
-- (internal/apiserver/session_resolver.go) filters u.deactivated_at IS NULL as of this migration.
