-- 0008_scm_mirror.sql — untrusted-external SCM mirror (Arch §5.4 / ADR-018, Story 11.1 ISI-2254)
--
-- Forward-only, versioned migration (same discipline as 0001_coord_schema.sql). Applied exactly
-- once, in filename order (after 0007_reconcile_effects.sql), by the migration runner on startup.
-- There is NO down migration.
--
-- WHAT THIS IS (§5.4 / ADR-018): the durable mirror a `Project`'s upstream host (GitHub first,
-- behind the pkg/scm SourceControlProvider seam) is projected into by the repo-sync reconciler
-- (pkg/controller/reposync). It is a MIRROR, not the source of truth: the fenced coordination
-- record (§6) stays authoritative and custody NEVER crosses the seam.
--
-- TRUST CONTRACT (§7.3.2 / D8, story 11.1 AC6): every row carries `external_origin` provenance
-- (provider, repo, external id, actor) stamped NOT NULL, and a `trust_level` pinned to
-- 'untrusted-external' by CHECK. Mirror rows are never trusted control input: agents and the
-- console consume them through the same untrusted-provenance envelope as memory/discussion.
--
-- FIELD OWNERSHIP (OQ13): columns here are EXTERNAL-OWNED ONLY (title/state/actor/origin) —
-- written solely by the inbound reconciler. KSquad-owned linkage (issue⇄work-item map, Run
-- correlation) lands in LATER stories (11.2/11.3) as ksquad-owned columns written solely by the
-- coordination path. There are deliberately NO claim/lease/fence columns: this schema cannot
-- express coordination custody, so the mirror cannot take it (AC6 by construction).
--
-- IDEMPOTENCE (story 11.1 AC2): `mirror_record_pkey` is (project_namespace, project_name, kind,
-- external_id) and every write is an INSERT .. ON CONFLICT DO UPDATE — a redelivered webhook or
-- a no-change poll tick upserts the same bytes. The repo-sync reconciler writes through
-- pkg/scm.SQLMirrorStore; nothing else writes here.

CREATE SCHEMA IF NOT EXISTS scm;

-- scm.repo — one row per mirrored (Project, upstream repository). The reconciler
-- upserts it as the anchor for mirror liveness; `last_mirror_update` is the poll
-- fallback's own freshness observation.
CREATE TABLE IF NOT EXISTS scm.repo (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_name       VARCHAR(253) NOT NULL,
    project_namespace  VARCHAR(253) NOT NULL,
    url                VARCHAR(512) NOT NULL,
    provider           VARCHAR(50)  NOT NULL,          -- github (v1); gitlab/gitea drop in behind the seam
    mirror_enabled     BOOLEAN      NOT NULL DEFAULT true,
    last_mirror_update TIMESTAMPTZ,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT repo_unique UNIQUE (project_name, project_namespace, url)
);

-- scm.mirror_record — the normalized mirror the reconciler upserts into. One row
-- per (Project, kind, external id); the shape is the pkg/scm NormalizedRecord
-- common denominator so EVERY provider maps onto it identically (AC1).
CREATE TABLE IF NOT EXISTS scm.mirror_record (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_name       VARCHAR(253) NOT NULL,
    project_namespace  VARCHAR(253) NOT NULL,
    kind               VARCHAR(20)  NOT NULL,          -- issue | pr | check_run | artifact
    external_id        VARCHAR(64)  NOT NULL,
    state              VARCHAR(50)  NOT NULL,
    title              TEXT,
    actor              VARCHAR(255),
    external_origin    JSONB        NOT NULL,          -- {provider, repo, external_id, actor} (§7.3.2)
    trust_level        VARCHAR(20)  NOT NULL DEFAULT 'untrusted-external'
                       CHECK (trust_level IN ('untrusted-external', 'trusted-control')),
    payload            JSONB,                          -- normalized record body (body/url/labels/…)
    mirrored_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT mirror_record_pkey UNIQUE (project_name, project_namespace, kind, external_id)
);

-- Echo suppression is applied IN THE RECONCILER (pkg/scm.BuildMirrorRows drops
-- records authored by the origin-marked bot actor) — the schema has no need for
-- an inbound trigger, and none is wanted: suppression is loop semantics, not
-- row semantics (OQ13).

CREATE INDEX IF NOT EXISTS mirror_record_project_idx ON scm.mirror_record (project_namespace, project_name);
CREATE INDEX IF NOT EXISTS mirror_record_kind_idx    ON scm.mirror_record (kind);
CREATE INDEX IF NOT EXISTS mirror_record_origin_idx  ON scm.mirror_record USING GIN (external_origin);
CREATE INDEX IF NOT EXISTS mirror_record_state_idx   ON scm.mirror_record (state);
