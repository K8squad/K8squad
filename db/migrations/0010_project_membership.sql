-- 0010_project_membership.sql — Epic 15.3 per-Project RBAC memberships (ADR-035, ISI-2921)
--
-- Forward-only, versioned migration (same discipline as 0001/0006/0008). Applied exactly once,
-- in filename order, by the CI/chart migration apply (psql db/migrations/*.sql). There is NO
-- down migration.
--
-- WHAT THIS IS: the durable store 0008 promised. 0008 pinned the base-permission axis
-- global_role ∈ {admin,user} on auth.user and noted, verbatim, "user = base role, permissions
-- come from project memberships in 15.3". This is that table: the (user, Project, role) grants
-- that turn a base `user` into an authorized caller on a specific Project. `admin` needs NO row
-- here — global_role=admin is fleet-wide authority and short-circuits the RBAC check before any
-- membership lookup (internal/apiserver/rbac.go).
--
-- DOMAIN MODEL (why `project` is text, not a FK): a Project is a first-class KSquad CRD
-- (ksquadv1.Project) that lives inside the caller's Team namespace and is addressed by its
-- metadata.name — the SAME string the dashboard route resolves (GET
-- /api/projects/{projectId}/dashboard → project.Name within the Team namespace, dashboard.go).
-- Projects are not rows in this database, so a membership references a Project by that stable name.
-- Tenancy stays rooted in the Team: a membership is only ever consulted for a caller whose resolved
-- AuthorContext.TeamID already scopes them to the namespace the Project lives in, so a bare
-- (user, project-name) pair cannot cross a Team boundary — the resolver never sees a foreign Team's
-- caller (fail-closed 404 existence-hiding happens upstream at the choke point / dashboard).
--
-- ROLE VOCABULARY (ADR-035 three-tier, mirrored in pkg/auth): viewer < contributor < maintainer.
-- The CHECK pins the enum in the store of record so an out-of-band write cannot inject a role the
-- middleware's rank map does not know (pkg/auth.RoleAtLeast would treat an unknown role as rank 0,
-- i.e. deny — but the DB refusing it is the stronger, structural guarantee).
--
-- LOAD-BEARING INVARIANTS:
--   * One role per (user, Project) — the UNIQUE(user_id, project) constraint makes a user's
--     authority on a Project single-valued and deterministic (no "which of two rows wins?").
--     The 15.9 OIDC sync (GroupMapping.Resolve already collapses duplicate projects to the
--     strongest role) upserts against this constraint; a human admin grant does the same.
--   * Cascade on user delete — ON DELETE CASCADE ties a membership's lifetime to its user, so
--     deactivating/removing a user cannot leave an orphaned grant that outlives the identity.
--   * Attribution — created_by carries the acting admin's principal (provenance for grants), NULL
--     only for the 15.9 IdP-synced rows (no human actor).
--
-- AUDIT (15.3 "grant/revoke emit to the audit trail"): membership mutations append to the existing
-- append-only coord.audit_log (§6.5, ADR-040) with event_type membership_granted /
-- membership_revoked, principal = the acting admin, payload naming {user, project, role}. No new
-- audit subsystem — the reject_mutation triggers already make that log structurally append-only.

-- ---------------------------------------------------------------------------
-- auth.project_membership — one (user, Project, role) grant (ADR-035 / 15.3).
-- ---------------------------------------------------------------------------
CREATE TABLE auth.project_membership (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid        NOT NULL REFERENCES auth.user(id) ON DELETE CASCADE,
    project    text        NOT NULL,                    -- Project CRD name, scoped by the user's Team namespace
    role       text        NOT NULL,                    -- viewer|contributor|maintainer (ADR-035)
    created_at timestamptz NOT NULL DEFAULT now(),
    created_by text            NULL,                    -- acting admin's principal; NULL = 15.9 IdP sync (no human actor)
    CONSTRAINT project_membership_role_check
        CHECK (role IN ('viewer','contributor','maintainer')),
    -- One role per user per Project: upsert target for both the human grant path and the 15.9
    -- OIDC sync (which has already collapsed duplicates to the strongest role).
    CONSTRAINT project_membership_user_project_uniq UNIQUE (user_id, project)
);

-- Enforcement hot path: the RBAC middleware looks a caller's role up by (principal → user_id, project).
-- The principal → user_id hop rides auth.user's existing UNIQUE(principal); this index keeps the
-- per-user membership fan-out (and the 8.15 "who can see this Project" review) cheap.
CREATE INDEX idx_project_membership_user ON auth.project_membership (user_id);
-- Reverse lookup ("all members of a Project", 8.15 review surface) scans by project name.
CREATE INDEX idx_project_membership_project ON auth.project_membership (project);
