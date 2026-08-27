-- 0013_scm_issue_link.sql — GitHub issues ⇄ KSquad work items sync (Arch §5.4/FR-H1, Story 11.2 ISI-2738)
--
-- Forward-only, versioned migration (same discipline as 0001..0012). Applied exactly
-- once, in filename order, by the apiserver migration runner (§12.3). There is NO down
-- migration — a mistake is corrected by a new forward migration.
--
-- WHAT THIS IS (§5.4 / FR-H1, story 11.2): the KSquad-owned issue⇄work-item LINKAGE and
-- per-link sync bookkeeping. 0008_scm_mirror.sql promised this exactly: "KSquad-owned
-- linkage (issue⇄work-item map, Run correlation) lands in LATER stories (11.2/11.3) as
-- ksquad-owned columns written solely by the coordination path". This is that table.
--
-- FIELD OWNERSHIP (OQ13 / 0008 discipline): scm.mirror_record stays EXTERNAL-owned,
-- written solely by the inbound reconciler; this table is KSQUAD-owned, written solely
-- by the coordination path (pkg/issuesync + the linkage API). The mirror row for an
-- issue and its link row meet ONLY inside the story-11.2 sync pass — no trigger crosses
-- the fence in either direction.
--
-- PROVENANCE TAGGING (story 11.2 AC2, ADR-001): `provenance` distinguishes who authored
-- the item FIRST — 'ksquad-native' (a work item created in KSquad and then linked to an
-- external issue) vs 'external-sourced' (a work item created FROM a provider issue).
-- The console reads this column to badge items; it is set once at link creation and
-- never rewritten by the sync loop (provenance is a fact about origin, not about the
-- last writer).
--
-- CONFLICT POLICY (story 11.2 AC3, §6.5): last-writer-wins WITH an audit row. The
-- winner is decided from the two observed write timestamps — external_updated_at (the
-- provider-side updated_at of the mirrored issue) vs ksquad_updated_at (the
-- coord.work_item.updated_at) — both rolling forward every sync pass. The decision
-- itself, winner and loser included, is recorded in coord.audit_log
-- (event_type='issue_sync') by pkg/issuesync in the SAME transaction as the apply; this
-- table carries only the rolling bookkeeping the next pass diffs against.
--
-- DIRECTION (story 11.2 AC1 "per configured direction"): the CRD owns the configured
-- direction (spec.repo.sync.issueSync.direction: inbound | bidirectional); this column
-- PINS the direction the link last synced under, so a config flip re-evaluates every
-- link on the next level-triggered pass instead of being silently retro-applied.

CREATE SCHEMA IF NOT EXISTS scm;

CREATE TABLE IF NOT EXISTS scm.issue_link (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_name       VARCHAR(253) NOT NULL,
    project_namespace  VARCHAR(253) NOT NULL,
    -- The fenced coordination record this link anchors (§6). CASCADE: deleting the
    -- work item deletes its linkage — the external issue simply stops being linked.
    work_item_id       UUID NOT NULL REFERENCES coord.work_item(id) ON DELETE CASCADE,
    provider           VARCHAR(50)  NOT NULL,          -- github (v1); behind the pkg/scm seam
    repo               VARCHAR(512) NOT NULL,
    external_id        VARCHAR(64)  NOT NULL,          -- provider's issue id (mirror_record key space)
    external_url       TEXT,
    -- inbound: provider → KSquad only (default). bidirectional: KSquad state changes
    -- reflect back through the provider seam (origin-marked for echo suppression).
    direction          VARCHAR(15)  NOT NULL DEFAULT 'inbound'
                       CHECK (direction IN ('inbound','bidirectional')),
    -- AC2 provenance: who authored the linked item first. Set at creation, immutable.
    provenance         VARCHAR(20)  NOT NULL
                       CHECK (provenance IN ('ksquad-native','external-sourced')),
    -- AC3 LWW bookkeeping: which side wrote last (rolling, updated by the sync pass).
    last_writer        VARCHAR(10)  NOT NULL DEFAULT 'external'
                       CHECK (last_writer IN ('external','ksquad')),
    -- Last observed/applied provider-side issue state + labels (the mirrored truth the
    -- work item reflects). external_state is the normalized open|closed projection.
    external_state     VARCHAR(20),
    external_labels    JSONB,
    external_updated_at TIMESTAMPTZ,
    -- Last observed/applied KSquad-side work_item.updated_at for this link.
    ksquad_updated_at  TIMESTAMPTZ,
    last_synced_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- One work item ↔ at most one external issue, and one external issue ↔ at most
    -- one work item: the linkage is a bijection per repo, enforced by the schema.
    CONSTRAINT issue_link_unique_external UNIQUE (provider, repo, external_id),
    CONSTRAINT issue_link_unique_work_item UNIQUE (work_item_id)
);

CREATE INDEX IF NOT EXISTS issue_link_project_idx ON scm.issue_link (project_namespace, project_name);
CREATE INDEX IF NOT EXISTS issue_link_work_item_idx ON scm.issue_link (work_item_id);
